//go:build !windows

package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeAudio(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("OggS fake opus"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The transcription run reads only its scratch dir, holding a copy of the
// audio, denies every acting tool, carries no third-party text and is
// marked as a group call.
func TestTranscribeAudioAgyArgs(t *testing.T) {
	argsFile, envFile := fakeAgy(t, `{"conversation_id":"c","status":"SUCCESS","response":"TRANSCRIPT: salaam, kihineh?\nsecond line"}`)
	audio := writeAudio(t, "11016339456-voice_AgAD8iUAAj7_QFQ.ogg")
	scratch := t.TempDir()
	got, err := TranscribeAudio("gemini-3.8-flash-low", "SEED LINE", scratch, audio)
	if err != nil || got != "salaam, kihineh?\nsecond line" {
		t.Fatalf("TranscribeAudio = %q, %v", got, err)
	}
	args := readLines(t, argsFile)
	joined := strings.Join(args, "\n")
	for _, want := range []string{"--model\ngemini-3.8-flash-low", "--disable-slash-commands", "--mode\naccept-edits", "--output-format\njson"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv missing %q: %v", want, args)
		}
	}
	if has(args, "--dangerously-skip-permissions") {
		t.Fatal("transcription skips permissions")
	}
	p, ok := settingsOf(t, args)
	if !ok {
		t.Fatal("no settings")
	}
	for _, deny := range []string{agyAllCommands, agyAllWrites, agyAllMCP, agyAllBrowser} {
		if !has(p.Deny, deny) {
			t.Errorf("not denied: %s (%v)", deny, p.Deny)
		}
	}
	if len(p.Allow) != 1 || !strings.HasPrefix(p.Allow[0], "read_file(") {
		t.Fatalf("allow = %v, want one read_file rule", p.Allow)
	}
	dir := strings.TrimSuffix(strings.TrimPrefix(p.Allow[0], "read_file("), ")")
	realScratch, _ := filepath.EvalSymlinks(scratch)
	if filepath.Dir(dir) != realScratch {
		t.Fatalf("read dir %q is not a fresh dir under %q", dir, realScratch)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("scratch dir was not removed")
	}
	for _, d := range p.Deny {
		if strings.HasPrefix(d, "read_file(") && pathWithin(dir, strings.TrimSuffix(strings.TrimPrefix(d, "read_file("), ")")) {
			t.Fatalf("deny %s would block the workspace", d)
		}
	}
	// The fake records one line per argv line; the prompt is the multi-line tail.
	prompt := joined[strings.Index(joined, "--print="):]
	if !strings.HasPrefix(prompt, "--print=SEED LINE\n\n") || !strings.Contains(prompt, "voice.ogg") ||
		!strings.Contains(prompt, "TRANSCRIPT: <the words spoken>") || strings.Contains(prompt, audio) {
		t.Fatalf("prompt = %q", prompt)
	}
	if !strings.Contains(strings.Join(readLines(t, envFile), "\n"), GroupCallEnv+"=1") {
		t.Error("group-call marker missing from env")
	}
}

func TestTranscribeAudioFailures(t *testing.T) {
	audio := writeAudio(t, "v.ogg")
	fakeAgy(t, `{"conversation_id":"","status":"ERROR","response":"","error":"authentication failed or timed out"}`)
	if _, err := TranscribeAudio("", "", t.TempDir(), audio); err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("unauthenticated agy: err = %v", err)
	}
	fakeAgy(t, `{"conversation_id":"c","status":"SUCCESS","response":"Sure, I can help with that."}`)
	if _, err := TranscribeAudio("", "", t.TempDir(), audio); err == nil {
		t.Fatal("a reply without TRANSCRIPT was accepted")
	}
	old := agyBin
	agyBin = filepath.Join(t.TempDir(), "no-agy")
	defer func() { agyBin = old }()
	if _, err := TranscribeAudio("", "", t.TempDir(), audio); err != ErrAgyMissing {
		t.Fatalf("missing agy: err = %v", err)
	}
}
