package freshness

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newRepo builds a real repository. Nothing here is stubbed: the whole claim
// this package makes is about files on a disk, and a fake filesystem would let
// it pass while being wrong about the only thing it does.
func newRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	cmd := exec.Command("git", "init", "-q", "-b", "main")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	return root
}

func mk(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTheHashIsTheOneGitWouldGive(t *testing.T) {
	// This is the load-bearing claim of the package: the identifier is git's
	// own, not a fingerprint scheme vector invented. Computing it in process is
	// only allowed because the value is identical, so the identity is pinned
	// against git itself rather than against a constant someone copied once.
	root := newRepo(t)
	files := []string{"empty.txt", "one.txt", "src/deep/nested.ts", "binary.bin"}
	mk(t, root, "empty.txt", "")
	mk(t, root, "one.txt", "hello\n")
	mk(t, root, "src/deep/nested.ts", "export const x = 1;\nexport const y = 2;\n")
	mk(t, root, "binary.bin", "\x00\x01\x02\xffnot text\n")

	got := Hash(root, files)
	for _, f := range files {
		cmd := exec.Command("git", "hash-object", "--", f)
		cmd.Dir = root
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git hash-object %s: %v", f, err)
		}
		want := strings.TrimSpace(string(out))
		if got[f] != want {
			t.Errorf("Hash(%s) = %q, git says %q", f, got[f], want)
		}
	}
}

func TestAPathThatCannotBeReadIsAbsentRatherThanZero(t *testing.T) {
	// A missing file must not hash to something. "Gone" is a real answer that
	// Compare reads as a deletion, and a placeholder hash would turn it into a
	// silent match.
	root := newRepo(t)
	mk(t, root, "here.txt", "x\n")
	if err := os.MkdirAll(filepath.Join(root, "adirectory"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := Hash(root, []string{"here.txt", "gone.txt", "adirectory"})
	if _, ok := got["here.txt"]; !ok {
		t.Error("a readable file produced no hash")
	}
	for _, absent := range []string{"gone.txt", "adirectory"} {
		if h, ok := got[absent]; ok {
			t.Errorf("%s hashed to %q, want no entry at all", absent, h)
		}
	}
}

// verified records a passing verdict covering the given files, exactly as
// verify would.
func verified(t *testing.T, root, task string, files ...string) Snapshot {
	t.Helper()
	s := Snapshot{
		At:      time.Now(),
		Task:    task,
		Verdict: "VERIFIED",
		Passed:  true,
		Files:   Hash(root, files),
	}
	if err := Record(root, s); err != nil {
		t.Fatalf("Record: %v", err)
	}
	got, ok := Last(root, task)
	if !ok {
		t.Fatal("the verdict just recorded could not be read back")
	}
	return got
}

func TestAFreshVerdictIsNotStale(t *testing.T) {
	// The moment after a verdict, it is a true statement about the tree. If
	// this reported staleness, every verify would immediately invalidate
	// itself and the signal would mean nothing.
	root := newRepo(t)
	mk(t, root, "src/a.ts", "a\n")
	mk(t, root, "src/b.ts", "b\n")
	snap := verified(t, root, "task", "src/a.ts", "src/b.ts")

	st := Compare(root, snap, []string{"src/a.ts", "src/b.ts"})
	if st.Stale() {
		t.Errorf("a verdict was stale the moment it was reached: %+v", st)
	}
	if st.Message() != "" {
		t.Errorf("message = %q, want none", st.Message())
	}
}

func TestEditingAnInScopeFileMakesTheVerdictStaleAndNamesIt(t *testing.T) {
	// The whole point. "Something changed" is the unfalsifiable answer this
	// tool refuses to give, so the file has to be named.
	root := newRepo(t)
	mk(t, root, "src/dashboard/Filter.tsx", "export const Filter = 1;\n")
	mk(t, root, "src/untouched.ts", "stable\n")
	snap := verified(t, root, "task", "src/dashboard/Filter.tsx", "src/untouched.ts")

	mk(t, root, "src/dashboard/Filter.tsx", "export const Filter = 2;\n")
	st := Compare(root, snap, []string{"src/dashboard/Filter.tsx", "src/untouched.ts"})

	if !st.Stale() {
		t.Fatal("an edited in-scope file left the verdict fresh")
	}
	if len(st.Changed) != 1 || st.Changed[0] != "src/dashboard/Filter.tsx" {
		t.Errorf("changed = %v, want only the edited file", st.Changed)
	}
	msg := st.Message()
	if !strings.Contains(msg, "src/dashboard/Filter.tsx") {
		t.Errorf("message = %q, want the file named", msg)
	}
	// The reader must not be able to mistake this for a failure.
	if !strings.Contains(msg, "nothing is blocked") {
		t.Errorf("message = %q, want it to say nothing is blocked", msg)
	}
	if !strings.Contains(msg, "UNVERIFIED") {
		t.Errorf("message = %q, want the existing word for unknown", msg)
	}
}

func TestTouchingAFileWithoutChangingItIsNotStaleness(t *testing.T) {
	// A rewrite with identical bytes, a checkout, a formatter that changed
	// nothing: the content is the evidence, not the timestamp. Reporting these
	// would train the reader to ignore the signal.
	root := newRepo(t)
	mk(t, root, "src/a.ts", "a\n")
	snap := verified(t, root, "task", "src/a.ts")

	mk(t, root, "src/a.ts", "a\n")
	if st := Compare(root, snap, []string{"src/a.ts"}); st.Stale() {
		t.Errorf("rewriting identical content read as staleness: %+v", st)
	}
}

func TestEditingAnOutOfScopeFileDoesNotMakeTheVerdictStale(t *testing.T) {
	// A file the verdict never covered cannot invalidate it. Drift outside the
	// boundary is the audit's finding, and reporting it here too would give the
	// same file two names in one message.
	root := newRepo(t)
	mk(t, root, "src/a.ts", "a\n")
	snap := verified(t, root, "task", "src/a.ts")

	mk(t, root, "elsewhere/x.ts", "brand new and undeclared\n")
	// The audit would not list elsewhere/x.ts as in scope, so neither does the
	// caller — which is exactly the input this asserts against.
	if st := Compare(root, snap, []string{"src/a.ts"}); st.Stale() {
		t.Errorf("an out-of-scope file made the verdict stale: %+v", st)
	}
}

func TestAddingAnInScopeFileCountsAsMovement(t *testing.T) {
	// A new file inside the boundary is part of the work and was never
	// verified. Untracked counts: git has not seen it, but the reader is still
	// being told VERIFIED about a tree that does not include it.
	root := newRepo(t)
	mk(t, root, "src/a.ts", "a\n")
	snap := verified(t, root, "task", "src/a.ts")

	mk(t, root, "src/added.ts", "new work\n")
	st := Compare(root, snap, []string{"src/a.ts", "src/added.ts"})

	if !st.Stale() {
		t.Fatal("a new in-scope file left the verdict fresh")
	}
	if len(st.Added) != 1 || st.Added[0] != "src/added.ts" {
		t.Errorf("added = %v, want only the new file", st.Added)
	}
	if !strings.Contains(st.Message(), "src/added.ts was added") {
		t.Errorf("message = %q, want the addition named", st.Message())
	}
}

func TestDeletingAnInScopeFileCountsAsMovement(t *testing.T) {
	// Deleting a verified file changes what the verdict was about just as
	// surely as editing one, and a comparison that only looked at files still
	// present would report the tree as unchanged.
	root := newRepo(t)
	mk(t, root, "src/a.ts", "a\n")
	mk(t, root, "src/gone.ts", "doomed\n")
	snap := verified(t, root, "task", "src/a.ts", "src/gone.ts")

	if err := os.Remove(filepath.Join(root, "src", "gone.ts")); err != nil {
		t.Fatal(err)
	}
	st := Compare(root, snap, []string{"src/a.ts"})

	if !st.Stale() {
		t.Fatal("a deleted in-scope file left the verdict fresh")
	}
	if len(st.Removed) != 1 || st.Removed[0] != "src/gone.ts" {
		t.Errorf("removed = %v, want only the deleted file", st.Removed)
	}
	if !strings.Contains(st.Message(), "src/gone.ts was deleted") {
		t.Errorf("message = %q, want the deletion named", st.Message())
	}
}

func TestAbsentStateMeansNoPriorVerdict(t *testing.T) {
	// A repository that has never verified must behave exactly as it did
	// before any of this existed.
	root := newRepo(t)
	if _, ok := Last(root, "task"); ok {
		t.Error("a verdict was found in a repository that never recorded one")
	}
}

func TestCorruptStateMeansNoPriorVerdictRatherThanAnError(t *testing.T) {
	// Unreadable bookkeeping is not a condition anyone can act on. Every one of
	// these shapes has to degrade to silence: an error escaping here would cost
	// the user the end of their turn over a state file.
	for name, content := range map[string]string{
		"binary noise":   "\x00\x01 not json at all\n",
		"truncated json": `{"schema":"vector.freshness/v1","snapshots":[{"task":"ta`,
		"wrong shape":    `{"schema":"vector.freshness/v1","snapshots":"not a list"}`,
		"empty file":     "",
		"nameless task":  `{"snapshots":[{"task":"  ","verdict":"VERIFIED","passed":true}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			root := newRepo(t)
			if err := os.MkdirAll(filepath.Join(root, ".vector"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(Path(root), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, ok := Last(root, "task"); ok {
				t.Error("corrupt state produced a verdict")
			}
		})
	}
}

func TestAnUnreadableStateFileMeansNoPriorVerdict(t *testing.T) {
	// A directory where the file should be is the shape an unreadable state
	// takes in a test, and it must be as harmless as a missing one.
	root := newRepo(t)
	if err := os.MkdirAll(Path(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := Last(root, "task"); ok {
		t.Error("an unreadable state file produced a verdict")
	}
}

func TestASnapshotWithNoTaskBelongsToNothing(t *testing.T) {
	// Without a task there is no key to file a verdict under, and pooling
	// unrelated work into one record would make every answer about the wrong
	// tree.
	root := newRepo(t)
	if err := Record(root, Snapshot{Verdict: "VERIFIED", Passed: true}); err == nil {
		t.Error("a snapshot with no task was accepted")
	}
}

func TestAVerdictSupersedesTheLastOneForTheSameTask(t *testing.T) {
	// One verdict per task is the whole ambition. Keeping the older one would
	// let a verdict from two edits ago answer a question about now, and the
	// shape of a task over time is already internal/attempt's job.
	root := newRepo(t)
	mk(t, root, "src/a.ts", "first\n")
	verified(t, root, "task", "src/a.ts")

	mk(t, root, "src/a.ts", "second\n")
	if err := Record(root, Snapshot{
		Task: "task", Verdict: "FAILED", Passed: false, Files: Hash(root, []string{"src/a.ts"}),
	}); err != nil {
		t.Fatal(err)
	}

	got, ok := Last(root, "task")
	if !ok {
		t.Fatal("no verdict after two recordings")
	}
	if got.Verdict != "FAILED" || got.Passed {
		t.Errorf("verdict = %s (passed=%v), want the newer FAILED", got.Verdict, got.Passed)
	}
	// The superseding record must also describe the tree it was taken from, or
	// the next comparison answers with the older run's hashes.
	if st := Compare(root, got, []string{"src/a.ts"}); st.Stale() {
		t.Errorf("the superseding snapshot did not describe the current tree: %+v", st)
	}
}

func TestTasksDoNotOverwriteEachOther(t *testing.T) {
	// Two tasks in one repository each keep their own verdict, so switching
	// between them does not silently retract an answer about the other.
	root := newRepo(t)
	mk(t, root, "src/a.ts", "a\n")
	mk(t, root, "src/b.ts", "b\n")
	verified(t, root, "alpha", "src/a.ts")
	verified(t, root, "beta", "src/b.ts")

	for _, task := range []string{"alpha", "beta"} {
		if _, ok := Last(root, task); !ok {
			t.Errorf("the verdict for %s was lost", task)
		}
	}
}

func TestTheStateStaysBounded(t *testing.T) {
	// The Stop hook reads this file whole, once per turn. A repository that has
	// run a hundred tasks must not make that read grow without limit.
	root := newRepo(t)
	mk(t, root, "src/a.ts", "a\n")
	for i := 0; i < maxTasks*3; i++ {
		verified(t, root, "task"+string(rune('a'+i)), "src/a.ts")
	}
	if _, ok := Last(root, "task"+string(rune('a'))); ok {
		t.Error("the oldest task survived; the state is not bounded")
	}
	if _, ok := Last(root, "task"+string(rune('a'+maxTasks*3-1))); !ok {
		t.Error("the newest task was evicted; the wrong end was trimmed")
	}
}

func TestALongListOfMovedFilesIsSummarised(t *testing.T) {
	// Naming forty files is the same as naming none: the one that matters is
	// buried and nobody reads to the end.
	var st Staleness
	st.Task, st.Verdict = "task", "VERIFIED"
	for i := 0; i < maxNamed+4; i++ {
		st.Changed = append(st.Changed, "src/f"+string(rune('a'+i))+".ts")
	}
	msg := st.Message()
	if !strings.Contains(msg, "and 4 more") {
		t.Errorf("message = %q, want the tail summarised", msg)
	}
	if strings.Contains(msg, "src/fi.ts") {
		t.Errorf("message = %q, want it to stop naming after %d", msg, maxNamed)
	}
}
