// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"context"
	"fmt"
	"time"
)

// seedInvestigations is the number of past investigations the history of the world holds: more
// than the min_samples of the trial metrics of core-series.yaml (14 for farm.task.attempts).
const seedInvestigations = 18

// SeedInvestigationLoad writes the history of the world's past investigations into the core
// series of ops-reader of the home tenant: the outcome, the attempts and the attempt duration of
// task_type:ops.investigate.v1 over the last day: two of three ran into the attempt deadline of
// five minutes; half of those finished on the second attempt, half failed after all three. A
// fresh stand
// has run one investigation, too little for an analysis to find anything; these are the
// points the observe loop would have written for such tasks (OBV-5). The keys are fixed per
// point, so a second seeding writes nothing again.
func (s *Stand) SeedInvestigationLoad(ctx context.Context) error {
	now := time.Now().UTC()
	db := s.Bench.Store().Pool
	const subject = "task_type:ops.investigate.v1"
	for i := range seedInvestigations {
		at := now.Add(-time.Duration(i+1) * 70 * time.Minute)
		key := fmt.Sprintf("sim-seed:investigation:%02d", i)
		attempts := 1.0
		outcome := "done"
		duration := 40.0 + float64(i%5)*12
		if i%3 != 2 {
			// Two of three ran into the attempt deadline: their first attempt was cut at five
			// minutes, the duration point of the task. Half of those finished on the second
			// attempt, half failed after the third.
			attempts, duration = 2, 300
			if i%2 == 0 {
				attempts, outcome = 3, "failed"
			}
		}
		for _, p := range []struct {
			series string
			num    *float64
			enum   *string
		}{
			{"farm.task.outcome", nil, &outcome},
			{"farm.task.attempts", &attempts, nil},
			{"farm.attempt.duration", &duration, nil},
		} {
			if _, err := db.Exec(ctx, `INSERT INTO obs_points (tenant, series, subject, at, num, enum, source, data_mark, role, key, created_at)
				VALUES ($8, $1, $2, $3, $4, $5, 'core', 'internal', 'ops-reader', $6, $7) ON CONFLICT DO NOTHING`,
				p.series, subject, at, p.num, p.enum, key, now, s.Home()); err != nil {
				return err
			}
		}
	}
	s.log.Addf(KindDirector, "the history of the world: %d past investigations of ops-reader in the core series, two of three cut at the attempt deadline, half of those failed", seedInvestigations)
	return nil
}
