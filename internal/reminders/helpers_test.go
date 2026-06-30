package reminders

import (
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Zouriel/zcoms-agent/internal/store"
	"github.com/Zouriel/zcoms/client"
)

// fakeClient records outbound sends and serves canned contacts.
type fakeClient struct {
	sent     []string // "transport|addr|text"
	marks    int
	contacts []client.Contact
}

func (f *fakeClient) SendOn(transport, to, text string) (client.Response, error) {
	f.sent = append(f.sent, transport+"|"+to+"|"+text)
	return client.Response{}, nil
}
func (f *fakeClient) MarkReadOn(string, string, []string) error { f.marks++; return nil }
func (f *fakeClient) Resolve(string) (int64, error)             { return 1, nil }
func (f *fakeClient) ResolveContact(who string) ([]client.Contact, error) {
	var out []client.Contact
	for _, c := range f.contacts {
		if strings.HasPrefix(strings.ToLower(c.Name), strings.ToLower(who)) {
			out = append(out, c)
		}
	}
	return out, nil
}
func (f *fakeClient) lastText() string {
	if len(f.sent) == 0 {
		return ""
	}
	p := strings.SplitN(f.sent[len(f.sent)-1], "|", 3)
	return p[len(p)-1]
}

// fakeTurn returns scripted agent outputs, one per call.
type fakeTurn struct {
	outs  []string
	calls int
}

func (f *fakeTurn) run(prompt, resumeID string) (string, string, error) {
	o := ""
	if f.calls < len(f.outs) {
		o = f.outs[f.calls]
	}
	f.calls++
	return o, "sess", nil
}

func newTestComp(t *testing.T, turn AgentTurn) (*Comp, *fakeClient, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	fc := &fakeClient{}
	d := New(fc, st, "@owner", 999, nil, turn)
	d.log = log.New(io.Discard, "", 0)
	return d, fc, st
}

func reload(t *testing.T, st *store.Store, id int64) store.Reminder {
	t.Helper()
	r, ok, err := st.GetReminder(id)
	if err != nil || !ok {
		t.Fatalf("reload #%d: ok=%v err=%v", id, ok, err)
	}
	return r
}
