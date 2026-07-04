package errands

import (
	"testing"
	"time"
)

func TestExtractRecordBlock(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "with space and trailing handoff",
			in:   "chat\nRECORD | Name: Bob\nRole: Dev\nHANDOFF",
			want: "Name: Bob\nRole: Dev",
		},
		{
			name: "spaceless pipe is still captured",
			in:   "RECORD|Name: Bob\nRole: Dev",
			want: "Name: Bob\nRole: Dev",
		},
		{
			name: "stops at next directive",
			in:   "RECORD | Name: Bob\nMSG | hi there",
			want: "Name: Bob",
		},
		{
			name: "no record",
			in:   "MSG | hello",
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractRecordBlock(tt.in); got != tt.want {
				t.Errorf("extractRecordBlock() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseLogLine(t *testing.T) {
	line := "[2026-07-04T10:15:00Z] cleared: note=\"looks fine\""
	ev, ok := parseLogLine("guard", line)
	if !ok {
		t.Fatal("expected the line to parse")
	}
	if ev.Role != "guard" || ev.Event != "cleared" {
		t.Errorf("role/event = %q/%q, want guard/cleared", ev.Role, ev.Event)
	}
	if ev.Detail != `note="looks fine"` {
		t.Errorf("detail = %q", ev.Detail)
	}
	want, _ := time.Parse(time.RFC3339, "2026-07-04T10:15:00Z")
	if !ev.Time.Equal(want) {
		t.Errorf("time = %v, want %v", ev.Time, want)
	}

	if _, ok := parseLogLine("guard", "a wrapped continuation line"); ok {
		t.Error("non-timestamped line should not parse")
	}
}
