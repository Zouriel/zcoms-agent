package agentd

import (
	"testing"
	"time"

	"github.com/Zouriel/zcoms-agent/internal/store"
)

func TestTriageGroupDueInterval(t *testing.T) {
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.Local)
	g := store.TriageGroup{ScheduleKind: "interval", ScheduleSpec: "1h"}

	// Never run → due.
	if !triageGroupDue(g, now) {
		t.Fatal("interval group with no last run should be due")
	}
	// Ran 2h ago → due.
	g.LastRunAt = now.Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	if !triageGroupDue(g, now) {
		t.Fatal("interval group last run 2h ago (1h spec) should be due")
	}
	// Ran 30m ago → not due.
	g.LastRunAt = now.Add(-30 * time.Minute).UTC().Format(time.RFC3339)
	if triageGroupDue(g, now) {
		t.Fatal("interval group last run 30m ago (1h spec) should NOT be due")
	}
}

func TestTriageGroupDueDaily(t *testing.T) {
	g := store.TriageGroup{ScheduleKind: "daily", ScheduleSpec: "09:00,18:00"}

	// 10:00, never run today → the 09:00 slot is due.
	at10 := time.Date(2026, 6, 29, 10, 0, 0, 0, time.Local)
	if !triageGroupDue(g, at10) {
		t.Fatal("daily group should be due after a passed slot")
	}
	// 08:00, never run → no slot passed yet → not due.
	at8 := time.Date(2026, 6, 29, 8, 0, 0, 0, time.Local)
	if triageGroupDue(g, at8) {
		t.Fatal("daily group should NOT be due before any slot")
	}
	// 10:00 but already ran at 09:30 today → not due (until the 18:00 slot).
	g.LastRunAt = time.Date(2026, 6, 29, 9, 30, 0, 0, time.Local).UTC().Format(time.RFC3339)
	if triageGroupDue(g, at10) {
		t.Fatal("daily group already ran after the 09:00 slot should NOT re-fire")
	}
	// 18:30, last run 09:30 → the 18:00 slot is now due.
	at1830 := time.Date(2026, 6, 29, 18, 30, 0, 0, time.Local)
	if !triageGroupDue(g, at1830) {
		t.Fatal("daily group should fire on the second slot")
	}
}
