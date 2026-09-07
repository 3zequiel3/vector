package verify

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/3zequiel3/vector/internal/attempt"
	"github.com/3zequiel3/vector/internal/audit"
)

// newRepo builds a Go repository whose verification commands vector will find
// on its own, so the test exercises detection rather than a hand-fed config.
func newRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	cmd := exec.Command("git", "init", "-q", "-b", "main")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	mk(t, root, "go.mod", "module example.com/x\n\ngo 1.27\n")
	mk(t, root, "go.sum", "")
	mk(t, root, "x.go", "package x\n\nfunc F() int { return 1 }\n")
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

func TestPassingChecksWithoutAScopeAreNotVerified(t *testing.T) {
	// Everything green is still not a full verdict when nobody declared where
	// the change was allowed to go.
	root := newRepo(t)
	rep, err := Run(Options{Dir: root, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != PartiallyVerified {
		t.Errorf("verdict = %s (%s), want PARTIALLY_VERIFIED", rep.Verdict, rep.Reason)
	}
	if !strings.Contains(rep.Reason, "no scope") {
		t.Errorf("reason = %q, want it to name the missing scope", rep.Reason)
	}
}

func TestAFailingCheckFails(t *testing.T) {
	root := newRepo(t)
	mk(t, root, "x.go", "package x\n\nfunc F() int { return \"not an int\" }\n")

	rep, err := Run(Options{Dir: root, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != Failed {
		t.Fatalf("verdict = %s (%s), want FAILED", rep.Verdict, rep.Reason)
	}
	if rep.ExitCode() != ExitProblem {
		t.Errorf("ExitCode = %d, want %d", rep.ExitCode(), ExitProblem)
	}
	// The failure output has to reach the reader; a verdict with no evidence
	// behind it is just an opinion.
	var sawOutput bool
	for _, c := range rep.Checks {
		if !c.Passed && c.Skipped == "" && c.Output != "" {
			sawOutput = true
		}
	}
	if !sawOutput {
		t.Error("no failing check carried its output")
	}
}

func TestScopeOutranksPassingChecks(t *testing.T) {
	// Passing tests do not retroactively authorize touching files nobody
	// declared. This is the ordering that keeps verify from laundering drift.
	root := newRepo(t)
	mk(t, root, ".vector/policy.toml",
		"[scope]\nalways_forbidden = []\n\n[mode]\nenforcement = \"advisory\"\n")
	mk(t, root, ".vector/scope/task.toml",
		"objective = \"only touch docs\"\nwrite = [\"docs/**\"]\n")
	mk(t, root, ".vector/current", "task\n")

	rep, err := Run(Options{Dir: root, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != OutOfScope {
		t.Fatalf("verdict = %s (%s), want OUT_OF_SCOPE", rep.Verdict, rep.Reason)
	}
	if rep.Scope.Status != audit.OutOfScope {
		t.Errorf("scope status = %s, want OUT_OF_SCOPE", rep.Scope.Status)
	}
	if rep.ExitCode() != ExitProblem {
		t.Errorf("ExitCode = %d, want %d", rep.ExitCode(), ExitProblem)
	}
}

func TestNothingRanIsNeverVerified(t *testing.T) {
	// A repository vector cannot verify must say so. Silence must not read as
	// success.
	root := t.TempDir()
	cmd := exec.Command("git", "init", "-q", "-b", "main")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	rep, err := Run(Options{Dir: root, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != Unverified {
		t.Errorf("verdict = %s (%s), want UNVERIFIED", rep.Verdict, rep.Reason)
	}
	if rep.ExitCode() == ExitOK {
		t.Error("UNVERIFIED exited 0; not checking must not look like passing")
	}
}

func TestOnlyNarrowsTheRun(t *testing.T) {
	root := newRepo(t)
	rep, err := Run(Options{Dir: root, Only: []string{"lint"}, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Checks) != 1 || rep.Checks[0].Name != "lint" {
		t.Fatalf("checks = %+v, want only lint", rep.Checks)
	}
}

func TestChecksRunCheapestFirst(t *testing.T) {
	// A type error explains the test failures that follow, so paying for the
	// slow signal first spends time on a result you can already predict.
	root := newRepo(t)
	rep, err := Run(Options{Dir: root, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range rep.Checks {
		names = append(names, c.Name)
	}
	want := []string{"typecheck", "lint", "test", "build"}
	for i := range want {
		if i >= len(names) || names[i] != want[i] {
			t.Fatalf("order = %v, want %v", names, want)
		}
	}
}

func TestTimeoutIsReportedAsFailureNotAHang(t *testing.T) {
	c := run(t.TempDir(), "test", "sleep 5", 50*time.Millisecond)
	if c.Passed {
		t.Fatal("a timed-out command was reported as passing")
	}
	if !strings.Contains(c.Output, "timed out") {
		t.Errorf("output = %q, want it to name the timeout", c.Output)
	}
}

// newScopedRepo is newRepo with a declared boundary wide enough that the run
// stays inside it, which is what gives the run a task id to be counted under.
func newScopedRepo(t *testing.T) string {
	t.Helper()
	root := newRepo(t)
	mk(t, root, ".vector/policy.toml",
		"[scope]\nalways_forbidden = []\n\n[mode]\nenforcement = \"advisory\"\n")
	mk(t, root, ".vector/scope/task.toml",
		"objective = \"whatever it takes\"\nwrite = [\"**\"]\n")
	mk(t, root, ".vector/current", "task\n")
	return root
}

func TestVerifyRecordsTheOutcomeAgainstTheTask(t *testing.T) {
	// vector does not run the agent, so the only attempts it can count are the
	// verify runs it performed itself. If the run is not written down, there is
	// nothing for a later turn to notice a loop in.
	root := newScopedRepo(t)
	rep, err := Run(Options{Dir: root, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}

	h := attempt.History(root, "task")
	if len(h) != 1 {
		t.Fatalf("history = %d records, want 1", len(h))
	}
	got := h[0]
	if got.Verdict != string(rep.Verdict) {
		t.Errorf("recorded verdict = %q, want %q", got.Verdict, rep.Verdict)
	}
	// Recording a size of zero would make every diff look like it never grew.
	if got.Files == 0 || got.Lines == 0 {
		t.Errorf("recorded %d files / %d lines, want the change measured", got.Files, got.Lines)
	}
	// "Passed" and the exit code must never disagree about the same run.
	if got.Passed != (rep.ExitCode() == ExitOK) {
		t.Errorf("recorded passed = %v, exit code = %d", got.Passed, rep.ExitCode())
	}
}

func TestARunWithNoTaskRecordsNothing(t *testing.T) {
	// Without a declared scope there is no task for an attempt to belong to,
	// and inventing one would pool unrelated work under a single count.
	root := newRepo(t)
	if _, err := Run(Options{Dir: root, Timeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(attempt.Path(root)); !os.IsNotExist(err) {
		t.Errorf("an attempt log exists (%v), want none without a task", err)
	}
}

func TestTheRetryCountNeverChangesTheVerdictOrTheExitCode(t *testing.T) {
	// This is the whole constraint. vector cannot tell a productive fourth
	// attempt from an unproductive one, so a long history of failures must
	// leave the verdict of the next run exactly where it would have been.
	root := newScopedRepo(t)
	for i := 1; i <= 6; i++ {
		if err := attempt.Record(root, attempt.Outcome{
			At:   time.Date(2026, 1, 1, 0, i, 0, 0, time.UTC),
			Task: "task", Verdict: string(Failed), Files: 9, Lines: 200 * i,
		}); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := Run(Options{Dir: root, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != Verified {
		t.Errorf("verdict = %s (%s), want VERIFIED despite the history", rep.Verdict, rep.Reason)
	}
	if rep.ExitCode() != ExitOK {
		t.Errorf("ExitCode = %d, want %d; the retry signal must not fail a run",
			rep.ExitCode(), ExitOK)
	}
}

func TestAnUnwritableAttemptLogDoesNotFailVerify(t *testing.T) {
	// Bookkeeping is not the answer verify owes its caller. A repository where
	// the log cannot be written still gets its verdict.
	root := newScopedRepo(t)
	if err := os.MkdirAll(attempt.Path(root), 0o755); err != nil {
		t.Fatal(err)
	}
	rep, err := Run(Options{Dir: root, Timeout: time.Minute})
	if err != nil {
		t.Fatalf("Run failed over a log it could not write: %v", err)
	}
	if rep.Verdict != Verified {
		t.Errorf("verdict = %s (%s), want VERIFIED", rep.Verdict, rep.Reason)
	}
}

func TestTailKeepsTheEnd(t *testing.T) {
	// The failure is at the end of a test run, and an uncapped dump would
	// flood whatever reads this.
	var lines []string
	for i := 0; i < 100; i++ {
		lines = append(lines, string(rune('a'+i%26)))
	}
	got := tail(strings.Join(lines, "\n"), 5)
	if strings.Count(got, "\n") != 5 {
		t.Errorf("got %d newlines, want 5 kept lines plus the elision marker", strings.Count(got, "\n"))
	}
	if !strings.HasPrefix(got, "…") {
		t.Error("truncation was not marked")
	}
}

func TestRepeatedFailuresReachWhoeverRanVerify(t *testing.T) {
	// The retry signal was only reaching the Stop hook, which means a person
	// running verify in a terminal — exactly the person deciding whether to try
	// again — never heard it.
	root := newRepo(t)
	mk(t, root, ".vector/policy.toml",
		"[scope]\nalways_forbidden = []\n\n[mode]\nenforcement = \"advisory\"\n")
	mk(t, root, ".vector/scope/task.toml", "objective = \"fix it\"\nwrite = [\"**\"]\n")
	mk(t, root, ".vector/current", "task\n")
	mk(t, root, "x.go", "package x\n\nfunc F() int { return \"broken\" }\n")

	var rep Report
	for i := 0; i < 3; i++ {
		var err error
		rep, err = Run(Options{Dir: root, Timeout: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		if rep.Verdict != Failed {
			t.Fatalf("run %d: verdict = %s (%s), want FAILED", i, rep.Verdict, rep.Reason)
		}
	}

	if rep.Retry == "" {
		t.Fatal("three consecutive failures produced no retry signal")
	}
	if !strings.Contains(rep.Retry, "task") {
		t.Errorf("retry = %q, want the task named", rep.Retry)
	}
	// The signal must not turn into a verdict of its own: vector cannot tell a
	// productive fourth attempt from an unproductive one, and halting real work
	// on a heuristic is how a tool gets uninstalled.
	if rep.ExitCode() != ExitProblem {
		t.Errorf("ExitCode = %d, want the failing checks to decide it, not the retry signal", rep.ExitCode())
	}
}

func TestASinglePassingRunCarriesNoRetrySignal(t *testing.T) {
	root := newRepo(t)
	mk(t, root, ".vector/scope/task.toml", "objective = \"ok\"\nwrite = [\"**\"]\n")
	mk(t, root, ".vector/current", "task\n")

	rep, err := Run(Options{Dir: root, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Retry != "" {
		t.Errorf("retry = %q, want none on a first run", rep.Retry)
	}
}
