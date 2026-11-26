package projects

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Zouriel/zcoms-agent/internal/store"
	"github.com/Zouriel/zcoms/client"
)

// `group dir <transport> <group> <path> [--create]` gives a group without a
// folder one (binding it there like project init does) or moves a bound
// group's .zcoms and .zcoms.d to another folder. Owner only: callers
// guarantee the line came from agent.sock. Every check on the path happens
// here, in code, whatever the console already checked.

// undoStack collects the steps that reverse a half-done folder change. run
// replays them newest first and returns what could not be undone.
type undoStack []func() error

func (u *undoStack) push(f func() error) { *u = append(*u, f) }

func (u undoStack) run() []string {
	var errs []string
	for i := len(u) - 1; i >= 0; i-- {
		if err := u[i](); err != nil {
			errs = append(errs, err.Error())
		}
	}
	return errs
}

// Dir runs `group dir`. path may start with ~; create makes the folder (and
// missing parents) when it does not exist.
func (s *Service) Dir(tp, group, path string, create bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.Owner.GroupGet(tp, group)
	if err != nil {
		return "", err
	}
	if g == nil {
		return "", fmt.Errorf("no %s group called %q", tp, group)
	}
	dest, err := cleanDest(path)
	if err != nil {
		return "", err
	}
	p, bound, err := s.Store.ProjectForGroup(tp, g.NativeID)
	if err != nil {
		return "", err
	}
	var ownID int64
	if bound {
		if filepath.Clean(p.Path) == dest {
			return "", fmt.Errorf("%s is already the folder of %s", dest, g.Name)
		}
		ownID = p.ID
	}
	// An unbound group may come back to a folder it left: unbinding keeps
	// .zcoms and the history, so those are adopted instead of refused.
	adopt := !bound && s.leftoversOf(tp, g, dest)
	if err := s.checkDest(tp, g.NativeID, ownID, dest, create, adopt); err != nil {
		return "", err
	}
	if bound {
		return s.moveFolder(p, dest, create)
	}
	return s.addFolder(tp, g, dest, create, adopt)
}

// cleanDest expands ~ and requires an absolute path.
func cleanDest(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("a folder path is required")
	}
	if strings.ContainsRune(path, 0) {
		return "", errors.New("bad folder path")
	}
	path = expandHome(path)
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s is not an absolute path", path)
	}
	return filepath.Clean(path), nil
}

// configDirs are the folders a group folder may never be in: the agent's own
// state, under ~/.config/zcoms (and the platform config dir when different).
func configDirs() []string {
	var out []string
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(home, ".config", "zcoms"))
	}
	if cfg, err := os.UserConfigDir(); err == nil {
		out = append(out, filepath.Join(cfg, "zcoms"))
	}
	var all []string
	for _, d := range out {
		all = append(all, d)
		if r, err := filepath.EvalSymlinks(d); err == nil && r != d {
			all = append(all, r)
		}
	}
	return all
}

// resolved returns dest with symlinks resolved as far as the path exists, so
// a link cannot slip a folder past the overlap and config checks.
func resolved(dest string) string {
	rest := ""
	for dir := dest; ; {
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dest
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
}

// checkDest validates a destination folder for a group. ownID is the group's
// current project (0 when it has none), left out of the overlap checks.
func (s *Service) checkDest(tp, nativeID string, ownID int64, dest string, create, adopt bool) error {
	destReal := resolved(dest)
	for _, cfg := range configDirs() {
		if within(dest, cfg) || within(destReal, cfg) {
			return fmt.Errorf("%s is inside the agent's config folder %s", dest, cfg)
		}
	}
	st, err := os.Stat(dest)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if !create {
			return fmt.Errorf("%s does not exist (tick create to make it)", dest)
		}
	case err != nil:
		return err
	case !st.IsDir():
		return fmt.Errorf("%s is not a folder", dest)
	}
	all, err := s.Store.ListProjects()
	if err != nil {
		return err
	}
	for _, p := range all {
		if p.ID == ownID {
			continue
		}
		root := filepath.Clean(p.Path)
		rootReal := resolved(root)
		switch {
		case dest == root || destReal == rootReal:
			return fmt.Errorf("%s is already the folder of the group %q", dest, p.GroupName)
		case within(dest, root) || within(destReal, rootReal):
			return fmt.Errorf("%s is inside %s, the folder of the group %q", dest, root, p.GroupName)
		case within(root, dest) || within(rootReal, destReal):
			return fmt.Errorf("%s contains %s, the folder of the group %q", dest, root, p.GroupName)
		}
	}
	folders, err := s.Store.ListGroupFolders()
	if err != nil {
		return err
	}
	for _, f := range folders {
		if f.Transport == tp && f.NativeID == nativeID {
			continue
		}
		if filepath.Clean(f.Path) == dest {
			return fmt.Errorf("%s is kept as another group's folder", dest)
		}
	}
	if adopt {
		return nil
	}
	for _, name := range []string{DotfileName, HistoryDir} {
		if _, err := os.Lstat(filepath.Join(dest, name)); err == nil {
			return fmt.Errorf("%s already has a %s from another group; pick a folder without one", dest, name)
		}
	}
	return nil
}

// leftoversOf reports whether dest holds .zcoms or .zcoms.d left behind by
// this same group: the folder is the one recorded for the group, or the
// .zcoms names the group. Leftovers of any other group are never adopted.
func (s *Service) leftoversOf(tp string, g *client.Group, dest string) bool {
	has := false
	for _, name := range []string{DotfileName, HistoryDir} {
		if _, err := os.Lstat(filepath.Join(dest, name)); err == nil {
			has = true
		}
	}
	if !has {
		return false
	}
	if folder, known, err := s.Store.GroupFolder(tp, g.NativeID); err == nil && known && filepath.Clean(folder) == dest {
		return true
	}
	dot, ok, err := ReadDotfile(dest)
	return err == nil && ok && dot.Group == g.Name
}

// step fails the named step when a test asks it to.
func (s *Service) step(name string) error {
	if s.failAt != nil {
		return s.failAt(name)
	}
	return nil
}

// makeDir creates dest and any missing parents, pushing their removal.
func makeDir(dest string, undo *undoStack) error {
	var missing []string
	for dir := dest; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(dir); err == nil {
			break
		}
		missing = append(missing, dir)
		if filepath.Dir(dir) == dir {
			break
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dest, err)
	}
	undo.push(func() error {
		for _, d := range missing { // deepest first; only ever removes empty folders
			if err := os.Remove(d); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
		return nil
	})
	return nil
}

// snapshot remembers files' contents (nil for a missing file) so undo can
// put them back.
func snapshot(files []string, undo *undoStack) {
	saved := make(map[string][]byte, len(files))
	for _, f := range files {
		if _, ok := saved[f]; ok {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			b = nil
		}
		saved[f] = b
	}
	undo.push(func() error {
		var errs []string
		for f, b := range saved {
			cur, err := os.ReadFile(f)
			if err == nil && b != nil && bytes.Equal(cur, b) {
				continue
			}
			if b == nil {
				if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
					errs = append(errs, err.Error())
				}
				continue
			}
			if err := os.WriteFile(f, b, 0o644); err != nil {
				errs = append(errs, err.Error())
			}
		}
		if len(errs) > 0 {
			return errors.New(strings.Join(errs, "; "))
		}
		return nil
	})
}

// ignoreIn gitignores .zcoms.d in every repo that could pick up folder.
func ignoreIn(folder string, undo *undoStack) error {
	repos := ReposFor(folder)
	files := make([]string, 0, len(repos))
	for _, r := range repos {
		files = append(files, filepath.Join(r, ".gitignore"))
	}
	snapshot(files, undo)
	for _, repo := range repos {
		if _, err := EnsureIgnored(repo, folder); err != nil {
			return fmt.Errorf("updating %s/.gitignore: %w", repo, err)
		}
	}
	return nil
}

// checkpoint folds a history database's WAL back into the main file, so the
// move carries one consistent file. A folder with no database is fine.
func checkpoint(folder string) error {
	h, err := openExisting(folder)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer h.Close()
	if _, err := h.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpointing %s: %w", HistoryPath(folder), err)
	}
	return nil
}

// moveEntry renames src to dst, falling back to copy, verify and remove when
// they are on different filesystems.
func (s *Service) moveEntry(src, dst string) error {
	rename := os.Rename
	if s.rename != nil {
		rename = s.rename
	}
	err := rename(src, dst)
	if err == nil {
		return nil
	}
	if !crossDevice(err) {
		return err
	}
	if err := copyTree(src, dst); err != nil {
		_ = os.RemoveAll(dst)
		return fmt.Errorf("copying %s to %s: %w", src, dst, err)
	}
	if err := sameTree(src, dst); err != nil {
		_ = os.RemoveAll(dst)
		return fmt.Errorf("verifying the copy of %s: %w", src, err)
	}
	if err := os.RemoveAll(src); err != nil {
		s.logf("group dir: copied %s but could not remove it: %v", src, err)
	}
	return nil
}

// crossDevice reports whether a rename failed because the two paths are on
// different filesystems (EXDEV; ERROR_NOT_SAME_DEVICE on Windows).
func crossDevice(err error) bool {
	if errors.Is(err, syscall.EXDEV) {
		return true
	}
	var errno syscall.Errno
	return errors.As(err, &errno) && errno == 17 && os.PathSeparator == '\\'
}

// copyTree copies a file or folder, keeping modes and symlinks.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			return copyFile(p, target, info.Mode().Perm())
		}
		return nil // sockets and the like are not ours
	})
}

func copyFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// sameTree checks every regular file under src has an identical copy under dst.
func sameTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		a, err := fileSum(p)
		if err != nil {
			return err
		}
		b, err := fileSum(filepath.Join(dst, rel))
		if err != nil {
			return err
		}
		if !bytes.Equal(a, b) {
			return fmt.Errorf("%s differs after copying", rel)
		}
		return nil
	})
}

func fileSum(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// moveIn moves old/name to dest/name when it exists, pushing the move back.
func (s *Service) moveIn(old, dest, name string, undo *undoStack) error {
	src, dst := filepath.Join(old, name), filepath.Join(dest, name)
	if _, err := os.Lstat(src); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err := s.moveEntry(src, dst); err != nil {
		return fmt.Errorf("moving %s: %w", src, err)
	}
	undo.push(func() error { return s.moveEntry(dst, src) })
	return nil
}

// workspaceFor returns the workspace row for folder, registering it first
// when discovery has not.
func (s *Service) workspaceFor(folder string) (store.Workspace, error) {
	ws, ok, err := s.Store.GetWorkspaceByPath(folder)
	if err != nil || ok {
		return ws, err
	}
	if err := s.Store.UpsertDiscovered(folder, filepath.Base(folder)); err != nil {
		return ws, err
	}
	ws, _, err = s.Store.GetWorkspaceByPath(folder)
	return ws, err
}

// rolledBack turns a failed step into the reply error after undoing.
func rolledBack(what, keep string, err error, undo undoStack) error {
	msg := fmt.Sprintf("%s failed: %v. Rolled back", what, err)
	if keep != "" {
		msg += "; " + keep + " is still the folder"
	}
	if errs := undo.run(); len(errs) > 0 {
		msg += ". Some steps could not be undone: " + strings.Join(errs, "; ")
	}
	return errors.New(msg)
}

// moveFolder moves a bound group's .zcoms and .zcoms.d from its folder to
// dest and repoints the binding. Any failure puts everything back.
func (s *Service) moveFolder(p store.Project, dest string, create bool) (string, error) {
	old := filepath.Clean(p.Path)
	// Nothing in this process may hold the database while it moves: the
	// standup and every other write path hold s.mu (held by the caller), the
	// readers outside it take dirMu.
	s.dirMu.Lock()
	defer s.dirMu.Unlock()

	var undo undoStack
	fail := func(err error) (string, error) {
		return "", rolledBack("Moving the folder of "+p.GroupName, old, err, undo)
	}
	if err := checkpoint(old); err != nil {
		return fail(err)
	}
	if create {
		if err := makeDir(dest, &undo); err != nil {
			return fail(err)
		}
	}
	if err := ignoreIn(dest, &undo); err != nil {
		return fail(err)
	}
	if err := GuardUntracked(dest); err != nil {
		return fail(err)
	}
	if err := s.step("move"); err != nil {
		return fail(err)
	}
	if err := s.moveIn(old, dest, HistoryDir, &undo); err != nil {
		return fail(err)
	}
	if err := s.moveIn(old, dest, DotfileName, &undo); err != nil {
		return fail(err)
	}

	// The managed block leaves the old folder's instruction files and goes
	// into the new folder's, where there are any (never creating one).
	snapshot(append(FindInstructionFiles(old), FindInstructionFiles(dest)...), &undo)
	if _, err := StripBlocks(old); err != nil {
		return fail(err)
	}
	blocks, err := WriteBlocks(dest, p.Transport, p.GroupName, false)
	if err != nil {
		return fail(err)
	}

	ws, err := s.workspaceFor(dest)
	if err != nil {
		return fail(err)
	}
	if err := s.step("commit"); err != nil {
		return fail(err)
	}
	if err := s.Store.MoveProject(store.Owner, p.ID, ws.ID, p.Transport, p.NativeID, dest); err != nil {
		return fail(err)
	}

	p.WorkspaceID, p.Path, p.MaxRole = ws.ID, dest, ws.MaxRole
	if s.OnFollow != nil {
		s.OnFollow(p)
	}
	msg := fmt.Sprintf("Moved the folder of %s from %s to %s (%s now).", p.GroupName, old, dest, KindOf(p))
	if len(blocks) > 0 {
		msg += "\n  " + strings.Join(blocks, "\n  ")
	}
	return msg, nil
}

// addFolder binds a group that has no folder at dest, the way project init
// does: .zcoms, .zcoms.d/history.db, .gitignore in every repo that could pick
// it up, the managed block where instruction files exist, the workspace, and
// listening on. Follow tasks stays off. Any failure puts everything back.
func (s *Service) addFolder(tp string, g *client.Group, dest string, create, adopt bool) (string, error) {
	pol, has, err := s.Store.GetGroupPolicy(tp, g.NativeID)
	if err != nil {
		return "", err
	}
	if has && pol.Kind == store.GroupProject {
		return "", fmt.Errorf("%s is already bound to a folder", g.Name)
	}
	restore := has && pol.Kind == store.GroupGeneral

	var undo undoStack
	fail := func(err error) (string, error) {
		return "", rolledBack("Adding a folder to "+g.Name, "", err, undo)
	}
	if create {
		if err := makeDir(dest, &undo); err != nil {
			return fail(err)
		}
	}
	if err := ignoreIn(dest, &undo); err != nil {
		return fail(err)
	}
	if err := GuardUntracked(dest); err != nil {
		return fail(err)
	}
	ws, err := s.workspaceFor(dest)
	if err != nil {
		return fail(err)
	}

	if !adopt {
		undo.push(func() error { return os.RemoveAll(filepath.Join(dest, HistoryDir)) })
	}
	h, err := OpenHistory(dest)
	if err != nil {
		return fail(err)
	}
	var notes []string
	dot := Dotfile{Group: g.Name}
	var oldDot []byte
	if adopt {
		// Keep the members and aliases the owner already set up.
		if prev, ok, err := ReadDotfile(dest); err == nil && ok {
			dot = prev
			dot.Group = g.Name
		}
		oldDot, _ = os.ReadFile(filepath.Join(dest, DotfileName))
		notes = append(notes, "reused the .zcoms and history already in "+dest)
	}
	if members, err := s.Owner.GroupMembers(tp, g.NativeID); err != nil {
		notes = append(notes, "warning: could not read the members yet ("+err.Error()+")")
	} else {
		if _, _, err := h.SyncMembers(tp, toMembers(members)); err != nil {
			h.Close()
			return fail(err)
		}
		for _, m := range toMembers(members) {
			dot.ensureMember(m.Name)
		}
	}
	if err := h.Close(); err != nil {
		return fail(err)
	}
	s.copyContactAliases(&dot)
	undo.push(func() error {
		if oldDot != nil {
			return os.WriteFile(filepath.Join(dest, DotfileName), oldDot, 0o640)
		}
		if err := os.Remove(filepath.Join(dest, DotfileName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	})
	if err := WriteDotfile(dest, dot); err != nil {
		return fail(err)
	}
	snapshot(FindInstructionFiles(dest), &undo)
	blocks, err := WriteBlocks(dest, tp, g.Name, false)
	if err != nil {
		return fail(err)
	}
	if err := s.step("commit"); err != nil {
		return fail(err)
	}

	prev, hadFolder, err := s.Store.GroupFolder(tp, g.NativeID)
	if err != nil {
		return fail(err)
	}
	p, err := s.Store.CreateProject(store.Owner, store.Project{
		WorkspaceID: ws.ID, Transport: tp, NativeID: g.NativeID, GroupName: g.Name, RestoreGeneral: restore,
	})
	if err != nil {
		return fail(err)
	}
	undo.push(func() error {
		if err := s.Store.DeleteProject(store.Owner, p.ID); err != nil {
			return err
		}
		if has {
			return s.Store.SetGroupPolicy(store.Owner, pol)
		}
		return nil
	})
	if err := s.Store.SetGroupFolder(store.Owner, tp, g.NativeID, dest); err != nil {
		return fail(err)
	}
	undo.push(func() error {
		if hadFolder {
			return s.Store.SetGroupFolder(store.Owner, tp, g.NativeID, prev)
		}
		return s.Store.DeleteGroupFolder(store.Owner, tp, g.NativeID)
	})
	if err := s.step("listen"); err != nil {
		return fail(err)
	}

	p.Path, p.MaxRole = ws.Path, ws.MaxRole
	if _, err := s.Owner.GroupSet(tp, g.NativeID, boolPtr(true), boolPtr(false)); err != nil {
		notes = append(notes, "warning: could not turn listening on in comms: "+err.Error())
	}
	if s.OnFollow != nil {
		s.OnFollow(p)
	}
	msg := fmt.Sprintf("Bound %s to the folder %s (%s). Follow tasks: off.", g.Name, dest, KindOf(p))
	if extra := append(blocks, notes...); len(extra) > 0 {
		msg += "\n  " + strings.Join(extra, "\n  ")
	}
	return msg, nil
}
