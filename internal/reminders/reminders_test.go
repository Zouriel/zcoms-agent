package reminders

import "testing"

func TestParseRemind(t *testing.T) {
	cases := []struct {
		in        string
		who, task string
		ok        bool
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

func TestRequesterKey(t *testing.T) {
	r := Requester{Transport: "telegram", Handle: "@alice"}
	if r.key() != "telegram|@alice" {
		t.Errorf("key = %q", r.key())
	}
}
