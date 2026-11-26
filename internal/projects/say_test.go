package projects

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Zouriel/zcoms-agent/internal/cmdline"
	"github.com/Zouriel/zcoms-agent/internal/store"
	"github.com/Zouriel/zcoms/client"
)

// boundProject binds a fresh folder to the Acme group and returns it.
func boundProject(t *testing.T) (*Service, *fakeOwner, string) {
	t.Helper()
	s, fo, _, _ := trackService(t, acmeGroup)
	if err := s.Store.SetGroupPolicy(store.Owner, store.GroupPolicy{Transport: "telegram", NativeID: "-1001", Name: "Acme", Kind: store.GroupGeneral}); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if _, err := s.Dir("telegram", "Acme", dest, false); err != nil {
		t.Fatal(err)
	}
	fo.sends = nil
	s.OwnerChat = func() string { return "@owner" }
	return s, fo, dest
}

func sayWords(t *testing.T, line string) []string {
	t.Helper()
	w, err := cmdline.Split(line)
	if err != nil {
		t.Fatal(err)
	}
	return w[1:] // drop "project"
}

// Coding sessions never open new threads: an unknown name lands in the
// project's updates thread, which is created once and then reused.
func TestSayFilesInANewThread(t *testing.T) {
	s, fo, dest := boundProject(t)
	a := writeTemp(t, dest, "a.png")
	b := writeTemp(t, dest, "b.pdf")
	out, err := s.Command(sayWords(t, `project say --path `+dest+` --thread "login bug" --file `+a+` --file `+b+` "form fixed"`))
	if err != nil {
		t.Fatal(err)
	}
	if len(fo.created) != 1 || fo.created[0] != DefaultUpdatesThread {
		t.Fatalf("created = %v", fo.created)
	}
	if len(fo.sends) != 2 {
		t.Fatalf("sends = %+v", fo.sends)
	}
	if s0 := fo.sends[0]; s0.to != "-1001" || s0.file != a || s0.text != "form fixed" || s0.thread != "900" {
		t.Fatalf("first = %+v", s0)
	}
	if s1 := fo.sends[1]; s1.file != b || s1.text != "" || s1.thread != "900" {
		t.Fatalf("second = %+v", s1)
	}
	if !strings.Contains(out, DefaultUpdatesThread) || !strings.Contains(out, "login bug") {
		t.Fatalf("out = %q", out)
	}

	// Another unknown name reuses the updates thread, creating nothing.
	fo.sends = nil
	if _, err := s.Command(sayWords(t, `project say --path `+dest+` --thread "Login Bug" step two`)); err != nil {
		t.Fatal(err)
	}
	if len(fo.created) != 1 || len(fo.sends) != 1 || fo.sends[0].thread != "900" || fo.sends[0].text != "step two" {
		t.Fatalf("reuse: created=%v sends=%+v", fo.created, fo.sends)
	}

	// Plain say stays in the main chat.
	fo.sends = nil
	if _, err := s.Command(sayWords(t, `project say --path `+dest+` done`)); err != nil {
		t.Fatal(err)
	}
	if fo.sends[0].thread != "" || fo.sends[0].to != "-1001" {
		t.Fatalf("plain say = %+v", fo.sends[0])
	}
}

func TestSayWithoutThreadsFallsBack(t *testing.T) {
	s, fo, dest := boundProject(t)
	fo.topicErr = errors.New("topics are off in this group; an admin has to enable them in the group settings")
	a := writeTemp(t, dest, "a.png")

	// Files go to the owner's private chat with a note, not the group.
	out, err := s.Command(sayWords(t, `project say --path `+dest+` --thread ui --file `+a+` "new header"`))
	if err != nil {
		t.Fatal(err)
	}
	if len(fo.sends) != 2 || fo.sends[0].to != "@owner" || !strings.Contains(fo.sends[0].text, "could not take a thread") ||
		fo.sends[1].to != "@owner" || fo.sends[1].file != a {
		t.Fatalf("sends = %+v", fo.sends)
	}
	for _, sd := range fo.sends {
		if sd.to == "-1001" {
			t.Fatal("files flooded the main chat")
		}
	}
	if !strings.Contains(out, "private chat") {
		t.Fatalf("out = %q", out)
	}

	// Text alone goes to the main chat.
	fo.sends = nil
	if _, err := s.Command(sayWords(t, `project say --path `+dest+` --thread ui shipped`)); err != nil {
		t.Fatal(err)
	}
	if len(fo.sends) != 1 || fo.sends[0].to != "-1001" || fo.sends[0].thread != "" {
		t.Fatalf("text fallback = %+v", fo.sends)
	}
}

func TestSayRefusesBadFiles(t *testing.T) {
	s, _, dest := boundProject(t)
	for _, line := range []string{
		`project say --path ` + dest + ` --file ` + filepath.Join(dest, "missing.png") + ` x`,
		`project say --path ` + dest + ` --file relative.png x`,
		`project say --path ` + dest + ` --file ` + dest + ` x`,
		`project say --path ` + dest,
	} {
		if _, err := s.Command(sayWords(t, line)); err == nil {
			t.Errorf("accepted: %s", line)
		}
	}
}

func TestSyncRewritesOldBlock(t *testing.T) {
	_, _, dest := boundProject(t)
	agents := filepath.Join(dest, "CLAUDE.md")
	old := "# Notes\n\n" + blockBegin + "\n## zcoms project group\n\nsend it with zc project say \"<text>\".\n" + blockEnd + "\n"
	must(t, os.WriteFile(agents, []byte(old), 0o644))
	if _, err := WriteBlocks(dest, "telegram", "Acme", false); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(agents)
	got := string(b)
	if !strings.Contains(got, "--thread") || !strings.Contains(got, "only for important") || strings.Count(got, "zcoms:begin") != 1 || !strings.HasPrefix(got, "# Notes") {
		t.Fatalf("block not rewritten:\n%s", got)
	}
	if strings.ContainsAny(BlockText("telegram", "A", DefaultUpdatesThread)+BlockText("whatsapp", "A", DefaultUpdatesThread), "–—") {
		t.Fatal("dash in block text")
	}
	if wa := BlockText("whatsapp", "A", DefaultUpdatesThread); strings.Contains(wa, "--thread") || !strings.Contains(wa, "only when asked") {
		t.Fatalf("whatsapp block:\n%s", wa)
	}
}

func writeTemp(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	must(t, os.WriteFile(p, []byte("x"), 0o600))
	return p
}

var _ OwnerComms = (*client.Client)(nil)

func TestSayFilesWithoutThreadGoToUpdatesThread(t *testing.T) {
	s, fo, dest := boundProject(t)
	dot, _, _ := ReadDotfile(dest)
	dot.UpdatesThread = "Screenshot & progress update"
	if err := WriteDotfile(dest, dot); err != nil {
		t.Fatal(err)
	}
	a := writeTemp(t, dest, "a.png")
	if _, err := s.Command(sayWords(t, `project say --path `+dest+` --file `+a+` "shot"`)); err != nil {
		t.Fatal(err)
	}
	if len(fo.created) != 1 || fo.created[0] != "Screenshot & progress update" || fo.sends[0].thread == "" {
		t.Fatalf("created=%v sends=%+v", fo.created, fo.sends)
	}
	if !strings.Contains(BlockText("telegram", "A", UpdatesThreadFor(dest)), `"Screenshot & progress update"`) {
		t.Fatal("block does not name the updates thread")
	}
}
