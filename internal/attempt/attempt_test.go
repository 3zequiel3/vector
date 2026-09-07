package attempt

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newRepo builds a real repository, because everything this package measures
// comes out of git. A stub would let the tests agree with an implementation
// that git disagrees with.
func newRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	run(t, root, "init", "-q", "-b", "main")
	return root
}

func run(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
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

// fail records one failed verify of the given size, at a distinct time so the
// order on disk is the order they happened in.
func fail(t *testing.T, root string, n, lines int) {
	t.Helper()
	record(t, root, n, lines, false)
}

func record(t *testing.T, root string, n, lines int, passed bool) {
	t.Helper()
	verdict := "FAILED"
	if passed {
		verdict = "VERIFIED"
	}
	err := Record(root, Outcome{
		At:      time.Date(2026, 1, 1, 0, n, 0, 0, time.UTC),
		Task:    "task",
		Verdict: verdict,
		Files:   3,
		Lines:   lines,
		Passed:  passed,
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
}

func TestAFirstFailureIsNotALoop(t *testing.T) {
	// A task that failed once has been worked on, not circled. Firing here
	// would fire on nearly every task and teach the reader to skip the message.
	root := newRepo(t)
	fail(t, root, 1, 120)

	j := Judge(root, "task")
	if j.Looping {
		t.Errorf("Looping = true after one failure: %q", j.Message())
	}
	if j.Failures != 1 {
		t.Errorf("Failures = %d, want 1", j.Failures)
	}
	if j.Message() != "" {
		t.Errorf("Message = %q, want silence", j.Message())
	}
}

func TestASecondFailureIsStillNotALoop(t *testing.T) {
	// Most real fixes land on the second try. The threshold has to sit above
	// the ordinary correction or it is not measuring anything.
	root := newRepo(t)
	fail(t, root, 1, 120)
	fail(t, root, 2, 200)

	if j := Judge(root, "task"); j.Looping {
		t.Errorf("Looping = true after two failures: %q", j.Message())
	}
}

func TestConsecutiveFailuresCrossTheThreshold(t *testing.T) {
	root := newRepo(t)
	fail(t, root, 1, 120)
	fail(t, root, 2, 400)
	fail(t, root, 3, 812)

	j := Judge(root, "task")
	if !j.Looping {
		t.Fatalf("Looping = false after %d failures", j.Failures)
	}
	// Every number in the message has to be one a reader can go and check,
	// or the message is an opinion.
	msg := j.Message()
	for _, want := range []string{"3", "task", "120", "812", "Nothing is blocked"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Message = %q, want it to name %q", msg, want)
		}
	}
	// Nothing in this tool reports safety, and nothing here reports a verdict.
	if strings.Contains(strings.ToLower(msg), "safe") {
		t.Errorf("Message = %q, want no claim of safety", msg)
	}
}

func TestAPassEndsTheSignal(t *testing.T) {
	// A pass is evidence the task reached a working state, so the failures
	// before it were iteration. Counting them forever would make every long
	// task eventually trip the signal.
	root := newRepo(t)
	fail(t, root, 1, 120)
	fail(t, root, 2, 400)
	fail(t, root, 3, 812)
	record(t, root, 4, 830, true)

	j := Judge(root, "task")
	if j.Looping || j.Failures != 0 {
		t.Errorf("Failures = %d, Looping = %v after a pass; want the streak ended", j.Failures, j.Looping)
	}
	// The history itself is not erased: the file is append-only, and the
	// earlier failures stay reviewable.
	if got := len(History(root, "task")); got != 4 {
		t.Errorf("History = %d records, want all 4 kept on disk", got)
	}
	// And a failure after the pass starts from one, not from four.
	fail(t, root, 5, 900)
	if j := Judge(root, "task"); j.Failures != 1 || j.Looping {
		t.Errorf("Failures = %d, Looping = %v; want the count restarted", j.Failures, j.Looping)
	}
}

func TestAGrowingDiffIsDistinguishedFromAShrinkingOne(t *testing.T) {
	// Both are loops. Only one is spreading, and the reader needs to know
	// which one they are looking at before deciding what to do about it.
	grow := newRepo(t)
	fail(t, grow, 1, 120)
	fail(t, grow, 2, 400)
	fail(t, grow, 3, 812)

	shrink := newRepo(t)
	fail(t, shrink, 1, 812)
	fail(t, shrink, 2, 400)
	fail(t, shrink, 3, 120)

	g, s := Judge(grow, "task"), Judge(shrink, "task")
	if !g.Growing {
		t.Error("Growing = false for a diff that went 120 -> 812")
	}
	if s.Growing {
		t.Error("Growing = true for a diff that went 812 -> 120")
	}
	if !s.Looping {
		t.Error("a shrinking diff across three failures is still three failures")
	}
	if !strings.Contains(g.Message(), "grew") {
		t.Errorf("growing message = %q, want it to say so", g.Message())
	}
	if !strings.Contains(s.Message(), "shrank") {
		t.Errorf("shrinking message = %q, want it to say so", s.Message())
	}
}

func TestAbsentStateIsNoHistory(t *testing.T) {
	// A repository that has never run verify has nothing to say, and must not
	// produce an error on the way to saying it.
	root := newRepo(t)
	if got := History(root, "task"); got != nil {
		t.Errorf("History = %v, want none", got)
	}
	j := Judge(root, "task")
	if j.Looping || j.Failures != 0 {
		t.Errorf("Judge = %+v, want an empty judgement", j)
	}
}

func TestCorruptStateIsNoHistory(t *testing.T) {
	// Bookkeeping vector cannot parse is bookkeeping it does not count. The
	// readable records still count: dropping them too would turn one bad line
	// into a silently disabled feature.
	root := newRepo(t)
	fail(t, root, 1, 120)
	fail(t, root, 2, 400)

	path := Path(root)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	junk := "\x00\x01 not a record\nnot\ta\trecord\teither\n" +
		"2026-01-01T00:03:00Z\ttask\tFAILED\tmany\tlots\tfail\n"
	if err := os.WriteFile(path, append([]byte(junk), data...), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := len(History(root, "task")); got != 2 {
		t.Errorf("History = %d records, want the 2 readable ones", got)
	}
	if j := Judge(root, "task"); j.Looping {
		t.Errorf("Looping = true on unparseable lines: %q", j.Message())
	}
}

func TestAnUnreadableLogIsNoHistory(t *testing.T) {
	// A directory where the log belongs is unreadable without depending on
	// file modes a root-run test would ignore.
	root := newRepo(t)
	if err := os.MkdirAll(Path(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := History(root, "task"); got != nil {
		t.Errorf("History = %v, want none", got)
	}
	if j := Judge(root, "task"); j.Looping {
		t.Error("an unreadable log produced a judgement")
	}
}

func TestOneTasksFailuresAreNotAnothers(t *testing.T) {
	// Two tasks share one log. Counting them together would report a loop that
	// nobody is in.
	root := newRepo(t)
	for i := 1; i <= 3; i++ {
		if err := Record(root, Outcome{
			At:      time.Date(2026, 1, 1, 0, i, 0, 0, time.UTC),
			Task:    []string{"alpha", "beta", "alpha"}[i-1],
			Verdict: "FAILED", Lines: 100 * i,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if j := Judge(root, "alpha"); j.Failures != 2 {
		t.Errorf("alpha Failures = %d, want 2", j.Failures)
	}
	if j := Judge(root, "beta"); j.Failures != 1 {
		t.Errorf("beta Failures = %d, want 1", j.Failures)
	}
}

func TestExpansionsAreCountedFromTheScopeFile(t *testing.T) {
	// The boundary can be widened between two verify runs, so the count is
	// read live rather than taken from whatever it was when a run was logged.
	root := newRepo(t)
	fail(t, root, 1, 120)
	fail(t, root, 2, 400)
	fail(t, root, 3, 812)
	mk(t, root, ".vector/scope/task.toml", `objective = "x"
write = ["src/**"]

[[expansion]]
at = "2026-01-01T00:00:00Z"
reason = "blocking"
evidence = "the build imports it"
write = ["lib/**"]

[[expansion]]
at = "2026-01-01T01:00:00Z"
reason = "blocking"
evidence = "so does the test"
write = ["test/**"]
`)
	j := Judge(root, "task")
	if j.Expansions != 2 {
		t.Fatalf("Expansions = %d, want 2", j.Expansions)
	}
	if !strings.Contains(j.Message(), "widened 2 times") {
		t.Errorf("Message = %q, want the widening named", j.Message())
	}
}

func TestTheLogStaysBounded(t *testing.T) {
	// Per-developer state that only grows is a file someone eventually has to
	// delete by hand. The newest records are the ones the judgement uses, so
	// they are the ones compaction keeps.
	root := newRepo(t)
	const runs = 2000
	for i := 1; i <= runs; i++ {
		if err := Record(root, Outcome{
			At:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute),
			Task: "task", Verdict: "FAILED", Files: 3, Lines: i, Passed: false,
		}); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(Path(root))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > maxBytes {
		t.Errorf("log is %d bytes after %d runs, want it capped at %d", info.Size(), runs, maxBytes)
	}
	h := History(root, "task")
	if len(h) == 0 || len(h) >= runs {
		t.Fatalf("History = %d records after %d runs, want a bounded tail", len(h), runs)
	}
	// Compaction must trim the old end, not the useful one.
	if got := h[len(h)-1].Lines; got != runs {
		t.Errorf("newest record has Lines = %d, want %d", got, runs)
	}
}

func TestVolumeCountsTrackedAndUntrackedChanges(t *testing.T) {
	// A brand-new file appears in no diff, and a retry that adds one is
	// exactly the growth this measures. Counting only tracked changes would
	// report a runaway diff as standing still.
	root := newRepo(t)
	mk(t, root, "kept.txt", "a\nb\nc\n")
	run(t, root, "add", "-A")
	run(t, root, "commit", "-qm", "base")

	mk(t, root, "kept.txt", "a\nb\nc\nd\n") // +1 line
	mk(t, root, "fresh.txt", "1\n2\n3\n4\n5\n")

	files, lines := Volume(root, "")
	if files != 2 {
		t.Errorf("files = %d, want 2 (one modified, one new)", files)
	}
	if lines != 6 {
		t.Errorf("lines = %d, want 6 (1 added to kept.txt, 5 in fresh.txt)", lines)
	}
}

func TestVolumeGrowsWithTheDiff(t *testing.T) {
	// The number has to move in the same direction the work does, or the
	// growth signal is measuring noise.
	root := newRepo(t)
	mk(t, root, "a.txt", "one\n")
	run(t, root, "add", "-A")
	run(t, root, "commit", "-qm", "base")

	mk(t, root, "a.txt", "one\ntwo\n")
	_, small := Volume(root, "")
	mk(t, root, "a.txt", strings.Repeat("line\n", 200))
	_, big := Volume(root, "")
	if big <= small {
		t.Errorf("volume went %d -> %d as the diff grew", small, big)
	}
}

func TestVolumeInARepositoryWithNoCommits(t *testing.T) {
	// The very first task in a fresh repository has no HEAD to diff against,
	// and answering zero there would hide the whole change.
	root := newRepo(t)
	mk(t, root, "new.txt", "1\n2\n3\n")
	files, lines := Volume(root, "")
	if files != 1 || lines != 3 {
		t.Errorf("files, lines = %d, %d; want 1, 3", files, lines)
	}
}

func TestVolumeOutsideARepositoryIsZeroNotAPanic(t *testing.T) {
	files, lines := Volume(t.TempDir(), "")
	if files != 0 || lines != 0 {
		t.Errorf("files, lines = %d, %d; want 0, 0", files, lines)
	}
}

func TestABinaryFileContributesNoLineCount(t *testing.T) {
	// A blob has no lines. Inventing a number for it would put noise into the
	// only trend this measures.
	root := newRepo(t)
	mk(t, root, "text.txt", "a\nb\n")
	if err := os.WriteFile(filepath.Join(root, "blob.bin"),
		[]byte{0x00, 0x01, 0x02, '\n', '\n', '\n'}, 0o644); err != nil {
		t.Fatal(err)
	}
	_, lines := Volume(root, "")
	if lines != 2 {
		t.Errorf("lines = %d, want 2 — the binary file's newlines must not count", lines)
	}
}

func TestRecordWithoutATaskRecordsNothing(t *testing.T) {
	// A run that is attached to no task has nothing to be counted against.
	root := newRepo(t)
	if err := Record(root, Outcome{Verdict: "FAILED"}); err == nil {
		t.Error("an outcome with no task was accepted")
	}
	if _, err := os.Stat(Path(root)); !os.IsNotExist(err) {
		t.Errorf("a log was created (%v), want none", err)
	}
}
