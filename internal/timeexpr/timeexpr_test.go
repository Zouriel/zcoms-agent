package timeexpr

import (
	"testing"
	"time"
)

// fixed reference instant for deterministic parsing: Thu 2026-06-18 10:00 local.
func refNow() time.Time { return time.Date(2026, 6, 18, 10, 0, 0, 0, time.Local) }

func TestParse(t *testing.T) {
	now := refNow()
	tests := []struct {
		name string
		in   string
		want time.Time
		err  bool
	}{
		{"now keyword", "now", now, false},
		{"plus duration", "+30m", now.Add(30 * time.Minute), false},
		{"bare duration", "90m", now.Add(90 * time.Minute), false},
		{"compound duration", "1h30m", now.Add(90 * time.Minute), false},
		{"hours", "+2h", now.Add(2 * time.Hour), false},

		// Long horizons: the whole point of the calendar units.
		{"days short", "+10d", now.AddDate(0, 0, 10), false},
		{"days word", "5 days", now.AddDate(0, 0, 5), false},
		{"weeks short", "+3w", now.AddDate(0, 0, 21), false},
		{"weeks word", "2 weeks", now.AddDate(0, 0, 14), false},
		{"months short", "+2mo", now.AddDate(0, 2, 0), false},
		{"months word", "2 months", now.AddDate(0, 2, 0), false},
		{"a year out", "1y", now.AddDate(1, 0, 0), false},

		{"clock later today", "15:30", time.Date(2026, 6, 18, 15, 30, 0, 0, time.Local), false},
		{"clock passed rolls tomorrow", "09:00", time.Date(2026, 6, 19, 9, 0, 0, 0, time.Local), false},
		{"full timestamp T", "2026-06-18T15:30", time.Date(2026, 6, 18, 15, 30, 0, 0, time.Local), false},
		{"far future date", "2026-12-25 09:00", time.Date(2026, 12, 25, 9, 0, 0, 0, time.Local), false},

		{"negative duration rejected", "+-5m", time.Time{}, true},
		{"absolute past rejected", "2026-06-18T09:00", time.Time{}, true},
		{"minutes not a calendar unit", "90mo", now.AddDate(0, 90, 0), false},
		{"empty", "", time.Time{}, true},
		{"garbage", "sometime soon", time.Time{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.in, now)
			if tt.err {
				if err == nil {
					t.Fatalf("Parse(%q) = %v, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q) unexpected error: %v", tt.in, err)
			}
			if !got.Equal(tt.want) {
				t.Fatalf("Parse(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
