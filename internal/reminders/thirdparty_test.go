package reminders

import "testing"

func TestThirdParty(t *testing.T) {
	cases := []struct {
		task    string
		who     string
		rest    string
		ok      bool
		comment string
	}{
		// The exact task text that reached the owner's own calendar and made the
		// reminder loop greet him as "Raani".
		{"Remind Raani to take her father's report", "Raani", "take her father's report", true, "the reported failure"},
		{"remind Sara about the invoice", "Sara", "the invoice", true, "about as the separator"},
		{"please remind my wife to leave by 6", "my wife", "leave by 6", true, "two-word who behind please"},
		{"can you remind Simi that her holiday starts Monday", "Simi", "her holiday starts Monday", true, "that as the separator"},

		// The owner's own schedule — must stay on their calendar untouched.
		{"remind me to water the plants", "", "", false, "self"},
		{"Remind myself about the dentist", "", "", false, "self, other wording"},
		{"remind me and Sara to leave early", "", "", false, "self plus someone else is still self"},

		// Ordinary events that merely contain the word, or no target at all.
		{"Take wife to class, then teach class", "", "", false, "no remind verb"},
		{"Classes every Sunday and Thursday", "", "", false, "plain event"},
		{"reminder", "", "", false, "verb with nothing after it"},
		{"remind Raani", "", "", false, "no task"},
	}
	for _, c := range cases {
		who, rest, ok := ThirdParty(c.task)
		if ok != c.ok {
			t.Errorf("ThirdParty(%q) ok = %v, want %v (%s)", c.task, ok, c.ok, c.comment)
			continue
		}
		if ok && (who != c.who || rest != c.rest) {
			t.Errorf("ThirdParty(%q) = (%q, %q), want (%q, %q)", c.task, who, rest, c.who, c.rest)
		}
	}
}
