// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// defaultFollowupS is the follow-up window of a type without followup_window_s (CNT-5).
const defaultFollowupS = 1800

// followupWindow is the follow-up window a completed result opens: the type declares
// need_input as a field code reads, and the result sets it to true (CNT-5).
func followupWindow(tt manifest.TaskType, result map[string]any) time.Duration {
	if v, _ := result["need_input"].(bool); !v {
		return 0
	}
	props, _ := tt.ResultSchema["properties"].(map[string]any)
	field, _ := props["need_input"].(map[string]any)
	if field["x-botaru-read-by"] != "code" {
		return 0
	}
	s := defaultFollowupS
	if tt.FollowupWindowS != nil {
		s = *tt.FollowupWindowS
	}
	return time.Duration(s) * time.Second
}

// openFollowup opens the follow-up window of a completed task for its conversation and
// initiator and returns its length, or 0 when the result opens none (CNT-5).
func (c *Core) openFollowup(ctx context.Context, tx pgx.Tx, now time.Time, task string, tt manifest.TaskType, result map[string]any) (time.Duration, error) {
	d := followupWindow(tt, result)
	if d <= 0 {
		return 0, nil
	}
	_, err := tx.Exec(ctx, `UPDATE tasks SET record = jsonb_set(record, '{followup_until}', to_jsonb($2::text)) WHERE id = $1`, task, fmtTime(now.Add(d)))
	return d, err
}

// closeFollowups closes the open windows of a conversation and initiator: a channel command
// closes them (CNT-5).
func closeFollowups(ctx context.Context, tx pgx.Tx, tenant, conversation, initiator string) error {
	_, err := tx.Exec(ctx, `UPDATE tasks SET record = record - 'followup_until'
		WHERE tenant = $1 AND initiator = $2 AND record->>'conversation' = $3 AND record ? 'followup_until'`, tenant, initiator, conversation)
	return err
}

// earlier is the task a person's message continues, with what the new task takes from it.
type earlier struct {
	ID, Type  string
	Untrusted bool
	// Mark is the data mark of the earlier task: the new one is not lower (CNT-3).
	Mark string
	// Data is the outcome of the earlier task, the attachment block of the new one.
	Data map[string]any
	// Tree is set when the earlier task is a child or a continuation: the new one joins its
	// tree under the same parent, so the tree limits hold for it too (PRM-6).
	Tree *treeRecord
}

// continuesTo finds the task an activity continues (CNT-5, PRM-9): the task of the bot message
// the person replied to, if it is of the same conversation and the person is its initiator or
// may read it; else, for a message without a command, the task whose follow-up window is open
// for this conversation and initiator, which the message then closes.
func (c *Core) continuesTo(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, a Activity, person string, groups []string, command bool) (*earlier, error) {
	conversation := "c:" + a.Channel + ":" + a.Chat.ID()
	var id string
	if a.ReplyTo != "" {
		err := tx.QueryRow(ctx, `SELECT task FROM deliveries WHERE tenant = $1 AND exec_kind = 'channel' AND exec_name = $2 AND target_id = $3 AND external_id = $4 AND task IS NOT NULL
			ORDER BY seq DESC LIMIT 1`, tv.Name, a.Channel, a.Chat.ID(), a.ReplyTo).Scan(&id)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}
	window := false
	if id == "" {
		if command {
			return nil, closeFollowups(ctx, tx, tv.Name, conversation, person)
		}
		err := tx.QueryRow(ctx, `SELECT id FROM tasks WHERE tenant = $1 AND initiator = $2 AND record->>'conversation' = $3
			AND (record->>'followup_until')::timestamptz > $4 ORDER BY created_at DESC LIMIT 1`, tv.Name, person, conversation, now).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		window = true
	}
	var typ, initiator, state string
	var rec, result []byte
	if err := tx.QueryRow(ctx, `SELECT type, initiator, state, record, result FROM tasks WHERE id = $1 AND tenant = $2`, id, tv.Name).
		Scan(&typ, &initiator, &state, &rec, &result); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	var r struct {
		Conversation  string `json:"conversation"`
		DataMark      string `json:"data_mark"`
		UntrustedSeen bool   `json:"untrusted_seen"`
	}
	if err := json.Unmarshal(rec, &r); err != nil {
		return nil, err
	}
	// A group conversation is kept per person (MEM-1 in this build): a reply in the group
	// continues a task of any person of that group chat.
	if r.Conversation != conversation && !(a.Chat.Kind == "group" && strings.HasPrefix(r.Conversation, conversation+":")) {
		return nil, nil
	}
	// The person reads the earlier outcome only with its data class, as initiator too.
	if !hasData(tv, groups, r.DataMark) || (initiator != person && !hasCap(tv, groups, "task.read:"+typ)) {
		return nil, nil
	}
	if window {
		if err := closeFollowups(ctx, tx, tv.Name, conversation, person); err != nil {
			return nil, err
		}
	}
	out := &earlier{ID: id, Type: typ, Untrusted: r.UntrustedSeen, Mark: r.DataMark, Data: map[string]any{"task": id, "state": state}}
	if tr, err := loadTree(ctx, tx, id); err != nil {
		return nil, err
	} else if tr.Parent != "" {
		out.Tree = &tr
	}
	if len(result) > 0 {
		var res map[string]any
		if json.Unmarshal(result, &res) == nil && len(res) > 0 && len(res) <= maxResultFields {
			out.Data["result"] = res
		}
	}
	return out, nil
}
