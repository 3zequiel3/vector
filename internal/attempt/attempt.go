// Package attempt counts how often a task has been verified, and answers
// whether the work looks like it is going in circles.
//
// An agent that cannot solve a task does not stop — it retries, and each retry
// grows the diff. Vector never runs the agent, so it cannot count "attempts"
// directly. It counts what it already sees on disk: how often `vector verify`
// ran for a task, how many of those runs failed, how the size of the change
// moved between them, and how many times the boundary was widened to fit it.
//
// Nothing here blocks, denies, or changes an exit code. Vector cannot tell a
// productive fourth attempt from an unproductive one; a tool that halts
// legitimate work on a heuristic is a tool that gets uninstalled. This package
// produces a message addressed to whoever reads it, and nothing else.
package attempt

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/3zequiel3/vector/internal/gitx"
	"github.com/3zequiel3/vector/internal/scope"
)

const (
	// minFailures is where the signal starts.
	//
	// One failed verify is work. A second is the ordinary correction that most
	// real fixes land on, and firing there would mean firing on almost every
	// task. The third consecutive failure is the first that the two-try
	// pattern does not explain, and it is also the smallest number that can
	// show a direction rather than a step: two failures give one measurement
	// of the diff, three give a first and a latest with something in between.
	minFailures = 3

	// maxBytes is where the log gets compacted. The Stop hook reads this file
	// whole, once per turn, so the cap is set where that read stays trivial
	// rather than at a tidy number: 64 KiB is roughly nine hundred records,
	// which is several hundred verify runs across every task in the repo.
	maxBytes = 64 << 10

	// keepRecords is what compaction leaves behind. The judgement never looks
	// further back than the last passing run, and a task that has failed two
	// hundred times in a row is long past the point where one more record
	// changes the answer.
	keepRecords = 200
)

// Outcome is one verify run, as it happened.
type Outcome struct {
	At      time.Time
	Task    string
	Verdict string
	Files   int // files git called changed at that moment
	Lines   int // lines added plus removed across them
	Passed  bool
}

// Path returns the attempt log for a repository.
//
// It sits beside `current` and `nudged` as per-developer working state: two
// people on the same branch retry different things, and committing one
// person's retry count would report it as the other's.
func Path(root string) string {
	return filepath.Join(root, ".vector", "attempts")
}

// Record appends one verify outcome.
//
// The log is append-only. Two sessions verifying the same repository must not
// be able to damage each other's bookkeeping, and a short line written with
// O_APPEND lands whole. The only rewrite is compaction, and it is deliberately
// rare — see compact.
func Record(root string, o Outcome) error {
	if strings.TrimSpace(o.Task) == "" {
		return fmt.Errorf("an outcome with no task belongs to nothing")
	}
	if o.At.IsZero() {
		o.At = time.Now()
	}
	if err := os.MkdirAll(filepath.Join(root, ".vector"), 0o755); err != nil {
		return err
	}
	path := Path(root)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(o.line()); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	compact(path)
	return nil
}

// History returns the recorded outcomes for one task, oldest first.
//
// Every way of failing to read — the file is missing, unreadable, half a line
// long, full of numbers that are not numbers — returns what it can and calls
// the rest no history. A miscounted attempt costs a message nobody needed; an
// error escaping into verify or the Stop hook costs the user their turn.
func History(root, task string) []Outcome {
	want := key(task)
	if want == "" {
		return nil
	}
	var out []Outcome
	for _, line := range readLines(Path(root)) {
		o, ok := parse(line)
		if ok && o.Task == want {
			out = append(out, o)
		}
	}
	return out
}

// Judgement is what the recorded history supports saying. Each field is a
// count taken from disk, so every part of the message can name its evidence.
type Judgement struct {
	Task string
	// Failures is the number of failed verifies since the last passing one.
	Failures int
	// FirstLines and LatestLines bracket that streak: the changed-line count
	// at its oldest failure and at its newest.
	FirstLines  int
	LatestLines int
	// Growing reports that the change is getting larger while still failing,
	// which is the difference between a retry that is converging and one that
	// is spreading.
	Growing bool
	// Expansions is how many times the task's boundary was widened, read from
	// the scope file rather than from the log, because it can move between
	// verify runs.
	Expansions int
	// Looping is only ever a suspicion. See minFailures.
	Looping bool
}

// Judge reads the history for a task and says what it supports.
//
// The streak stops at the last passing run: a pass is evidence the task
// reached a working state, so the failures before it were iteration, not a
// loop. Carrying them forever would make every long task eventually trip the
// signal, which is how a heuristic becomes noise and gets ignored.
func Judge(root, task string) Judgement {
	j := Judgement{Task: strings.TrimSpace(task)}
	if j.Task == "" {
		return j
	}

	recs := History(root, task)
	streak := recs
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Passed {
			streak = recs[i+1:]
			break
		}
	}
	j.Failures = len(streak)
	if j.Failures > 0 {
		j.FirstLines = streak[0].Lines
		j.LatestLines = streak[len(streak)-1].Lines
		j.Growing = j.LatestLines > j.FirstLines
	}

	// A boundary that had to be widened three times to fit one task is part of
	// the same story as the failures, so it is read live rather than taken
	// from whatever it was during the last verify.
	if sc, err := scope.LoadScope(root, j.Task); err == nil && sc != nil {
		j.Expansions = len(sc.Expansions)
	}

	j.Looping = j.Failures >= minFailures
	return j
}

// Message is what a person or an agent gets to read, or "" when the history
// does not support saying anything.
//
// It names every number it used. It also says outright that nothing was
// blocked, because a message that reads like a refusal will be treated as one.
func (j Judgement) Message() string {
	if !j.Looping {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "vector: %d failed verifies in a row on %s, with no passing run in between.",
		j.Failures, j.Task)

	switch {
	case j.LatestLines > j.FirstLines:
		fmt.Fprintf(&b, " The diff grew from %d to %d changed lines across them.",
			j.FirstLines, j.LatestLines)
	case j.LatestLines < j.FirstLines:
		// Shrinking is still a loop, but it is the shape of one that may be
		// converging. Saying which way it moved is the difference between a
		// number and a reason to keep going.
		fmt.Fprintf(&b, " The diff shrank from %d to %d changed lines across them.",
			j.FirstLines, j.LatestLines)
	default:
		fmt.Fprintf(&b, " The diff has stayed at %d changed lines.", j.LatestLines)
	}

	if j.Expansions > 0 {
		times := "time"
		if j.Expansions > 1 {
			times = "times"
		}
		fmt.Fprintf(&b, " The scope was widened %d %s to fit it.", j.Expansions, times)
	}
	b.WriteString(" Nothing is blocked — vector cannot tell a productive attempt from" +
		" an unproductive one. Consider stopping and asking a human what to change.")
	return b.String()
}

// Volume measures how large the change is right now: how many files git calls
// changed, and how many lines those changes add or remove.
//
// The git calls live here rather than in gitx on purpose. gitx answers "which
// paths changed", and every caller of it wants that set of paths; counting
// lines is a different question with its own failure modes — binary blobs,
// files git has never seen — and folding it into the shared package would make
// it answer two questions less clearly than it answers one.
//
// base is the same base audit compares against, so the number a verdict was
// recorded beside is the number that verdict was about.
func Volume(root, base string) (files, lines int) {
	changed, err := gitx.ChangedFiles(root, base)
	if err != nil {
		return 0, 0
	}
	files = len(changed)

	ref := base
	if ref == "" {
		ref = "HEAD"
	}
	args := []string{"diff", "--numstat", "--no-renames", ref}
	if base == "" && !hasHEAD(root) {
		// A repository with no commits has no HEAD to diff against, exactly as
		// gitx.ChangedFiles handles it.
		args = []string{"diff", "--numstat", "--no-renames", "--cached"}
	}
	if out, err := git(root, args...); err == nil {
		lines += numstatTotal(out)
	}

	// Untracked files appear in no diff at all, and a brand-new 800-line file
	// is precisely the growth this exists to see. They are read instead. The
	// list comes from the same command gitx uses, so the two agree on which
	// paths are untracked without having to compare spellings.
	if out, err := git(root, "ls-files", "--others", "--exclude-standard"); err == nil {
		for _, p := range strings.Split(out, "\n") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			lines += countLines(filepath.Join(root, filepath.FromSlash(p)))
		}
	}
	return files, lines
}

// numstatTotal sums added and removed lines. A "-" in either column marks a
// binary file, which has no line count; counting it as zero is the honest
// answer, and inventing one would put noise into the only trend this measures.
func numstatTotal(out string) int {
	total := 0
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(strings.TrimSpace(line), "\t", 3)
		if len(f) < 3 {
			continue
		}
		for _, col := range f[:2] {
			if n, err := strconv.Atoi(col); err == nil {
				total += n
			}
		}
	}
	return total
}

// countLines counts newlines in a file without loading it into memory: an
// untracked artifact can be enormous, and vector must not fall over on one.
// A file it cannot read, or one holding a NUL byte, counts as zero rather than
// as a guess.
func countLines(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	r := bufio.NewReader(f)
	buf := make([]byte, 32<<10)
	count, lastByte := 0, byte('\n')
	for {
		n, err := r.Read(buf)
		chunk := buf[:n]
		if bytes.IndexByte(chunk, 0) >= 0 {
			return 0
		}
		count += bytes.Count(chunk, []byte{'\n'})
		if n > 0 {
			lastByte = chunk[n-1]
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return 0
		}
	}
	// A final line with no trailing newline is still a line.
	if lastByte != '\n' {
		count++
	}
	return count
}

func hasHEAD(root string) bool {
	_, err := git(root, "rev-parse", "--verify", "HEAD")
	return err == nil
}

func git(root string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return stdout.String(), nil
}

// line renders one record. Tab-separated so a person can read the file with
// the same tools they read any log with, and every field is passed through key
// so no value can smuggle in a separator and shift the columns.
func (o Outcome) line() string {
	result := "fail"
	if o.Passed {
		result = "pass"
	}
	return fmt.Sprintf("%s\t%s\t%s\t%d\t%d\t%s\n",
		o.At.UTC().Format(time.RFC3339), key(o.Task), key(o.Verdict),
		o.Files, o.Lines, result)
}

func parse(line string) (Outcome, bool) {
	f := strings.Split(strings.TrimRight(line, "\r\n"), "\t")
	if len(f) != 6 {
		return Outcome{}, false
	}
	at, err := time.Parse(time.RFC3339, f[0])
	if err != nil {
		return Outcome{}, false
	}
	files, err := strconv.Atoi(f[3])
	if err != nil {
		return Outcome{}, false
	}
	lines, err := strconv.Atoi(f[4])
	if err != nil {
		return Outcome{}, false
	}
	if f[5] != "pass" && f[5] != "fail" {
		return Outcome{}, false
	}
	if f[1] == "" {
		return Outcome{}, false
	}
	return Outcome{
		At: at, Task: f[1], Verdict: f[2],
		Files: files, Lines: lines, Passed: f[5] == "pass",
	}, true
}

func readLines(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Split(string(data), "\n")
}

// key reduces a value to something that is safe to keep on one tab-separated
// line, the same way the hook's session ids are reduced. History looks the
// task up through key as well, so a name and its recorded form always agree.
func key(s string) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= 128 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// compact trims the log once it outgrows maxBytes, keeping the newest
// keepRecords lines.
//
// This is the one place the file is rewritten, and it is reached roughly once
// per seven hundred appends. The rewrite is a temp file and a rename, so a
// reader never sees a half-written log; a session that appends during the
// rename writes into the replaced inode and loses those records. That is the
// trade being made on purpose: the loss is a smaller streak, which errs toward
// saying nothing, and this signal saying nothing is the harmless direction.
// Trimming in place would risk a reader seeing a truncated file instead.
//
// Every failure here is ignored. An unwritable repository should keep a log
// that is too long, not lose a verify run over housekeeping.
func compact(path string) {
	info, err := os.Stat(path)
	if err != nil || info.Size() <= maxBytes {
		return
	}
	var kept []string
	for _, line := range readLines(path) {
		if strings.TrimSpace(line) != "" {
			kept = append(kept, line)
		}
	}
	if len(kept) <= keepRecords {
		return
	}
	kept = kept[len(kept)-keepRecords:]

	tmp, err := os.CreateTemp(filepath.Dir(path), ".vector-attempts-*")
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(strings.Join(kept, "\n") + "\n"); err != nil {
		tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp.Name(), path)
}
