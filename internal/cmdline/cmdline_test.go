package cmdline

import (
	"reflect"
	"testing"
)

func TestSplit(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`project init --path /a/b --group 'Acme build'`, []string{"project", "init", "--path", "/a/b", "--group", "Acme build"}},
		{`project say --path /x "it's \"done\" now"`, []string{"project", "say", "--path", "/x", `it's "done" now`}},
		{`group on telegram My\ Group`, []string{"group", "on", "telegram", "My Group"}},
		{`a '' b`, []string{"a", "", "b"}},
		{`  spaced   out  `, []string{"spaced", "out"}},
	}
	for _, c := range cases {
		got, err := Split(c.in)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("Split(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	if _, err := Split(`say "open`); err == nil {
		t.Error("unterminated quote accepted")
	}
}

func TestParse(t *testing.T) {
	a, err := Parse([]string{"--path", "/p", "--create-agents-md", "alias", "--follow=on", "rm"}, "create-agents-md")
	if err != nil {
		t.Fatal(err)
	}
	if a.Flags["path"] != "/p" || !a.Has("create-agents-md") || a.Flags["follow"] != "on" {
		t.Fatalf("flags = %v", a.Flags)
	}
	if !reflect.DeepEqual(a.Pos, []string{"alias", "rm"}) {
		t.Fatalf("pos = %v", a.Pos)
	}
}

func TestParseRepeatedFlags(t *testing.T) {
	a, err := Parse([]string{"--file", "/a.png", "--file=/b.png", "--thread", "ui", "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if got := a.All["file"]; len(got) != 2 || got[0] != "/a.png" || got[1] != "/b.png" {
		t.Fatalf("All[file] = %v", got)
	}
	if a.Flags["file"] != "/b.png" || a.Flags["thread"] != "ui" || len(a.Pos) != 1 {
		t.Fatalf("args = %+v", a)
	}
}
