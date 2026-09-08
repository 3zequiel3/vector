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
	"github.com/3zequiel3/vector/internal/freshness"
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

// newVerdictRepo is newScopedRepo as `vector init` actually leaves it: the
// per-developer state under .vector/ is gitignored, so the files vector writes
// about a run are not themselves changes the next run has to explain. Without
// that line in setup's localState, the verdict state written at the end of one
// verify would look like a new in-scope file at the start of the next.
func newVerdictRepo(t *testing.T) string {
	t.Helper()
	root := newScopedRepo(t)
	mk(t, root, ".vector/.gitignore", "current\nnudged\nattempts\nverdicts\n")
	return root
}

// inScopeNow asks the same question the Stop hook asks: what is inside the
// boundary at this moment.
func inScopeNow(t *testing.T, root string) []string {
	t.Helper()
	rep, err := audit.Run(audit.Options{Dir: root})
	if err != nil {
		t.Fatal(err)
	}
	return rep.InScope
}

func TestVerifyRecordsTheTreeItsVerdictWasAbout(t *testing.T) {
	// A verdict with no record of what it was about cannot expire, and a
	// verdict that cannot expire is the false confidence this tool refuses:
	// five edits later it is still the last thing anyone was told.
	root := newVerdictRepo(t)
	rep, err := Run(Options{Dir: root, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}

	snap, ok := freshness.Last(root, "task")
	if !ok {
		t.Fatal("a verdict was reached and nothing was recorded about the tree")
	}
	if snap.Verdict != string(rep.Verdict) {
		t.Errorf("recorded verdict = %q, want %q", snap.Verdict, rep.Verdict)
	}
	// "Passed" and the exit code must never disagree about the same run.
	if snap.Passed != (rep.ExitCode() == ExitOK) {
		t.Errorf("recorded passed = %v, exit code = %d", snap.Passed, rep.ExitCode())
	}
	// Every file the audit placed in scope has to be in there. A snapshot that
	// covered only some of them would answer "not stale" about files it never
	// looked at.
	for _, f := range rep.Scope.InScope {
		if _, ok := snap.Files[f]; !ok {
			t.Errorf("in-scope file %s was not recorded", f)
		}
	}
	if len(snap.Files) == 0 {
		t.Error("no files recorded; the verdict was about nothing")
	}

	// And it is a true statement about the tree right now.
	if st := freshness.Compare(root, snap, inScopeNow(t, root)); st.Stale() {
		t.Errorf("the verdict was stale the moment it was reached: %s", st.Message())
	}
}

func TestEditingAnInScopeFileAfterAVerdictMakesItStale(t *testing.T) {
	// The case the whole feature exists for: someone verified, kept editing,
	// and the last thing they heard is now a claim about a tree that is gone.
	root := newVerdictRepo(t)
	if _, err := Run(Options{Dir: root, Timeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	snap, ok := freshness.Last(root, "task")
	if !ok {
		t.Fatal("no verdict recorded")
	}

	mk(t, root, "x.go", "package x\n\nfunc F() int { return 2 }\n")
	st := freshness.Compare(root, snap, inScopeNow(t, root))
	if !st.Stale() {
		t.Fatal("an edit after the verdict left it fresh")
	}
	if !strings.Contains(st.Message(), "x.go") {
		t.Errorf("message = %q, want the edited file named", st.Message())
	}
}

func TestARunWithNoTaskRecordsNoVerdict(t *testing.T) {
	// Without a declared scope there is no task a verdict can belong to, and
	// filing one under a guessed key would answer a later question about the
	// wrong work.
	root := newRepo(t)
	if _, err := Run(Options{Dir: root, Timeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(freshness.Path(root)); !os.IsNotExist(err) {
		t.Errorf("verdict state exists (%v), want none without a task", err)
	}
}

func TestAnUnwritableVerdictStateDoesNotFailVerify(t *testing.T) {
	// Bookkeeping is not the answer verify owes its caller. A repository where
	// the snapshot cannot be written still gets its verdict.
	root := newVerdictRepo(t)
	if err := os.MkdirAll(freshness.Path(root), 0o755); err != nil {
		t.Fatal(err)
	}
	rep, err := Run(Options{Dir: root, Timeout: time.Minute})
	if err != nil {
		t.Fatalf("Run failed over state it could not write: %v", err)
	}
	if rep.Verdict != Verified {
		t.Errorf("verdict = %s (%s), want VERIFIED", rep.Verdict, rep.Reason)
	}
}

func TestAStaleVerdictNeverChangesTheVerdictOrTheExitCode(t *testing.T) {
	// Stale is not failed; it is unknown. A run that finds a stale record from
	// a previous run must land exactly where it would have landed without one.
	root := newVerdictRepo(t)
	if err := freshness.Record(root, freshness.Snapshot{
		At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Task: "task",
		Verdict: string(Verified), Passed: true,
		Files: map[string]string{"x.go": "0000000000000000000000000000000000000000"},
	}); err != nil {
		t.Fatal(err)
	}

	rep, err := Run(Options{Dir: root, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != Verified {
		t.Errorf("verdict = %s (%s), want VERIFIED despite the stale record", rep.Verdict, rep.Reason)
	}
	if rep.ExitCode() != ExitOK {
		t.Errorf("ExitCode = %d, want %d; staleness decides nothing", rep.ExitCode(), ExitOK)
	}
}

func TestVerifySaysNothingAboutStaleness(t *testing.T) {
	// verify has just re-verified. The only stale verdict it could name is the
	// one it is replacing in the same breath, so the line would be false by the
	// time the reader reached it. The Stop hook is where nobody re-verified and
	// the question has an answer worth hearing.
	root := newVerdictRepo(t)
	if _, err := Run(Options{Dir: root, Timeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	mk(t, root, "x.go", "package x\n\nfunc F() int { return 3 }\n")

	rep, err := Run(Options{Dir: root, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := rep.WriteText(&out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(out.String()), "stale") {
		t.Errorf("verify reported staleness about its own run:\n%s", out.String())
	}

	// It did replace the record, though: the snapshot has to describe the tree
	// this run was about, or the Stop hook answers with the older run's hashes.
	snap, ok := freshness.Last(root, "task")
	if !ok {
		t.Fatal("no verdict recorded")
	}
	if st := freshness.Compare(root, snap, inScopeNow(t, root)); st.Stale() {
		t.Errorf("the second run did not replace the first's snapshot: %s", st.Message())
	}
}

// commitAll stages and commits the whole tree, so a later change has something
// to be a change *from*. Without a commit there is no HEAD, and a diff has no
// prior state to subtract against.
func commitAll(t *testing.T, root string) {
	t.Helper()
	for _, args := range [][]string{
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
		{"add", "-A"},
		{"commit", "-qm", "base"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}

func TestAChangeThatGutsItsOwnTestIsNotVerified(t *testing.T) {
	// The blind spot this closes: the suite exits zero because there is
	// nothing left in it to fail. Every command passes, the change is in
	// scope, and the verdict would otherwise read VERIFIED.
	root := newRepo(t)
	mk(t, root, "x_test.go", `package x

import "testing"

func TestF(t *testing.T) {
	if F() != 1 {
		t.Error("F broke")
	}
	if F() < 0 {
		t.Error("F went negative")
	}
}
`)
	mk(t, root, ".vector/policy.toml", "[mode]\nenforcement = \"advisory\"\n")
	mk(t, root, ".vector/scope/task.toml", "objective = \"o\"\nwrite = [\"**\"]\n")
	mk(t, root, ".vector/current", "task\n")
	commitAll(t, root)

	// What a reward-hacking agent does: the assertions go, the test remains,
	// and `go test ./...` is delighted.
	mk(t, root, "x_test.go", `package x

import "testing"

func TestF(t *testing.T) {}
`)

	rep, err := Run(Options{Dir: root, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != PartiallyVerified {
		t.Fatalf("verdict = %s (%s), want PARTIALLY_VERIFIED", rep.Verdict, rep.Reason)
	}
	if !strings.Contains(rep.Reason, "x_test.go") {
		t.Errorf("reason = %q, want it to name the file it is about", rep.Reason)
	}
	// Lowered, not failed. Nothing here justifies a non-zero exit: the change
	// may be a legitimate consolidation, and vector cannot tell.
	if rep.ExitCode() != ExitOK {
		t.Errorf("ExitCode = %d, want %d — this lowers a claim, it does not fail a build",
			rep.ExitCode(), ExitOK)
	}
}

func TestAChangeThatAddsTestsIsStillVerified(t *testing.T) {
	// The ordinary, good case. A tool that complains when tests are added is
	// a tool people turn off.
	root := newRepo(t)
	mk(t, root, "x_test.go", "package x\n\nimport \"testing\"\n\nfunc TestF(t *testing.T) {}\n")
	mk(t, root, ".vector/policy.toml", "[mode]\nenforcement = \"advisory\"\n")
	mk(t, root, ".vector/scope/task.toml", "objective = \"o\"\nwrite = [\"**\"]\n")
	mk(t, root, ".vector/current", "task\n")
	commitAll(t, root)

	mk(t, root, "x_test.go", `package x

import "testing"

func TestF(t *testing.T) {
	if F() != 1 {
		t.Error("F broke")
	}
}
`)

	rep, err := Run(Options{Dir: root, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != Verified {
		t.Errorf("verdict = %s (%s), want VERIFIED", rep.Verdict, rep.Reason)
	}
}

func TestShrinkingOrdinarySourceIsNotATestFinding(t *testing.T) {
	// Deleting production code is what most changes do. Only the files that
	// do the judging count, or the finding means nothing.
	root := newRepo(t)
	mk(t, root, "x.go", "package x\n\nfunc F() int {\n\ta := 1\n\tb := 0\n\tc := 0\n\treturn a + b + c\n}\n")
	mk(t, root, "x_test.go", "package x\n\nimport \"testing\"\n\nfunc TestF(t *testing.T) {\n\tif F() != 1 {\n\t\tt.Error(\"no\")\n\t}\n}\n")
	mk(t, root, ".vector/policy.toml", "[mode]\nenforcement = \"advisory\"\n")
	mk(t, root, ".vector/scope/task.toml", "objective = \"o\"\nwrite = [\"**\"]\n")
	mk(t, root, ".vector/current", "task\n")
	commitAll(t, root)

	mk(t, root, "x.go", "package x\n\nfunc F() int { return 1 }\n")

	rep, err := Run(Options{Dir: root, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != Verified {
		t.Errorf("verdict = %s (%s), want VERIFIED", rep.Verdict, rep.Reason)
	}
}

func TestWeakeningIsFoundAcrossCommitsAgainstABase(t *testing.T) {
	// The CI case, and the one the feature was silently useless for. A fresh
	// checkout has a working tree identical to HEAD, so every diff against
	// HEAD is empty however many commits back the suite was gutted. Only the
	// ref the branch came from can see it.
	root := newRepo(t)
	mk(t, root, "x_test.go", `package x

import "testing"

func TestF(t *testing.T) {
	if F() != 1 {
		t.Error("F broke")
	}
	if F() < 0 {
		t.Error("F went negative")
	}
}
`)
	mk(t, root, ".vector/policy.toml", "[mode]\nenforcement = \"advisory\"\n")
	mk(t, root, ".vector/scope/task.toml", "objective = \"o\"\nwrite = [\"**\"]\n")
	mk(t, root, ".vector/current", "task\n")
	commitAll(t, root)

	cmd := exec.Command("git", "branch", "base")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git branch: %v: %s", err, out)
	}

	// The gutting happens in a commit, and the tree is clean afterwards.
	mk(t, root, "x_test.go", "package x\n\nimport \"testing\"\n\nfunc TestF(t *testing.T) {}\n")
	commitAll(t, root)

	// Against HEAD there is nothing to see, and that is the honest answer to
	// the question "what has this session done".
	rep, err := Run(Options{Dir: root, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != Verified {
		t.Errorf("against HEAD: verdict = %s (%s), want VERIFIED on a clean tree",
			rep.Verdict, rep.Reason)
	}

	// Against the branch point it is the whole change, and it is found.
	rep, err = Run(Options{Dir: root, Base: "base", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != PartiallyVerified {
		t.Fatalf("against base: verdict = %s (%s), want PARTIALLY_VERIFIED",
			rep.Verdict, rep.Reason)
	}
	if !strings.Contains(rep.Reason, "x_test.go") {
		t.Errorf("reason = %q, want it to name the file", rep.Reason)
	}
}

func TestRenamingATestIsNotWeakeningIt(t *testing.T) {
	// A `git mv` removes nothing. Reporting it as "the suite that passed is
	// not the suite that was there" is the false positive that gets a tool
	// uninstalled.
	root := newRepo(t)
	mk(t, root, "x_test.go", `package x

import "testing"

func TestF(t *testing.T) {
	if F() != 1 {
		t.Error("F broke")
	}
	if F() < 0 {
		t.Error("F went negative")
	}
}
`)
	mk(t, root, ".vector/policy.toml", "[mode]\nenforcement = \"advisory\"\n")
	mk(t, root, ".vector/scope/task.toml", "objective = \"o\"\nwrite = [\"**\"]\n")
	mk(t, root, ".vector/current", "task\n")
	commitAll(t, root)

	cmd := exec.Command("git", "mv", "x_test.go", "renamed_test.go")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git mv: %v: %s", err, out)
	}

	rep, err := Run(Options{Dir: root, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != Verified {
		t.Errorf("verdict = %s (%s), want VERIFIED — a rename removed nothing",
			rep.Verdict, rep.Reason)
	}
}

func TestBothFindingsAreReportedWhenBothAreTrue(t *testing.T) {
	// Dropping either would hide a finding behind an unrelated one.
	root := newRepo(t)
	mk(t, root, "x_test.go", `package x

import "testing"

func TestF(t *testing.T) {
	if F() != 1 {
		t.Error("F broke")
	}
	if F() < 0 {
		t.Error("F went negative")
	}
}
`)
	commitAll(t, root)
	mk(t, root, "x_test.go", "package x\n\nimport \"testing\"\n\nfunc TestF(t *testing.T) {}\n")

	rep, err := Run(Options{Dir: root, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != PartiallyVerified {
		t.Fatalf("verdict = %s (%s), want PARTIALLY_VERIFIED", rep.Verdict, rep.Reason)
	}
	if !strings.Contains(rep.Reason, "x_test.go") {
		t.Errorf("reason = %q, does not name the weakened suite", rep.Reason)
	}
	if !strings.Contains(rep.Reason, "no scope") {
		t.Errorf("reason = %q, does not mention that no scope was declared", rep.Reason)
	}
}

func TestABoundaryThatIsNotAboutMigrationsDoesNotAuthoriseOne(t *testing.T) {
	// The hole this closes. The objective was to move a button; the diff drops
	// a table. The boundary did not fail — "**" makes every path in scope by
	// construction, so the audit reads IN_SCOPE and says nothing at all.
	root := newRepo(t)
	mk(t, root, ".vector/policy.toml", "[mode]\nenforcement = \"advisory\"\n")
	mk(t, root, ".vector/scope/task.toml", "objective = \"tweak a button\"\nwrite = [\"src/**\"]\n")
	mk(t, root, ".vector/current", "task\n")
	commitAll(t, root)
	mk(t, root, "src/migrations/0003_drop.sql", "DROP TABLE customers;\n")

	rep, err := Run(Options{Dir: root, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != PartiallyVerified {
		t.Fatalf("verdict = %s (%s), want PARTIALLY_VERIFIED", rep.Verdict, rep.Reason)
	}
	if !strings.Contains(rep.Reason, "src/migrations/0003_drop.sql") {
		t.Errorf("reason = %q, want it to name the path", rep.Reason)
	}
	if !strings.Contains(rep.Reason, "no declared pattern was about") {
		t.Errorf("reason = %q, want it to say the declaration was not about it", rep.Reason)
	}
	// Reported, never blocked. The migration may well belong to the task, and
	// vector has no way to know — it says only that nothing declared it.
	if rep.ExitCode() != ExitOK {
		t.Errorf("ExitCode = %d, want %d", rep.ExitCode(), ExitOK)
	}
}

func TestANamedBoundaryAuthorisesAMigration(t *testing.T) {
	// The exemption that keeps this from being noise. A boundary that names
	// where the work lives is a claim someone made and can be held to; only
	// the refusal to make one is the finding.
	root := newRepo(t)
	mk(t, root, ".vector/policy.toml", "[mode]\nenforcement = \"advisory\"\n")
	mk(t, root, ".vector/scope/task.toml",
		"objective = \"add the drop-customers migration\"\nwrite = [\"migrations/**\"]\n")
	mk(t, root, ".vector/current", "task\n")
	commitAll(t, root)
	mk(t, root, "migrations/0003_drop.sql", "DROP TABLE customers;\n")

	rep, err := Run(Options{Dir: root, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != Verified {
		t.Errorf("verdict = %s (%s), want VERIFIED", rep.Verdict, rep.Reason)
	}
}

func TestABroadBoundaryOverOrdinarySourceIsStillVerified(t *testing.T) {
	// A repository-wide task is a legitimate declaration. "**" only becomes a
	// finding when it is the thing that swept up something dangerous.
	root := newRepo(t)
	mk(t, root, ".vector/policy.toml", "[mode]\nenforcement = \"advisory\"\n")
	mk(t, root, ".vector/scope/task.toml", "objective = \"rename a symbol everywhere\"\nwrite = [\"**\"]\n")
	mk(t, root, ".vector/current", "task\n")
	commitAll(t, root)
	mk(t, root, "x.go", "package x\n\nfunc F() int { return 2 }\n")

	rep, err := Run(Options{Dir: root, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != Verified {
		t.Errorf("verdict = %s (%s), want VERIFIED", rep.Verdict, rep.Reason)
	}
}
