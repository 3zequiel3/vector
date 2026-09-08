// Package gitx asks git what actually changed.
//
// Git is the only source of truth Vector trusts about the working tree. It
// cannot be prompted, it cannot be persuaded, and it does not depend on any
// hook having fired.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
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
	diffArgs := []string{"diff", "--name-only", "--no-renames", "-z", ref}
	if base == "" && !hasHEAD(dir) {
		diffArgs = []string{"diff", "--name-only", "--no-renames", "-z", "--cached"}
	}
	if out, err := run(dir, diffArgs...); err == nil {
		addPaths(set, out)
	} else {
		return nil, fmt.Errorf("git diff: %w", err)
	}

	// Untracked but not ignored.
	if out, err := run(dir, "ls-files", "--others", "--exclude-standard", "-z"); err == nil {
		addPaths(set, out)
	}

	files := make([]string, 0, len(set))
	for f := range set {
		files = append(files, f)
	}
	sort.Strings(files)
	return files, nil
}

// Numstat returns, per repo-relative path, how many lines a change added and
// removed: [0] is added, [1] is removed.
//
// It answers a question ChangedFiles cannot — that one says which files moved,
// this one says which direction. The base follows the same rule, and the same
// no-HEAD fallback, so the two describe the same change.
//
// Untracked files are deliberately absent. git has nothing to diff a new file
// against, and a file that did not exist before cannot have had anything
// removed from it, so the only caller that needs this is asking about something
// untracked files cannot be.
//
// A binary file's counts are reported by git as "-", and are recorded as zero:
// the number of lines in a binary is not a fact, and inventing one to fill the
// column would put a fabricated measurement into a verdict.
//
// Two flags here are load-bearing, and both were chosen against a reproduction.
//
// Renames are detected, unlike in ChangedFiles, because the two answer opposite
// questions. To the audit a rename is a change to two paths and both must be in
// bounds. To a caller asking what a change removed, a rename is nothing at all:
// with renames off, `git mv a_test.go b_test.go` reports ten lines deleted from
// a file that moved byte for byte, and a tool that calls that "the suite that
// passed is not the suite that was there" is one nobody keeps installed.
//
// -z is what makes the paths trustworthy. Under git's default core.quotePath a
// path with a non-ASCII byte comes back C-quoted and octal-escaped —
// "unicode/t\303\253st_test.go" — and a caller matching on the end of that
// string is matching on a trailing quote. -z emits raw bytes and NUL
// separators, so nothing has to be unquoted and a path may contain anything but
// NUL, including tabs and newlines.
func Numstat(dir, base string) (map[string][2]int, error) {
	ref := base
	if ref == "" {
		ref = "HEAD"
	}
	args := []string{"diff", "--numstat", "-z", ref}
	if base == "" && !hasHEAD(dir) {
		args = []string{"diff", "--numstat", "-z", "--cached"}
	}
	out, err := run(dir, args...)
	if err != nil {
		return nil, fmt.Errorf("git diff --numstat: %w", err)
	}

	// Under -z a plain entry is one record, "added\tdeleted\tpath". A rename
	// is three: "added\tdeleted\t" with the path field empty, then the old
	// path, then the new one. The new path is the file that exists now, and
	// the one a later question about it will be asked under.
	stats := map[string][2]int{}
	recs := strings.Split(out, "\x00")
	for i := 0; i < len(recs); i++ {
		fields := strings.SplitN(recs[i], "\t", 3)
		if len(fields) < 3 {
			continue
		}
		path := fields[2]
		if path == "" {
			if i+2 >= len(recs) {
				continue
			}
			path = recs[i+2]
			i += 2
		}
		if path == "" {
			continue
		}
		stats[filepath.ToSlash(path)] = [2]int{
			count(fields[0]), count(fields[1]),
		}
	}
	return stats, nil
}

// count parses one numstat column, treating git's "-" for binary content as
// zero rather than as an error: a binary file in the diff is not a reason to
// refuse to report the text files beside it.
func count(field string) int {
	n, err := strconv.Atoi(strings.TrimSpace(field))
	if err != nil {
		return 0
	}
	return n
}

// AllFiles lists every file in the repository that git would consider part of
// it: tracked files plus untracked ones that are not ignored.
//
// Tracked files alone are not enough. A repository with no commits yet has none
// of them, and answering "no files here" would turn every path question into a
// misleading negative.
func AllFiles(dir string) ([]string, error) {
	set := map[string]struct{}{}
	out, err := run(dir, "ls-files", "-z")
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	addPaths(set, out)
	if out, err := run(dir, "ls-files", "--others", "--exclude-standard", "-z"); err == nil {
		addPaths(set, out)
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

// addPaths collects NUL-separated paths.
//
// Nothing is trimmed. Under -z each record is already exactly the path, and a
// filename may legitimately begin or end with a space — trimming would quietly
// rename it into one that matches nothing.
func addPaths(set map[string]struct{}, out string) {
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			set[filepath.ToSlash(p)] = struct{}{}
		}
	}
}

// gitTimeout bounds every git call.
//
// The hook runs once per tool call and every event of it reaches git. With no
// bound, a repository on an unresponsive network mount, or a git that wedges
// for any of the ordinary reasons git wedges, stops the agent's tool call
// forever — and the package that made a point of not hanging on a test suite
// was hanging with no limit in the one place it runs most.
//
// Thirty seconds is not a performance budget. Every call here is name-only or
// numstat and returns in milliseconds on any repository anyone works in; this
// is the line past which the answer is "git is not coming back" rather than
// "git is slow", and vector would rather say nothing than hold the session.
const gitTimeout = 30 * time.Second

func run(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("git %s did not return within %s", args[0], gitTimeout)
		}
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
