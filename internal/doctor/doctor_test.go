package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newRepo builds a real git repository, because doctor's answers come from git
// and stubbing it would test the stub.
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

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// find returns the first check matching group and name.
func find(t *testing.T, r Report, group, name string) Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.Group == group && c.Name == name {
			return c
		}
	}
	t.Fatalf("no %q/%q check in %+v", group, name, r.Checks)
	return Check{}
}

func TestDeadScopePatternIsCaught(t *testing.T) {
	// A pattern matching nothing is the worst available outcome: the boundary
	// looks declared while silently excluding everything the task needs.
	root := newRepo(t)
	write(t, root, "src/app.ts", "export {}")
	write(t, root, ".vector/scope/task.toml", "objective = \"x\"\nwrite = [\"src/**\", \"sorc/**\"]\n")

	rep, err := Run(root)
	if err != nil {
		t.Fatal(err)
	}
	c := find(t, rep, "scopes", "task")
	if c.Level != "warn" {
		t.Fatalf("level = %q, want warn", c.Level)
	}
	if !strings.Contains(c.Detail, "sorc/**") {
		t.Errorf("detail = %q, want the dead pattern named", c.Detail)
	}
	if strings.Contains(c.Detail, "src/**") {
		t.Errorf("detail = %q, want the live pattern left out", c.Detail)
	}
}

func TestUncommittedFilesStillCountAsInventory(t *testing.T) {
	// With no commits, tracked files are empty. Reporting every pattern as dead
	// would make doctor useless on a fresh repository.
	root := newRepo(t)
	write(t, root, "src/app.ts", "export {}")
	write(t, root, ".vector/scope/task.toml", "objective = \"x\"\nwrite = [\"src/**\"]\n")

	rep, err := Run(root)
	if err != nil {
		t.Fatal(err)
	}
	if c := find(t, rep, "scopes", "task"); c.Level != "ok" {
		t.Errorf("level = %q (%s), want ok on an uncommitted tree", c.Level, c.Detail)
	}
}

func TestSelfProtectionGapIsAFailure(t *testing.T) {
	// An agent that can edit the rules constraining it is not constrained.
	root := newRepo(t)
	write(t, root, ".vector/policy.toml",
		"[scope]\nalways_forbidden = [\"secretos/**\"]\n\n[mode]\nenforcement = \"advisory\"\n")

	rep, err := Run(root)
	if err != nil {
		t.Fatal(err)
	}
	if c := find(t, rep, "repository", "self-protection"); c.Level != "fail" {
		t.Errorf("level = %q, want fail when .vector/ is writable", c.Level)
	}
	if rep.ExitCode() != 1 {
		t.Errorf("ExitCode = %d, want 1 when a check failed", rep.ExitCode())
	}
}

func TestEmptyScopeIsAFailure(t *testing.T) {
	root := newRepo(t)
	write(t, root, ".vector/scope/task.toml", "objective = \"x\"\nwrite = []\n")

	rep, err := Run(root)
	if err != nil {
		t.Fatal(err)
	}
	if c := find(t, rep, "scopes", "task"); c.Level != "fail" {
		t.Errorf("level = %q, want fail — an empty scope excludes everything", c.Level)
	}
}

func TestBrokenPolicyIsAFailureNotACrash(t *testing.T) {
	root := newRepo(t)
	write(t, root, ".vector/policy.toml", "this is not valid TOML = = =\n")

	rep, err := Run(root)
	if err != nil {
		t.Fatalf("doctor must diagnose, not fail: %v", err)
	}
	if c := find(t, rep, "repository", "policy.toml"); c.Level != "fail" {
		t.Errorf("level = %q, want fail", c.Level)
	}
}

func TestTierIsT2WithoutAnyHook(t *testing.T) {
	// Nothing may claim prevention that was not verified.
	root := newRepo(t)
	rep, err := Run(root)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Tier != "T2" {
		t.Errorf("tier = %q, want T2 with no hook registered anywhere", rep.Tier)
	}
}

func TestWarningsDoNotFailTheExitCode(t *testing.T) {
	// Degraded enforcement is still enforcement. Treating a warning as a
	// failure trains people to ignore the output.
	root := newRepo(t)
	rep, err := Run(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range rep.Checks {
		if c.Level == "fail" {
			t.Fatalf("unexpected failing check: %+v", c)
		}
	}
	if rep.ExitCode() != 0 {
		t.Errorf("ExitCode = %d, want 0 when only warnings are present", rep.ExitCode())
	}
}

func TestHookDetectionNeedsAnInvocationNotTheWord(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{`{"hooks":{"PreToolUse":[{"command":"vector hook pre-tool"}]}}`, true},
		{`{"hooks":{"Stop":[{"command":"/usr/local/bin/vector audit -task x"}]}}`, true},
		{`{"comment":"algún día vamos a usar vector acá"}`, false},
		{`{"path":"/home/me/Proyectos/vector/src"}`, false},
	}
	for _, c := range cases {
		if got := mentionsVectorHook([]byte(c.text)); got != c.want {
			t.Errorf("mentionsVectorHook(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}
