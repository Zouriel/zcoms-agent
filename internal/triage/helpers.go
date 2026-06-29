package triage

import (
	"fmt"
	"strings"
	"time"
)

// platformLabel renders a triage source for the digest prompt.
func platformLabel(source string) string {
	if source == "wa" {
		return "WhatsApp"
	}
	return "Telegram"
}

// snippet truncates long message text for the prompt.
func snippet(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// timeLabel renders when a message arrived, e.g. "2h ago (Sat 14:32)".
func timeLabel(t time.Time) string {
	if t.IsZero() {
		return "unknown time"
	}
	return fmt.Sprintf("%s (%s)", humanAgo(t), t.Format("Mon 15:04"))
}

func humanAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
