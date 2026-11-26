// Package cmdline splits and parses the command lines the zc CLI forwards to
// agent.sock for `group …` and `project …`. Values with spaces arrive
// shell-quoted, so splitting is quote-aware.
package cmdline

import (
	"errors"
	"fmt"
	"strings"
)

// Split breaks a command line into words. Single quotes keep everything
// literally; double quotes allow \" and \\ escapes; a backslash outside quotes
// escapes the next character.
func Split(s string) ([]string, error) {
	var (
		out    []string
		cur    strings.Builder
		inWord bool
		quote  rune
	)
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case quote == '"':
			switch {
			case r == '"':
				quote = 0
			case r == '\\' && i+1 < len(rs) && (rs[i+1] == '"' || rs[i+1] == '\\' || rs[i+1] == '$' || rs[i+1] == '`'):
				i++
				cur.WriteRune(rs[i])
			default:
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == '\\':
			if i+1 < len(rs) {
				i++
				cur.WriteRune(rs[i])
				inWord = true
			}
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote in command")
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out, nil
}

// Args is a parsed command: --name value flags, bare --switch flags and the
// remaining positional words in order.
type Args struct {
	Flags map[string]string
	Pos   []string
	// All holds every value of each flag in order, for flags that may be
	// repeated (Flags keeps the last).
	All map[string][]string
}

// Parse separates flags from positional words. switches names the flags that
// take no value. "--" ends flag parsing.
func Parse(words []string, switches ...string) (Args, error) {
	sw := make(map[string]bool, len(switches))
	for _, s := range switches {
		sw[s] = true
	}
	a := Args{Flags: map[string]string{}, All: map[string][]string{}}
	for i := 0; i < len(words); i++ {
		w := words[i]
		if w == "--" {
			a.Pos = append(a.Pos, words[i+1:]...)
			break
		}
		if !strings.HasPrefix(w, "--") || len(w) == 2 {
			a.Pos = append(a.Pos, w)
			continue
		}
		name := strings.TrimPrefix(w, "--")
		if k, v, ok := strings.Cut(name, "="); ok {
			a.Flags[k] = v
			a.All[k] = append(a.All[k], v)
			continue
		}
		if sw[name] {
			a.Flags[name] = "true"
			continue
		}
		if i+1 >= len(words) {
			return a, fmt.Errorf("--%s needs a value", name)
		}
		a.Flags[name] = words[i+1]
		a.All[name] = append(a.All[name], words[i+1])
		i++
	}
	return a, nil
}

// Has reports whether a flag was given.
func (a Args) Has(name string) bool { _, ok := a.Flags[name]; return ok }
