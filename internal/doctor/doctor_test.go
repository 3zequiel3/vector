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

// isolate replaces PATH with a directory holding only git, and HOME with an
// empty one, so the integration checks answer about a known machine instead of
// about whatever the developer running the suite happens to have installed.
// git stays because doctor cannot resolve a repository without it — everything
// else being gone is the point.
func isolate(t *testing.T) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed; doctor cannot run at all without it")
	}
	bin := t.TempDir()
	if err := os.Symlink(git, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("HOME", t.TempDir())
	return bin
}

// integration returns the check for an integration by name, and fails when the
// group forgot one: an integration silently dropped from the report is worse
// than one reported absent.
func integrationCheck(t *testing.T, r Report, name string) Check {
	t.Helper()
	return find(t, r, "integrations", name)
}

func TestEveryIntegrationIsReportedAbsentWithoutFailing(t *testing.T) {
	// Absent optional software is a fact, not a finding. If any of these came
	// back warn or fail, doctor would read as a list of unmet dependencies for
	// a tool whose only dependency is git.
	isolate(t)
	root := newRepo(t)

	rep, err := Run(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gentle-ai", "openspec", "chronicle", "atlas", "engram"} {
		c := integrationCheck(t, rep, name)
		if c.Level != "info" {
			t.Errorf("%s: level = %q, want info — absence is not a warning", name, c.Level)
		}
		if c.Present {
			t.Errorf("%s: present = true on an isolated machine (%s)", name, c.Detail)
		}
		if !strings.Contains(c.Detail, "optional") {
			t.Errorf("%s: detail = %q, want it to say the piece is optional", name, c.Detail)
		}
		if c.Unlocks == "" {
			t.Errorf("%s: no unlocks — absence is only meaningful next to what it costs", name)
		}
	}
	if rep.ExitCode() != 0 {
		t.Errorf("ExitCode = %d, want 0: no optional piece may fail the run", rep.ExitCode())
	}
}

func TestGitIsNamedTheOnlyHardRequirement(t *testing.T) {
	isolate(t)
	root := newRepo(t)

	rep, err := Run(root)
	if err != nil {
		t.Fatal(err)
	}
	c := integrationCheck(t, rep, "git")
	if c.Level != "ok" {
		t.Errorf("level = %q, want ok — git demonstrably answered", c.Level)
	}
	if !strings.Contains(c.Detail, "only hard requirement") {
		t.Errorf("detail = %q, want it to state that git is the only hard requirement", c.Detail)
	}
}

func TestRepositoryIntegrationsAreDetectedFromTheirArtifacts(t *testing.T) {
	// Each of these is detected by a file the tool actually writes, so the
	// evidence in the report is something that exists rather than a guess.
	isolate(t)
	root := newRepo(t)
	write(t, root, "openspec/changes/x/proposal.md", "# x\n")
	write(t, root, ".ledger/fingerprints.json", "{}\n")
	write(t, root, "CHANGES.md", "# changes\n")

	rep, err := Run(root)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"openspec":  "openspec/",
		"chronicle": ".ledger/fingerprints.json",
		"atlas":     "CHANGES.md",
	} {
		c := integrationCheck(t, rep, name)
		if !c.Present {
			t.Errorf("%s: present = false, detail = %q", name, c.Detail)
		}
		if !strings.Contains(c.Detail, want) {
			t.Errorf("%s: detail = %q, want the evidence %q named", name, c.Detail, want)
		}
	}
}

func TestPresentIntegrationsSayVectorDoesNotReadThem(t *testing.T) {
	// The failure this guards against is the one doctor exists to refuse:
	// listing a piece as present and letting the reader conclude vector is
	// already using it.
	isolate(t)
	root := newRepo(t)
	write(t, root, "CHANGES.md", "# changes\n")

	rep, err := Run(root)
	if err != nil {
		t.Fatal(err)
	}
	c := integrationCheck(t, rep, "atlas")
	if c.Use != string(notYet) {
		t.Errorf("use = %q, want %q — nothing in vector reads CHANGES.md today", c.Use, notYet)
	}
	if !strings.Contains(c.Detail, "does not read it yet") {
		t.Errorf("detail = %q, want it to say vector does not read it yet", c.Detail)
	}
}

func TestEngramIsPresentAndReportedUnreadable(t *testing.T) {
	// engram is the one piece that does not become usable by writing more
	// vector code, so it must not be reported as merely "not yet".
	isolate(t)
	root := newRepo(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	write(t, home, ".engram/engram.db", "not really sqlite")

	rep, err := Run(root)
	if err != nil {
		t.Fatal(err)
	}
	c := integrationCheck(t, rep, "engram")
	if !c.Present {
		t.Fatalf("present = false with the db in place: %q", c.Detail)
	}
	if c.Use != string(unreadable) {
		t.Errorf("use = %q, want %q", c.Use, unreadable)
	}
	if !strings.Contains(c.Detail, "cannot read it") {
		t.Errorf("detail = %q, want it to say vector cannot read it", c.Detail)
	}
}

func TestGentleAIVersionIsCapturedWhenTheBinaryReportsOne(t *testing.T) {
	bin := isolate(t)
	fakeBinary(t, bin, "gentle-ai", "#!/bin/sh\necho 'gentle-ai 9.9.9'\n")
	root := newRepo(t)

	rep, err := Run(root)
	if err != nil {
		t.Fatal(err)
	}
	c := integrationCheck(t, rep, "gentle-ai")
	if !c.Present {
		t.Fatalf("present = false with the binary on PATH: %q", c.Detail)
	}
	if !strings.Contains(c.Detail, "9.9.9") {
		t.Errorf("detail = %q, want the reported version", c.Detail)
	}
	if strings.Contains(c.Detail, "gentle-ai 9.9.9") {
		t.Errorf("detail = %q, want the binary's own name dropped from its answer", c.Detail)
	}
}

func TestAnUncooperativeBinaryIsStillReportedPresent(t *testing.T) {
	// The version flag differs between tools and between versions of one tool.
	// Not getting an answer says nothing about whether the binary is installed,
	// and must not be allowed to hide it.
	cases := []struct {
		name   string
		script string
	}{
		{"exits non-zero", "#!/bin/sh\necho 'unknown flag' >&2\nexit 1\n"},
		{"prints usage", "#!/bin/sh\necho 'Usage: gentle-ai <command> [options]'\n"},
		{"prints nothing", "#!/bin/sh\nexit 0\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin := isolate(t)
			fakeBinary(t, bin, "gentle-ai", tc.script)
			root := newRepo(t)

			rep, err := Run(root)
			if err != nil {
				t.Fatal(err)
			}
			c := integrationCheck(t, rep, "gentle-ai")
			if !c.Present {
				t.Fatalf("present = false: %q", c.Detail)
			}
			if !strings.Contains(c.Detail, "version not reported") {
				t.Errorf("detail = %q, want it to admit the version is unknown", c.Detail)
			}
		})
	}
}

// fakeBinary puts a real executable on the isolated PATH. Stubbing exec would
// test the stub; a script that a shell actually runs tests what doctor does.
func fakeBinary(t *testing.T, dir, name, script string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}
