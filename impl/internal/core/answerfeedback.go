// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	// taskAnswer is the optional "Useful" / "Not needed" card under a bot's answer to a person
	// (INS-7).
	taskAnswer = "task_answer"
	// answerFeedbackDays is the period of the answer feedback an analysis reads.
	answerFeedbackDays = 30
	// maxAnswerFeedbackTypes bounds the task types of that input.
	maxAnswerFeedbackTypes = 20
)

// answerDelivered plans, once a bot's answer to a work task went out to the person who asked in
// a direct chat, a card with the buttons "Useful" and "Not needed" when the tenant turns it on
// with attention.answer_feedback (INS-7). Planned only after the answer was delivered, the card
// never goes ahead of an answer that waits for a notice, nor goes for an answer a hold or a
// mark withheld.
func (c *Core) answerDelivered(ctx context.Context, tx pgx.Tx, now time.Time, tenant, delivery string) error {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	tv := cat.Tenants[tenant]
	if tv == nil || !tv.Spec.Attention.AnswerFeedback {
		return nil
	}
	var task, recipient, target, channel, typ string
	err = tx.QueryRow(ctx, `SELECT d.task, d.recipient, d.target_id, d.exec_name, t.type FROM deliveries d JOIN tasks t ON t.id = d.task
		WHERE d.id = $1 AND d.dataorigin = 'model' AND d.body->>'form' = 'result' AND d.recipient = t.initiator AND t.state = 'completed'
			AND t.record->'origin'->'chat'->>'kind' = 'direct'`,
		delivery).Scan(&task, &recipient, &target, &channel, &typ)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	external, direct := strings.CutPrefix(target, "direct:")
	if !direct || purposeOf(cat.TaskTypes[typ]) != "work" {
		return nil
	}
	return c.answerFeedback(ctx, tx, now, tv, task, recipient, channel, external)
}

// answerFeedback plans the card of answerDelivered to an active person. The cards go only into
// the room the person's daily card limit has left: decision cards and manager notices count
// first and never wait for an answer card, and an answer card is left out, never deferred, once
// those and the answer cards of the day reach noncritical_per_day. Pressing it is optional and
// changes nothing but the share of useful answers an analysis reads.
func (c *Core) answerFeedback(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, task, person, channel, external string) error {
	var human bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM principals WHERE tenant = $1 AND id = $2 AND kind = 'human' AND state = 'active')`,
		tv.Name, person).Scan(&human); err != nil || !human {
		return err
	}
	// One count per person at a time, as at the hand-out of a card (APR-15).
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 7300107))`, tv.Name+"/"+person); err != nil {
		return err
	}
	// One card per task.
	var had bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM manager_feedback WHERE tenant = $1 AND kind = $2 AND task = $3)`,
		tv.Name, taskAnswer, task).Scan(&had); err != nil || had {
		return err
	}
	day := tenantDay(tv, now)
	cards, err := c.cardsToday(ctx, tx, tv.Name, person, day)
	if err != nil {
		return err
	}
	answers, err := feedbackCount(ctx, tx, tv.Name, person, taskAnswer, day)
	if err != nil || cards+answers >= noncriticalPerDay(tv) {
		return err
	}
	return c.feedbackCard(ctx, tx, now, tv.Name, taskAnswer, task, adminChat{Principal: person, Channel: channel, ExternalID: external},
		"notice.task_answer", nil, true, nil)
}

// answerFeedbackInput is what an analysis reads of the answer feedback of its tenant: per task
// type of the last answerFeedbackDays days, the cards sent and the "Useful" and "Not needed"
// presses, closed numbers only (INS-7). Nil when the tenant has the feedback off.
func answerFeedbackInput(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView) ([]any, error) {
	if tv == nil || !tv.Spec.Attention.AnswerFeedback {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `SELECT t.type, count(*), count(*) FILTER (WHERE f.value = 'useful'), count(*) FILTER (WHERE f.value = 'not_needed')
		FROM manager_feedback f JOIN tasks t ON t.id = f.task
		WHERE f.tenant = $1 AND f.kind = $2 AND f.created_at >= $3
		GROUP BY t.type ORDER BY count(*) DESC, t.type LIMIT $4`, tv.Name, taskAnswer, now.AddDate(0, 0, -answerFeedbackDays), maxAnswerFeedbackTypes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var typ string
		var sent, useful, notNeeded int
		if err := rows.Scan(&typ, &sent, &useful, &notNeeded); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"type": typ, "sent": sent, "useful": useful, "not_needed": notNeeded})
	}
	return out, rows.Err()
}
