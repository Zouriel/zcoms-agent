// Package projects binds a folder to a project group: the .zcoms file, the
// per-project history database, the managed block in the repo's agent
// instructions, the morning standup, and the `project …` owner commands.
//
// The binding itself lives in agent.db and is written only by owner commands.
// Nothing inside the folder (.zcoms, the instruction block, chat history) can
// change which group a folder is bound to.
package projects

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// DotfileName is the hand-editable project file at the folder root.
const DotfileName = ".zcoms"

// Dotfile is the .zcoms content. It holds names only; platform ids stay in
// agent.db and the history database. Group is display only.
type Dotfile struct {
	Group       string `toml:"group"`
	FollowTasks bool   `toml:"follow_tasks"`
	// UpdatesThread is the one Telegram topic coding sessions post screenshots
	// and progress in ("" means DefaultUpdatesThread).
	UpdatesThread string      `toml:"updates_thread,omitempty"`
	Members       []DotMember `toml:"member"`
}

// DefaultUpdatesThread names the updates topic when .zcoms sets none.
const DefaultUpdatesThread = "Misc"

// UpdatesThreadFor returns the project's updates topic name from .zcoms.
func UpdatesThreadFor(folder string) string {
	if d, ok, err := ReadDotfile(folder); err == nil && ok && strings.TrimSpace(d.UpdatesThread) != "" {
		return strings.TrimSpace(d.UpdatesThread)
	}
	return DefaultUpdatesThread
}

// DotMember is one [[member]] entry: a display name and the nicknames people
// use for them in the chat.
type DotMember struct {
	Name    string   `toml:"name"`
	Aliases []string `toml:"aliases"`
}

// ReadDotfile reads <dir>/.zcoms. A missing file returns ok=false and no error.
func ReadDotfile(dir string) (Dotfile, bool, error) {
	var d Dotfile
	b, err := os.ReadFile(filepath.Join(dir, DotfileName))
	if errors.Is(err, fs.ErrNotExist) {
		return d, false, nil
	}
	if err != nil {
		return d, false, err
	}
	if _, err := toml.Decode(string(b), &d); err != nil {
		return d, true, fmt.Errorf("reading %s: %w", DotfileName, err)
	}
	return d, true, nil
}

// WriteDotfile writes <dir>/.zcoms.
func WriteDotfile(dir string, d Dotfile) error {
	var buf bytes.Buffer
	buf.WriteString("# zcoms project file. Edit member aliases freely; the group binding\n")
	buf.WriteString("# itself lives in the agent and is not changed by this file.\n")
	if err := toml.NewEncoder(&buf).Encode(d); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, DotfileName), buf.Bytes(), 0o644)
}

// member returns the entry for name (case-insensitive), or nil.
func (d *Dotfile) member(name string) *DotMember {
	for i := range d.Members {
		if strings.EqualFold(d.Members[i].Name, name) {
			return &d.Members[i]
		}
	}
	return nil
}

// ensureMember adds name with no aliases when it is not listed yet, reporting
// whether it did.
func (d *Dotfile) ensureMember(name string) bool {
	if name == "" || d.member(name) != nil {
		return false
	}
	d.Members = append(d.Members, DotMember{Name: name})
	return true
}

// AddAlias adds alias to member, returning an error when the member is not
// listed or the alias is taken by someone else.
func (d *Dotfile) AddAlias(member, alias string) error {
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return errors.New("alias is empty")
	}
	m := d.member(member)
	if m == nil {
		return fmt.Errorf("no member named %q in %s", member, DotfileName)
	}
	for _, o := range d.Members {
		if strings.EqualFold(o.Name, alias) && !strings.EqualFold(o.Name, m.Name) {
			return fmt.Errorf("%q is already a member's name", alias)
		}
		for _, a := range o.Aliases {
			if strings.EqualFold(a, alias) {
				if strings.EqualFold(o.Name, m.Name) {
					return nil
				}
				return fmt.Errorf("%q is already an alias of %s", alias, o.Name)
			}
		}
	}
	m.Aliases = append(m.Aliases, alias)
	return nil
}

// RemoveAlias drops alias from member.
func (d *Dotfile) RemoveAlias(member, alias string) error {
	m := d.member(member)
	if m == nil {
		return fmt.Errorf("no member named %q in %s", member, DotfileName)
	}
	for i, a := range m.Aliases {
		if strings.EqualFold(a, alias) {
			m.Aliases = append(m.Aliases[:i], m.Aliases[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("%s has no alias %q", m.Name, alias)
}

// MatchMember resolves a name or nickname to a member name. It matches the
// member's name or one of its aliases exactly (ignoring case and a leading @).
// Anything else, or a nickname two members share, is unmatched: never guessed.
func (d *Dotfile) MatchMember(who string) (string, bool) {
	who = strings.TrimPrefix(strings.TrimSpace(who), "@")
	if who == "" {
		return "", false
	}
	var found []string
	for _, m := range d.Members {
		hit := strings.EqualFold(m.Name, who)
		for _, a := range m.Aliases {
			hit = hit || strings.EqualFold(a, who)
		}
		if hit {
			found = append(found, m.Name)
		}
	}
	if len(found) != 1 {
		return "", false
	}
	return found[0], true
}
