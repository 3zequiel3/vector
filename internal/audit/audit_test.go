package audit

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func newRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
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

// commitAll makes the current tree the baseline, so a later diff shows only
// what the test itself changed.
func commitAll(t *testing.T, root string) {
	t.Helper()
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "base"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}

func scoped(t *testing.T, root string) {
	t.Helper()
	mk(t, root, ".vector/policy.toml",
		"[scope]\nalways_forbidden = [\".vector/**\", \".claude/settings.json\", \".env\"]\n\n"+
			"[mode]\nenforcement = \"advisory\"\n")
	mk(t, root, ".vector/scope/task.toml", "objective = \"add a thing\"\nwrite = [\"src/**\"]\n")
	mk(t, root, ".vector/current", "task\n")
}

func TestVectorsOwnFilesAreNotAFinding(t *testing.T) {
	// Declaring a scope writes .vector/scope/<task>.toml, and .vector/** is
	// forbidden so the agent cannot edit its own constraints. Before this,
	// every task opened with a FORBIDDEN verdict about vector's own footprint —
	// and a tool whose first answer on every task is a false alarm teaches
	// people to ignore its answers.
	root := newRepo(t)
	scoped(t, root)
	commitAll(t, root) // the policy is already in the tree; only the task moves it now
	mk(t, root, ".vector/observations.md", "# Observations\n")
	mk(t, root, ".vector/.gitignore", "current\n")
	mk(t, root, "src/a.ts", "export {}\n")

	rep, err := Run(Options{Dir: root})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != InScope {
		t.Fatalf("status = %s, want IN_SCOPE (findings: %+v)", rep.Status, rep.Findings)
	}
	for _, want := range []string{".vector/observations.md", ".vector/.gitignore"} {
		var found bool
		for _, b := range rep.Bookkeeping {
			if b == want {
				found = true
			}
		}
		if !found {
			t.Errorf("bookkeeping = %v, want it to include %s", rep.Bookkeeping, want)
		}
	}
	if rep.ExitCode() != ExitOK {
		t.Errorf("ExitCode = %d, want 0", rep.ExitCode())
	}
}

func TestThePolicyIsStillAHardFinding(t *testing.T) {
	// policy.toml is the enforcement contract. A change to it is exactly the
	// thing worth reporting, which is why it is not bookkeeping.
	root := newRepo(t)
	scoped(t, root)
	mk(t, root, "src/a.ts", "export {}\n")

	rep, err := Run(Options{Dir: root})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != Forbidden {
		t.Fatalf("status = %s, want FORBIDDEN for an uncommitted policy.toml", rep.Status)
	}
	var named bool
	for _, f := range rep.Findings {
		if f.Path == ".vector/policy.toml" {
			named = true
		}
	}
	if !named {
		t.Errorf("findings = %+v, want policy.toml named", rep.Findings)
	}
}

func TestOnlyBookkeepingMeansNothingToReview(t *testing.T) {
	root := newRepo(t)
	mk(t, root, ".vector/scope/task.toml", "objective = \"x\"\nwrite = [\"src/**\"]\n")
	mk(t, root, ".vector/current", "task\n")

	rep, err := Run(Options{Dir: root})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != NoChanges {
		t.Errorf("status = %s, want NO_CHANGES when only vector's own files moved", rep.Status)
	}
}

func TestTheEnforcementConfigOfTheAgentIsStillAFinding(t *testing.T) {
	// The hook and sandbox live in .claude/settings.json. Vector writes it at
	// init, but a change to it during a task is the self-weakening case the
	// forbidden list exists for.
	root := newRepo(t)
	scoped(t, root)
	commitAll(t, root)
	mk(t, root, ".claude/settings.json", "{}\n")

	rep, err := Run(Options{Dir: root})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != Forbidden {
		t.Errorf("status = %s, want FORBIDDEN (findings: %+v)", rep.Status, rep.Findings)
	}
}
