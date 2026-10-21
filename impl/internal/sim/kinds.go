// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// The scenarios of the everyday kinds of work beyond the incidents: the availability summary and
// the free question (R-04), the monthly and the nightly sensor reports (R-09), a CTD file a
// person sends (R-07) and the knowledge of a tenant (R-21).

// typeEnd waits for the first task of a type in a tenant created since a time to end, and
// returns its state, reason and result.
func (d *Director) typeEnd(ctx context.Context, tenant, typ string, since time.Time) (state, reason string, result map[string]any, err error) {
	err = d.S.waitFor(ctx, d.Timeout, func() (bool, error) {
		var raw []byte
		e := d.S.Bench.Store().Pool.QueryRow(ctx, `SELECT state, coalesce(reason, ''), result FROM tasks WHERE tenant = $1 AND type = $2
			AND created_at >= $3 ORDER BY created_at LIMIT 1`, tenant, typ, since).Scan(&state, &reason, &raw)
		if e != nil {
			return false, nil
		}
		result = nil
		_ = json.Unmarshal(raw, &result)
		return state == "completed" || state == "failed" || state == "canceled", nil
	})
	if errors.Is(err, context.DeadlineExceeded) {
		err = fmt.Errorf("no task %s of %s ended within %v (state %q)", typ, tenant, d.Timeout, state)
	}
	return
}

// toolsNotDone are the tools of a list the task of a message key did not call to the end.
func (d *Director) toolsNotDone(ctx context.Context, tenant, key string, tools []string) []string {
	var missing []string
	for _, tool := range tools {
		var done int
		_ = d.S.Bench.Store().Pool.QueryRow(ctx, `SELECT count(*) FROM toolcalls t JOIN tasks k ON k.id = t.task
			WHERE k.tenant = $3 AND k.idempotency_key = $1 AND t.tool = $2 AND t.state = 'done'`, key, tool, tenant).Scan(&done)
		if done == 0 {
			missing = append(missing, tool)
		}
	}
	return missing
}

// reports plays the reports of the home tenant: alice asks whether the sites are up and how the
// day went, runs the monthly report, and the nightly sensor check runs on its schedule; the
// reporter writes the paragraphs of both reports from the numbers of the jobs.
func reports(ctx context.Context, d *Director, sc string) error {
	home := d.S.Home()
	key, err := d.S.Say("alice", "alice-dm", "/health Are the sites and the API up right now?")
	if err != nil {
		return err
	}
	state, reason, result, err := d.taskEnd(ctx, key)
	if err != nil {
		return err
	}
	missing := d.toolsNotDone(ctx, home, key, []string{"checks/latest"})
	verdict := fmt.Sprint(result["verdict"])
	d.check(sc, "health_answered", state == "completed" && len(missing) == 0 && (verdict == "up" || verdict == "degraded"),
		"state %s %s, tools not called %v, verdict %s", state, reason, missing, verdict)
	key, err = d.S.Say("alice", "alice-dm", "How did the day go on the farm?")
	if err != nil {
		return err
	}
	state, reason, result, err = d.taskEnd(ctx, key)
	if err != nil {
		return err
	}
	d.check(sc, "summary_answered", state == "completed" && fmt.Sprint(result["answer"]) != "", "state %s %s: %s", state, reason, clip(fmt.Sprint(result["answer"]), 200))

	start := time.Now().UTC()
	month := start.AddDate(0, -1, 0).Format("2006-01")
	if _, err := d.S.Say("alice", "alice-dm", "/run month-report "+month); err != nil {
		return err
	}
	state, reason, nums, err := d.typeEnd(ctx, home, "report.monthly.numbers.v1", start)
	if err != nil {
		return err
	}
	d.check(sc, "month_numbers", state == "completed" && nums["month"] == month, "state %s %s, result %v", state, reason, nums)
	state, reason, result, err = d.typeEnd(ctx, home, "report.monthly.v1", start)
	if err != nil {
		return err
	}
	text := fmt.Sprint(result["text"])
	// The number of runs as a number of its own, not a digit of the month.
	runs := fmt.Sprint(nums["ctd_qc_runs"])
	hasRuns := regexp.MustCompile(`(^|[^\d.])` + regexp.QuoteMeta(runs) + `([^\d.]|$)`).MatchString(strings.ReplaceAll(text, month, ""))
	d.check(sc, "month_report", state == "completed" && strings.Contains(text, month) && hasRuns,
		"state %s %s, %s CTD QC runs: %s", state, reason, runs, clip(text, 200))

	start = time.Now().UTC()
	refused := d.S.childRefused.Load()
	if err := d.S.SetSchedule(ctx, home, SensorSchedule, true); err != nil {
		return err
	}
	state, reason, checked, err := d.typeEnd(ctx, home, "data.sensor_check.v1", start)
	if offErr := d.S.SetSchedule(ctx, home, SensorSchedule, false); offErr != nil && err == nil {
		err = offErr
	}
	if err != nil {
		return err
	}
	// A failure is the known gap only when the core refused the check's child as such.
	gap := ""
	if d.S.childRefused.Load() > refused {
		gap = GapSystemChild
	}
	d.checkGap(sc, "sensor_check", gap, state == "completed" && jsonNum(checked["sensors_checked"]) > 0, "state %s %s, result %v", state, reason, checked)
	if state != "completed" {
		// The report is the child the check enqueues: without the check there is none to wait for.
		d.checkGap(sc, "sensor_report", gap, false, "no sensor check completed")
		return nil
	}
	state, reason, result, err = d.typeEnd(ctx, home, "report.sensor.v1", start)
	if err != nil {
		return err
	}
	d.check(sc, "sensor_report", state == "completed" && fmt.Sprint(result["text"]) != "", "state %s %s: %s", state, reason, clip(fmt.Sprint(result["text"]), 200))
	return nil
}

// ctdFile is the CTD profile of the world a person sends to data-reader.
const ctdFile = "ctd-st-05.csv"

// describeFile plays a CTD file a person of the lab tenant sends with /describe: data-reader
// reads it with the data plugin and copies the plugin's numbers into the result.
func describeFile(ctx context.Context, d *Director, sc string) error {
	lab := d.S.LabTenant()
	unnamed := d.S.Brain.NoAttachments()
	key, err := d.S.SayWithFile(ctx, d.S.PersonIn(lab, "bob"), d.S.PersonIn(lab, "bob-dm"), "/describe What is in this CTD profile?", ctdFile)
	if err != nil {
		return err
	}
	state, reason, result, err := d.taskEnd(ctx, key)
	if err != nil {
		return err
	}
	missing := d.toolsNotDone(ctx, lab, key, []string{"data/describe"})
	// A failure is the known gap only when the bot found no attachment to name.
	gap := ""
	if d.S.Brain.NoAttachments() > unnamed {
		gap = GapAttachments
	}
	d.checkGap(sc, "ctd_described", gap, state == "completed" && len(missing) == 0 && jsonNum(result["rows"]) > 0,
		"state %s %s, tools not called %v, rows %v, depth_max_m %v", state, reason, missing, result["rows"], result["depth_max_m"])
	return nil
}

// kbManual is the document the knowledge scenario uploads; kbWord is the word the question and
// the document share.
const (
	kbWord   = "thermosalinograph"
	kbManual = "# Thermosalinograph TSG-9 manual\n\nThe thermosalinograph TSG-9 needs a clean conductivity cell. " +
		"Flush the cell with fresh water after every cruise and check the pump flow before the next one.\n"
)

// knowledge plays the knowledge of a tenant: bob uploads a manual to the ingested corpus, the
// ingest job of data-job cuts it into fragments, and ops-reader answers a question from it with
// kb_search and kb_read.
func knowledge(ctx context.Context, d *Director, sc string) error {
	home := d.S.Home()
	start := time.Now().UTC()
	doc, err := d.S.UploadKnowledge(ctx, home, "vendor-manuals", "tsg-9-manual.md", []byte(kbManual))
	d.check(sc, "manual_uploaded", err == nil && doc != "", "document %s %v", doc, err)
	if err != nil {
		return nil
	}
	state, reason, result, err := d.typeEnd(ctx, home, "kb.ingest.v1", start)
	if err != nil {
		return err
	}
	d.check(sc, "manual_indexed", state == "completed" && jsonNum(result["fragments"]) > 0, "state %s %s, result %v", state, reason, result)
	key, err := d.S.Say("bob", "bob-dm", "/logs What does the manual say about the "+kbWord+"?")
	if err != nil {
		return err
	}
	state, reason, result, err = d.taskEnd(ctx, key)
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(result)
	d.check(sc, "manual_answered", state == "completed" && strings.Contains(strings.ToLower(string(raw)), "conductivity cell"),
		"state %s %s: %s", state, reason, clip(string(raw), 200))
	return nil
}

// scriptRequest is the plain words of the translator scenario: check the last pipeline run and
// clean up after it. The steps read the pipeline status (internal), so the run card
// fits the Telegram chat of the lab tenant; a script with a step of a raw_logs task, such as a
// disk investigation, would carry raw_logs and its card would be withheld there.
const scriptRequest = "/script Check the last pipeline run and then clean up the pipeline temporary files."

// translatorScript plays the script translator (R-13) in the lab tenant: bob asks in plain words,
// the translator drafts a script, bob runs it on its card, the status step reads the run of the
// world and the cleanup step asks for its approval, which the safe policy gives.
func translatorScript(ctx context.Context, d *Director, sc string) error {
	lab := d.S.LabTenant()
	bob, dm := d.S.PersonIn(lab, "bob"), d.S.PersonIn(lab, "bob-dm")
	cards := len(d.S.Cards())
	key, err := d.S.Say(bob, dm, scriptRequest)
	if err != nil {
		return err
	}
	state, reason, result, err := d.taskEnd(ctx, key)
	if err != nil {
		return err
	}
	run, _ := result["run_ref"].(string)
	d.check(sc, "script_drafted", state == "completed" && result["outcome"] == "drafted" && run != "", "state %s %s, result %v", state, reason, result)
	if run == "" {
		return nil
	}
	// bob runs the script on its card (SCR-6).
	var card *Card
	err = d.S.waitFor(ctx, time.Minute, func() (bool, error) {
		for _, c := range d.S.Cards()[cards:] {
			if c.Chat == dm && c.Pressed == "" && slices.ContainsFunc(c.Buttons, func(b Button) bool { return b.Code() == "ap" }) {
				card = &c
				return true, nil
			}
		}
		return false, nil
	})
	d.check(sc, "run_card", err == nil, "the card of run %s", run)
	if err != nil {
		return nil
	}
	if err := d.S.Press(card.No, "ap", bob); err != nil {
		return err
	}
	var runState string
	err = d.S.waitFor(ctx, d.Timeout, func() (bool, error) {
		e := d.S.Bench.Store().Pool.QueryRow(ctx, `SELECT state FROM script_runs WHERE id = $1`, run).Scan(&runState)
		return e == nil && slices.Contains([]string{"completed", "failed", "canceled", "rejected"}, runState), nil
	})
	steps := map[string]string{}
	rows, e := d.S.Bench.Store().Pool.Query(ctx, `SELECT id, state FROM script_steps WHERE run = $1`, run)
	if e == nil {
		for rows.Next() {
			var id, st string
			if rows.Scan(&id, &st) == nil {
				steps[id] = st
			}
		}
		rows.Close()
	}
	d.check(sc, "script_ran", err == nil && runState == "completed" && steps["check"] == "ok" && steps["clean"] == "ok",
		"run %s %s, steps %v", run, runState, steps)
	return nil
}

// translatorRefuses plays the refusals of the translator (R-14): a request outside its
// vocabulary is unsupported and a vague one ambiguous, and neither makes a run.
func translatorRefuses(ctx context.Context, d *Director, sc string) error {
	for _, ask := range []struct{ text, outcome string }{
		{"/script Order a new server and install a database on it.", "unsupported"},
		{"/script Do something about the things that are slow.", "ambiguous"},
	} {
		key, err := d.S.Say("bob", "bob-dm", ask.text)
		if err != nil {
			return err
		}
		state, reason, result, err := d.taskEnd(ctx, key)
		if err != nil {
			return err
		}
		d.check(sc, "refused_"+ask.outcome, state == "completed" && result["outcome"] == ask.outcome && result["run_ref"] == nil,
			"state %s %s, result %v", state, reason, result)
	}
	return nil
}

// botNotes plays the messages between bots (R-15): a sensor of the home tenant falls silent,
// the sensor watch of data-job sends a sensor note to the inbox of ops-reader, and the summary
// of the day ops-reader writes for alice reads it and names the sensor.
func botNotes(ctx context.Context, d *Director, sc string) error {
	home := d.S.Home()
	sensors := catalogList(d.S.cfg.Work, home, "sensors")
	if len(sensors) == 0 {
		return fmt.Errorf("tenant %s has no sensors in its catalog", home)
	}
	sensor := sensors[len(sensors)-1]
	fault := SensorFault(home, sensor)
	start := time.Now().UTC()
	if err := d.world("sensor "+sensor+" falls silent (the director's fault)", func(w *World) { w.Faults[fault] = true }); err != nil {
		return err
	}
	defer func() { _ = d.world("sensor "+sensor+" sends again", func(w *World) { delete(w.Faults, fault) }) }()
	inboxes := func(states ...string) (int, error) {
		var n int
		err := d.S.Bench.Store().Pool.QueryRow(ctx, `SELECT count(DISTINCT inbox_kind || ':' || inbox_name) FROM messages WHERE tenant = $1
			AND type = 'report.sensor_note.v1' AND created_at >= $2 AND state = ANY($3)`, home, start, states).Scan(&n)
		return n, err
	}
	err := d.S.waitFor(ctx, d.Timeout, func() (bool, error) {
		n, err := inboxes("queued", "leased", "acked")
		return n == len(NoteTo), err
	})
	d.check(sc, "notes_sent", err == nil, "a sensor note in %v: %v", NoteTo, err)
	if err != nil {
		return nil
	}
	// The fault ends once the note is out, so the watch sends no more (the deferred heal is a
	// safety net for an early return).
	if err := d.world("sensor "+sensor+" sends again", func(w *World) { delete(w.Faults, fault) }); err != nil {
		return err
	}
	key, err := d.S.Say("alice", "alice-dm", "How did the day go on the farm?")
	if err != nil {
		return err
	}
	state, reason, result, err := d.taskEnd(ctx, key)
	if err != nil {
		return err
	}
	answer := fmt.Sprint(result["answer"])
	d.check(sc, "notes_read", state == "completed" && strings.Contains(answer, sensor), "state %s %s: %s", state, reason, clip(answer, 200))
	acked, err := inboxes("acked")
	d.check(sc, "notes_acked", err == nil && acked == len(NoteTo), "inboxes with the note acknowledged %d of %d %v", acked, len(NoteTo), err)
	return nil
}

// followUp plays the continuation of a conversation (R-16): alice asks how the sensors are, the
// summary answers and asks back, which opens the follow-up window, and her next words without a
// command continue that task with its type.
func followUp(ctx context.Context, d *Director, sc string) error {
	home := d.S.Home()
	key, err := d.S.Say("alice", "alice-dm", "How are the sensors today?")
	if err != nil {
		return err
	}
	state, reason, result, err := d.taskEnd(ctx, key)
	if err != nil {
		return err
	}
	var first, until string
	_ = d.S.Bench.Store().Pool.QueryRow(ctx, `SELECT id, coalesce(record->>'followup_until', '') FROM tasks WHERE tenant = $1 AND idempotency_key = $2`,
		home, key).Scan(&first, &until)
	d.check(sc, "window_opened", state == "completed" && result["need_input"] == true && until != "",
		"state %s %s, need_input %v, window until %q", state, reason, result["need_input"], until)
	key, err = d.S.Say("alice", "alice-dm", "And was it the same yesterday?")
	if err != nil {
		return err
	}
	state, reason, _, err = d.taskEnd(ctx, key)
	if err != nil {
		return err
	}
	var typ, continues string
	_ = d.S.Bench.Store().Pool.QueryRow(ctx, `SELECT type, coalesce(record->>'continues', '') FROM tasks WHERE tenant = $1 AND idempotency_key = $2`,
		home, key).Scan(&typ, &continues)
	d.check(sc, "continued", state == "completed" && first != "" && continues == first && typ == "ops.summary.v1",
		"state %s %s, type %s, continues %q of %q", state, reason, typ, continues, first)
	return nil
}

// directive plays a person's directive to the farm manager (R-18): bob directs it with /manager
// to look at the pipeline reader, and the seat runs the directive task.
func directive(ctx context.Context, d *Director, sc string) error {
	home := d.S.Home()
	start := time.Now().UTC()
	if _, err := d.S.Say("bob", "bob-dm", "/manager role pipeline-reader Look at why the last pipeline run failed and tell me."); err != nil {
		return err
	}
	state, reason, result, err := d.typeEnd(ctx, home, "manager.directive.v1", start)
	if err != nil {
		return err
	}
	var by string
	_ = d.S.Bench.Store().Pool.QueryRow(ctx, `SELECT coalesce(record->'directive'->>'via', '') FROM tasks WHERE tenant = $1 AND type = 'manager.directive.v1'
		AND created_at >= $2 ORDER BY created_at LIMIT 1`, home, start).Scan(&by)
	d.check(sc, "directive_done", state == "completed" && by == "channel", "state %s %s, via %q: %v", state, reason, by, result)
	return nil
}

// leaver is the person of the home tenant the erasure scenario erases and offboards: no other
// scenario or the load uses dave (carol is the approver of the cards).
const leaver = "dave"

// erasure plays the deletion of a person's data and the leaving of a staff member (R-22):
// dave asks the bot to forget him with /forget, the erasure request runs its steps to the end,
// and then an operator offboards him, which opens a second request that runs to the end too.
func erasure(ctx context.Context, d *Director, sc string) error {
	home := d.S.Home()
	p := d.S.seed.Person(leaver)
	if p == nil {
		return fmt.Errorf("no seed person %s", leaver)
	}
	db := d.S.Bench.Store().Pool
	start := time.Now().UTC()
	// ended waits for the latest erasure request of dave other than one already seen to end.
	ended := func(seen string) (string, string, []string, error) {
		var id, reason, state string
		err := d.S.waitFor(ctx, d.Timeout, func() (bool, error) {
			e := db.QueryRow(ctx, `SELECT id, reason, state FROM erasure_requests WHERE tenant = $1 AND subject = $2 AND id <> $3
				AND created_at >= $4 ORDER BY created_at DESC LIMIT 1`, home, p.ID, seen, start).Scan(&id, &reason, &state)
			return e == nil && (state == "done" || state == "failed"), nil
		})
		var open []string
		rows, e := db.Query(ctx, `SELECT store || ' ' || state FROM erasure_steps WHERE request = $1 AND state NOT IN ('done', 'not_applicable') ORDER BY store`, id)
		if e == nil {
			for rows.Next() {
				var s string
				if rows.Scan(&s) == nil {
					open = append(open, s)
				}
			}
			rows.Close()
		}
		return id, reason + " " + state, open, err
	}
	// dave first tells a bot something of his own, his canary, so the invariant watch can see
	// whether it comes back in a model request after the erasure (I-8).
	key, err := d.S.Say(leaver, leaver+"-dm", "/hello please note my badge code "+p.Canary)
	if err != nil {
		return err
	}
	if _, _, _, err := d.taskEnd(ctx, key); err != nil {
		return err
	}
	if _, err := d.S.Say(leaver, leaver+"-dm", "/forget"); err != nil {
		return err
	}
	first, state, open, err := ended("")
	d.check(sc, "forgotten", err == nil && state == "forget done" && len(open) == 0, "erasure %s, steps not done %v %v", state, open, err)

	var out map[string]any
	err = d.S.Bench.P2(ctx, home, "principal.offboard", map[string]any{"subject": p.ID}, &out)
	if err != nil {
		d.check(sc, "offboarded", false, "principal.offboard: %v", err)
		return nil
	}
	var pstate string
	_ = db.QueryRow(ctx, `SELECT state FROM principals WHERE tenant = $1 AND id = $2`, home, p.ID).Scan(&pstate)
	// The offboarding opens a request of its own, not the one of /forget.
	_, state, open, err = ended(first)
	d.check(sc, "offboarded", pstate == "offboarded" && err == nil && state == "offboarding done" && len(open) == 0,
		"principal %s, erasure %s, steps not done %v %v", pstate, state, open, err)
	return nil
}
