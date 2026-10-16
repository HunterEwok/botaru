// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// With profile monitor alarms reach people only through the monitoring system, which sets
// their silencing and quiet hours (MON-1, DLV-10): an admin in quiet hours gets no alarm_notice
// of a firing alarm, the episode goes as one monitor_alarm delivery, and /prefs says the quiet
// hours of the core do not cover alarms. Without the profile the admin's alarm_notice goes in
// quiet hours too, undeferred.
func TestMonitorAlarmPath(t *testing.T) {
	ctx := context.Background()
	for _, monitor := range []bool{false, true} {
		b := newBench(t)
		b.withAdmins(t)
		if monitor {
			b.monitorOn(t)
		}
		cat, err := b.c.Catalog(ctx)
		if err != nil {
			t.Fatal(err)
		}
		now := b.now(t).In(tenantLocation(cat, "demo"))
		quiet := now.Add(-time.Hour).Format("15:04") + "-" + now.Add(time.Hour).Format("15:04")
		a := Activity{Kind: "message", Channel: "telegram", UpdateID: "p1", Chat: Chat{Kind: "direct", ExternalID: "1002"}, Text: "/prefs quiet " + quiet}
		a.Sender.Identity, a.Sender.Verified = "telegram:1002", true
		res, err := b.c.SubmitActivity(ctx, b.channel, a)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res.Text, "Quiet hours: "+quiet) || strings.Contains(res.Text, "monitoring system") != monitor {
			t.Fatalf("monitor %v: /prefs %q", monitor, res.Text)
		}
		alarm := Alarm{Tenant: "demo", Name: "worker.stale", Subject: AlarmSubject{Kind: "worker", ID: "wk_1"}, Severity: "warn"}
		b.alarmTx(t, func(tx pgx.Tx, now time.Time) error { return b.c.fireAlarm(ctx, tx, now, alarm) })
		notices, exports := countNotices(t, b), countKind(t, b, "monitor_alarm", "")
		if monitor && (notices != 0 || exports != 1) || !monitor && (notices != 1 || exports != 0) {
			t.Fatalf("monitor %v: alarm_notice %d, monitor_alarm %d", monitor, notices, exports)
		}
		if !monitor {
			if !slices.ContainsFunc(b.drain(t), func(s sent) bool { return s.kind == "alarm_notice" }) {
				t.Fatal("the alarm_notice waits for the quiet hours")
			}
		}
	}
}
