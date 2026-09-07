// Package gitx asks git what actually changed.
//
// Git is the only source of truth Vector trusts about the working tree. It
// cannot be prompted, it cannot be persuaded, and it does not depend on any
// hook having fired.
package gitx

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// ErrNotARepo reports that the given directory is not inside a git worktree.
var ErrNotARepo = errors.New("not inside a git repository")

// Root returns the absolute path of the repository containing dir.
func Root(dir string) (string, error) {
	out, err := run(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", ErrNotARepo
	}
	return filepath.Clean(strings.TrimSpace(out)), nil
}

// ChangedFiles returns every repo-relative path the working tree has touched,
// as forward-slash paths, sorted and deduplicated.
//
// When base is empty the comparison is against HEAD, which answers "what has
// this session done". A non-empty base compares against that ref instead,
// which is what a branch or CI review wants. Untracked files are included in
// both cases: a brand-new file outside the declared scope is exactly the kind
// of drift this tool exists to catch.
func ChangedFiles(dir, base string) ([]string, error) {
	ref := base
	if ref == "" {
		ref = "HEAD"
	}

	set := map[string]struct{}{}

	// Tracked modifications, staged and unstaged. On a repository with no
	// commits yet there is no HEAD to diff against, so tracked changes are
	// simply everything in the index.
	diffArgs := []string{"diff", "--name-only", "--no-renames", ref}
	if base == "" && !hasHEAD(dir) {
		diffArgs = []string{"diff", "--name-only", "--no-renames", "--cached"}
	}
	if out, err := run(dir, diffArgs...); err == nil {
		addLines(set, out)
	} else {
		return nil, fmt.Errorf("git diff: %w", err)
	}

	// Untracked but not ignored.
	if out, err := run(dir, "ls-files", "--others", "--exclude-standard"); err == nil {
		addLines(set, out)
	}

	files := make([]string, 0, len(set))
	for f := range set {
		files = append(files, f)
	}
	sort.Strings(files)
	return files, nil
}

// AllFiles lists every file in the repository that git would consider part of
// it: tracked files plus untracked ones that are not ignored.
//
// Tracked files alone are not enough. A repository with no commits yet has none
// of them, and answering "no files here" would turn every path question into a
// misleading negative.
func AllFiles(dir string) ([]string, error) {
	set := map[string]struct{}{}
	out, err := run(dir, "ls-files")
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	addLines(set, out)
	if out, err := run(dir, "ls-files", "--others", "--exclude-standard"); err == nil {
		addLines(set, out)
	}
	files := make([]string, 0, len(set))
	for f := range set {
		files = append(files, f)
	}
	sort.Strings(files)
	return files, nil
}

func hasHEAD(dir string) bool {
	_, err := run(dir, "rev-parse", "--verify", "HEAD")
	return err == nil
}

func addLines(set map[string]struct{}, out string) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			set[filepath.ToSlash(line)] = struct{}{}
		}
	}
}

func run(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
