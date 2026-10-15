// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The translator (docs/coordination.md, TRN-2, TRN-3, TRN-5). Its task starts only from the text
// its initiator typed; its result is the closed translation-result, and the answer to the person
// is built by code from that result and the vocabulary names, never from model text.

// translationSchema is spec/model/translation-result.schema.json.
const translationSchema = "urn:botaru:schema:translation-result:1"

// checkTranslation is the check of a translator result beyond its type's schema: the closed
// translation-result, and a drafted outcome names a run this task proposed (TRN-3).
func (c *Core) checkTranslation(ctx context.Context, tx pgx.Tx, tenant, task string, result map[string]any) error {
	if c.spec.Validate(translationSchema, result) != nil {
		return refuse("output_schema_invalid", "the result is not a translation-result")
	}
	if result["outcome"] != "drafted" {
		return nil
	}
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM script_runs WHERE tenant = $1 AND id = $2 AND anchor_task = $3)`,
		tenant, result["run_ref"], task).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return refuse("output_schema_invalid", "run_ref is not a run this task proposed")
	}
	return nil
}

// translatorAnswer is the code text of a translator result (TRN-5): a drafted script points at
// its card; otherwise the codes of what is missing and the names of the types and actions the
// initiator may use through this role.
func (c *Core) translatorAnswer(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant, role, initiator string, result map[string]any) (string, error) {
	var locale *string
	if err := tx.QueryRow(ctx, `SELECT locale FROM principals WHERE tenant = $1 AND id = $2`, tenant, initiator).Scan(&locale); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	loc := deref(locale)
	outcome, _ := result["outcome"].(string)
	if outcome == "drafted" {
		return c.texts.Render(loc, "translator.drafted", nil), nil
	}
	var missing []string
	list, _ := result["missing"].([]any)
	for _, m := range list {
		if code, ok := m.(string); ok {
			missing = append(missing, c.texts.Render(loc, "translator.missing."+code, nil))
		}
	}
	types, actions := []string{}, []string{}
	if r, ok := cat.Roles[role]; ok && r.Scripts != nil {
		if tv := cat.Tenants[tenant]; tv != nil {
			groups, err := principalGroups(ctx, tx, now, tv, initiator)
			if err != nil {
				return "", err
			}
			for _, t := range r.Scripts.StepTypes {
				if tt, ok := cat.TaskTypes[t]; ok && hasCap(tv, groups, "task:"+t) {
					name := tt.Title
					if name == "" {
						name = t
					}
					types = append(types, name)
				}
			}
			for _, a := range r.Scripts.Actions {
				if act, ok := cat.Actions[a]; ok && hasCap(tv, groups, "action.request:"+a) {
					name := act.Title
					if name == "" {
						name = a
					}
					actions = append(actions, name)
				}
			}
		}
	}
	slices.Sort(types)
	slices.Sort(actions)
	none := c.texts.Render(loc, "translator.none", nil)
	join := func(l []string) string {
		if len(l) == 0 {
			return none
		}
		return strings.Join(l, ", ")
	}
	return c.texts.Render(loc, "translator."+outcome, map[string]any{
		"missing": strings.Join(missing, "; "), "types": join(types), "actions": join(actions),
	}), nil
}
