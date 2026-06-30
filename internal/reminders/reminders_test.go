package reminders

import (
	"testing"
	"time"
)

func TestParseRemind(t *testing.T) {
	cases := []struct {
		in            string
		who, task     string
		ok            bool
	}{
		{"remind me to get my wife a rose", "me", "get my wife a rose", true},
		{"remind Sara to send the invoice", "Sara", "send the invoice", true},
		{"remind me about the 3pm sync", "me", "the 3pm sync", true},
		{"reminder me to water the plants", "me", "water the plants", true},
		{"remind me water the plants", "me", "water the plants", true}, // no separator
		{"remind", "", "", false},
		{"remind me", "", "", false}, // no task
	}
	for _, c := range cases {
		who, task, ok := ParseRemind(c.in)
		if ok != c.ok || who != c.who || task != c.task {
			t.Errorf("ParseRemind(%q) = (%q,%q,%v), want (%q,%q,%v)", c.in, who, task, ok, c.who, c.task, c.ok)
		}
	}
}

func TestIsSelf(t *testing.T) {
	for _, w := range []string{"me", "Me", "myself", "I", "self"} {
		if !isSelf(w) {
			t.Errorf("isSelf(%q) = false, want true", w)
		}
	}
	if isSelf("Sara") {
		t.Error("isSelf(Sara) = true, want false")
	}
}

func TestHeuristicClassify(t *testing.T) {
	now := time.Date(2026, 6, 30, 10, 0, 0, 0, time.Local) // a Tuesday, 10:00
	h := heuristic{}

	// Rose: one-off, until-done, no time.
	d := h.Classify("get my wife a rose", now)
	if d.Kind != "oneoff" || d.DeadlineBound || !d.EventAt.IsZero() {
		t.Errorf("rose: %+v", d)
	}
	if d.PreDelay != defaultPreDelay || d.PostGap != defaultPostGap {
		t.Errorf("rose gaps: %+v", d)
	}

	// 3pm sync: one-off, deadline-bound, event at 15:00 today.
	d = h.Classify("the 3pm sync", now)
	if d.Kind != "oneoff" || !d.DeadlineBound {
		t.Errorf("sync: %+v", d)
	}
	if d.EventAt.Hour() != 15 || d.EventAt.Minute() != 0 {
		t.Errorf("sync event time: %v", d.EventAt)
	}

	// Recurring weekday standup.
	d = h.Classify("standup every weekday at 9am", now)
	if d.Kind != "recurring" || d.RecurSpec != "weekdays 09:00" {
		t.Errorf("standup: %+v", d)
	}
	if d.EventAt.Weekday() == time.Saturday || d.EventAt.Weekday() == time.Sunday {
		t.Errorf("standup landed on a weekend: %v", d.EventAt)
	}

	// Daily recurring.
	d = h.Classify("take meds daily at 8pm", now)
	if d.Kind != "recurring" || d.RecurSpec != "daily 20:00" {
		t.Errorf("daily: %+v", d)
	}
}

func TestHeuristicClassifyReply(t *testing.T) {
	h := heuristic{}
	pos := []string{"done", "yes did it", "bought it", "all sent ✅", "yeah finished"}
	neg := []string{"not yet", "no", "didn't get to it", "haven't", "later", "still need to"}
	ack := []string{"ok", "okay", "sure", "will do", "on it", "got it", "alright", "👍"}
	for _, r := range pos {
		v := h.ClassifyReply("buy a rose", r)
		if !v.Positive || v.Ack {
			t.Errorf("reply %q = %+v, want positive", r, v)
		}
	}
	for _, r := range neg {
		v := h.ClassifyReply("buy a rose", r)
		if v.Positive || v.Ack {
			t.Errorf("reply %q = %+v, want negative", r, v)
		}
	}
	// Acknowledgments are neither done nor a refusal — the "okay = done" bug.
	for _, r := range ack {
		v := h.ClassifyReply("buy a rose", r)
		if !v.Ack || v.Positive {
			t.Errorf("reply %q = %+v, want Ack", r, v)
		}
	}
}

func TestRequesterKey(t *testing.T) {
	r := Requester{Transport: "telegram", Handle: "@alice"}
	if r.key() != "telegram|@alice" {
		t.Errorf("key = %q", r.key())
	}
}
