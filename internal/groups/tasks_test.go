package groups

import (
	"os"
	"strings"
	"testing"

	"github.com/Zouriel/zcoms-agent/internal/store"
)

type generalProjects struct{}

func (generalProjects) CallContext(int64) (ProjectContext, error) {
	return ProjectContext{General: true, Notes: "Open tasks:\n- #4 Book the venue (Sara)\n"}, nil
}

func (generalProjects) StandupReply(int64, Call) (string, bool) { return "", false }

// A tracked group whose folder holds no repo answers like a general group
// (web only, empty scratch dir) but still sees its open tasks.
func TestTasksGroupWithoutRepoRunsAsGeneral(t *testing.T) {
	c := &fakeComms{}
	var got RunRequest
	h := newHandler(c, func(r RunRequest) (string, error) { got = r; return "Sara has it.", nil })
	h.Scratch = t.TempDir()
	h.Projects = generalProjects{}
	call, _ := CallFromEvent(groupEvent("-100", "60"))
	h.Process(store.GroupPolicy{Kind: store.GroupProject, ProjectID: 1}, call)

	if strings.Join(got.Profile.Tools, ",") != "WebSearch,WebFetch" || got.Profile.HasShell() {
		t.Fatalf("tools = %+v", got.Profile)
	}
	if !strings.HasPrefix(got.Dir, h.Scratch) {
		t.Fatalf("ran in %q, not a scratch dir", got.Dir)
	}
	if _, err := os.Stat(got.Dir); !os.IsNotExist(err) {
		t.Fatal("scratch dir left behind")
	}
	if !strings.Contains(got.Prompt, "Book the venue") || !strings.Contains(got.Prompt, "no files, no shell") {
		t.Fatalf("prompt lacks the tasks or the general reach: %q", got.Prompt)
	}
}
