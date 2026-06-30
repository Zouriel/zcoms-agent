package bridge

import "github.com/Zouriel/zcoms-agent/internal/store"

// Editable canned bridge messages. The owner can reword these from the console;
// an override is stored as the agent.db setting "phrase.<key>" and read LIVE (no
// restart). Defaults are kept plain and dash-free.
//
// To add another editable message: append a row here and call d.phraseOr("<key>")
// where the string used to be hardcoded.

// Phrase is one editable message with its current value + the shipped default.
type Phrase struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Value   string `json:"value"`
	Default string `json:"default"`
}

var phraseDefaults = []struct{ key, label, def string }{
	{"location_gate", "No project picked (you sent a work request)", "Pick a location first (send 'locations')."},
	{"location_gate_file", "No project picked (you sent a file)", "Pick a location first (send 'locations'), then send the file. It needs a project to live in."},
	{"busy", "A previous request is still running", "Still working on your previous message, one moment."},
}

// DefaultPhrase returns the shipped default for key ("" if unknown).
func DefaultPhrase(key string) string {
	for _, p := range phraseDefaults {
		if p.key == key {
			return p.def
		}
	}
	return ""
}

// IsPhraseKey reports a known editable phrase key.
func IsPhraseKey(key string) bool { return DefaultPhrase(key) != "" }

// PhraseOr returns the owner's override for key (agent.db), else the default.
// Read live, so a console edit applies with no restart.
func PhraseOr(st *store.Store, key string) string {
	if st != nil {
		if v, _ := st.GetSetting("phrase." + key); v != "" {
			return v
		}
	}
	return DefaultPhrase(key)
}

// Phrases returns every editable message with its current value (for the console).
func Phrases(st *store.Store) []Phrase {
	out := make([]Phrase, 0, len(phraseDefaults))
	for _, p := range phraseDefaults {
		out = append(out, Phrase{Key: p.key, Label: p.label, Value: PhraseOr(st, p.key), Default: p.def})
	}
	return out
}
