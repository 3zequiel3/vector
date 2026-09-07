package verify

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
