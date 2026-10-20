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
// series of ops-reader: the outcome, the attempts and the attempt duration of
// task_type:ops.investigate.v1 over the last day: two of three took all three attempts near the
// attempt timeout and a third failed. A fresh stand
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
			// Two of three ran into the lease and took all three attempts, each close to the
			// five minutes of the attempt timeout; half of those failed in the end.
			attempts, duration = 3, 260+float64(i%4)*12
			if i%2 == 0 {
				outcome = "failed"
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
				VALUES ('demo', $1, $2, $3, $4, $5, 'core', 'internal', 'ops-reader', $6, $7) ON CONFLICT DO NOTHING`,
				p.series, subject, at, p.num, p.enum, key, now); err != nil {
				return err
			}
		}
	}
	s.log.Addf(KindDirector, "the history of the world: %d past investigations of ops-reader in the core series, two of three with three attempts near the timeout, a third failed", seedInvestigations)
	return nil
}
