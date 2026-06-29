package triage

import (
	"testing"

	"github.com/Zouriel/zcoms-agent/internal/runner"
)

func TestAllowListed(t *testing.T) {
	allow := runner.Allowlist{
		"telegram|@alice":     {Role: runner.RoleRead},
		"whatsapp|9609752353": {Role: runner.RoleRead},
	}
	cases := []struct {
		m    message
		want bool
	}{
		{message{Source: "tg", Sender: "@alice"}, true},
		{message{Source: "tg", Sender: "@bob"}, false},
		{message{Source: "wa", WAChat: "9609752353@s.whatsapp.net"}, true},
		{message{Source: "wa", WAChat: "9999999999@s.whatsapp.net"}, false},
	}
	for _, c := range cases {
		if got := allowListed(allow, c.m); got != c.want {
			t.Errorf("allowListed(%+v) = %v, want %v", c.m, got, c.want)
		}
	}
}

// The important-index regex pulls the message number out of a triage bullet so
// only those senders get the auto-reply.
func TestImportantRe(t *testing.T) {
	got := map[int]bool{}
	bullets := []string{
		"• [3] Mon 14:00 — Shanna: needs an answer",
		"- [1] urgent",
		"• general chatter (no index, ignored)",
	}
	for _, b := range bullets {
		if mm := importantRe.FindStringSubmatch(b); mm != nil {
			if mm[1] == "3" || mm[1] == "1" {
				got[len(mm[1])] = true // just exercise the capture
			}
		}
	}
	if importantRe.FindStringSubmatch(bullets[0]) == nil || importantRe.FindStringSubmatch(bullets[1]) == nil {
		t.Fatal("failed to capture index from a [N] bullet")
	}
	if importantRe.FindStringSubmatch(bullets[2]) != nil {
		t.Fatal("captured an index from a bullet with none")
	}
}
