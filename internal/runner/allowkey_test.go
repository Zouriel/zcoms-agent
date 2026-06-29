package runner

import "testing"

func TestAllowKeyMatching(t *testing.T) {
	cases := []struct{ platform, stored, inbound string }{
		{"telegram", "@ZourielCorbet", "@zourielcorbet"}, // case-insensitive
		{"telegram", "ali", "@ali"},                      // stored bare, inbound @-form
		{"whatsapp", "+960 765-4321", "9607654321@s.whatsapp.net"}, // number vs jid
		{"whatsapp", "9607654321", "+9607654321"},                 // digits vs +form
	}
	for _, c := range cases {
		if k1, k2 := AllowKey(c.platform, c.stored), AllowKey(c.platform, c.inbound); k1 != k2 {
			t.Errorf("%s: stored %q -> %q != inbound %q -> %q", c.platform, c.stored, k1, c.inbound, k2)
		}
	}
	// platforms must not cross-match even with identical digits/text
	if AllowKey("telegram", "9607654321") == AllowKey("whatsapp", "9607654321") {
		t.Error("telegram and whatsapp keys collided")
	}
}

func TestNormalizeAllowHandle(t *testing.T) {
	if got := NormalizeAllowHandle("whatsapp", "+960 765-4321"); got != "9607654321" {
		t.Errorf("wa normalize = %q", got)
	}
	if got := NormalizeAllowHandle("telegram", "ali"); got != "@ali" {
		t.Errorf("tg normalize = %q", got)
	}
}
