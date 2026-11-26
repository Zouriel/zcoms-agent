package projects

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ignoreLine is what init appends to a repo's .gitignore.
const ignoreLine = HistoryDir + "/"

// skipDirs are never walked when looking for repos or instruction files.
var skipDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true, HistoryDir: true}

func gitAvailable() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// EnclosingRepo returns the top level of the git repo that contains dir, or ""
// when dir is not inside one.
func EnclosingRepo(dir string) string {
	if !gitAvailable() {
		return ""
	}
	top, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return filepath.Clean(top)
}

// reposInside lists git repos under dir (dir itself included when it is one).
func reposInside(dir string) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if _, serr := os.Stat(filepath.Join(p, ".git")); serr == nil {
			out = append(out, p)
		}
		if p != dir && skipDirs[d.Name()] {
			return filepath.SkipDir
		}
		return nil
	})
	return out
}

// ReposFor returns every repo that could pick up the history folder: the
// enclosing repo plus every repo inside folder, without duplicates.
func ReposFor(folder string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(r string) {
		if r != "" && !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	add(EnclosingRepo(folder))
	for _, r := range reposInside(folder) {
		add(filepath.Clean(r))
	}
	return out
}

// hasIgnoreLine reports whether a .gitignore already ignores the history
// folder by one of the obvious spellings.
func hasIgnoreLine(content string) bool {
	for _, l := range strings.Split(content, "\n") {
		switch strings.TrimSpace(l) {
		case ".zcoms.d", ".zcoms.d/", "/.zcoms.d", "/.zcoms.d/", "**/.zcoms.d", "**/.zcoms.d/":
			return true
		}
	}
	return false
}

// EnsureIgnored appends ".zcoms.d/" to repo/.gitignore unless the repo already
// ignores it, creating the file when missing. It reports whether it changed
// the file. folder is the project folder (used to ask git whether the history
// path is already ignored by some other rule).
func EnsureIgnored(repo, folder string) (bool, error) {
	path := filepath.Join(repo, ".gitignore")
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	content := string(b)
	if hasIgnoreLine(content) {
		return false, nil
	}
	if gitAvailable() {
		probe := filepath.Join(folder, HistoryDir, "history.db")
		if within(probe, repo) {
			if _, err := git(repo, "check-ignore", "-q", "--no-index", probe); err == nil {
				return false, nil
			}
		}
	}
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += ignoreLine + "\n"
	return true, os.WriteFile(path, []byte(content), 0o644)
}

// within reports whether p is root or below it.
func within(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// GuardUntracked refuses when the history database is tracked by git in the
// enclosing repo: writing it would leak chat history into commits.
func GuardUntracked(folder string) error {
	repo := EnclosingRepo(folder)
	if repo == "" {
		return nil
	}
	db := HistoryPath(folder)
	if _, err := git(repo, "ls-files", "--error-unmatch", db); err == nil {
		rel, _ := filepath.Rel(repo, db)
		return fmt.Errorf("%s is tracked by git, so I will not write project history; run `git rm --cached %s` and commit, then try again", rel, rel)
	}
	return nil
}
