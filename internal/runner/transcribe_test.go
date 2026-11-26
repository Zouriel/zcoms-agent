package runner

import (
	"strings"
	"testing"
)

func TestParseTranscript(t *testing.T) {
	if got, ok := ParseTranscript("TRANSCRIPT: hello\nworld"); !ok || got != "hello\nworld" {
		t.Fatalf("got %q, %v", got, ok)
	}
	if got, ok := ParseTranscript("Here it is:\n**TRANSCRIPT**: ހެލޯ"); !ok || got != "ހެލޯ" {
		t.Fatalf("got %q, %v", got, ok)
	}
	thinking := "<the words spoken>\"\n\nWait, can we write:\nTRANSCRIPT: ހެލޯ\n\nThe instruction:\nTRANSCRIPT: <the words spoken>\"\n\nLet's output exactly that.TRANSCRIPT: ހެލޯ ދުނިޔެ"
	if got, ok := ParseTranscript(thinking); !ok || got != "ހެލޯ ދުނިޔެ" {
		t.Fatalf("thinking aloud: got %q, %v", got, ok)
	}
	for _, bad := range []string{"", "hello", "TRANSCRIPT:   ", "TRANSCRIPT: <the words spoken>", "the transcript is below"} {
		if _, ok := ParseTranscript(bad); ok {
			t.Errorf("ParseTranscript(%q) accepted", bad)
		}
	}
	long, ok := ParseTranscript("TRANSCRIPT: " + strings.Repeat("ab ", 3000))
	if !ok || len([]rune(long)) > MaxTranscript+3 {
		t.Fatalf("not capped: %d runes", len([]rune(long)))
	}
}

func TestAudioExt(t *testing.T) {
	for in, want := range map[string]string{"/a/b.OGG": ".ogg", "/a/b.opus": ".opus", "/a/b": "", "/a/b.o g": "", "/a/b.verylongext": ""} {
		if got := audioExt(in); got != want {
			t.Errorf("audioExt(%q) = %q, want %q", in, got, want)
		}
	}
}
