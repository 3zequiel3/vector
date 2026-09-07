package hook

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteTargetsFindsRedirections(t *testing.T) {
	// These are the shapes that slip past a hook watching only Edit and Write.
	cases := []struct {
		cmd  string
		want []string
	}{
		{"cat > src/a.ts << EOF", []string{"src/a.ts"}},
		{"echo hi >> notes.md", []string{"notes.md"}},
		{"echo hi>notes.md", []string{"notes.md"}},
		{"build 2> errors.log", []string{"errors.log"}},
		{"build &> all.log", []string{"all.log"}},
		{"printf x | tee out.txt", []string{"out.txt"}},
		{"cd src && echo y > z.ts", []string{"z.ts"}},
		{"echo a > one.txt; echo b > two.txt", []string{"one.txt", "two.txt"}},
		{`echo "spaced name" > "my file.txt"`, []string{"my file.txt"}},
	}
	for _, c := range cases {
		got := WriteTargets(c.cmd)
		if !sameSet(got, c.want) {
			t.Errorf("WriteTargets(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

func TestWriteTargetsFindsWritingCommands(t *testing.T) {
	cases := []struct {
		cmd  string
		want []string
	}{
		{"mv old.ts new.ts", []string{"new.ts"}},
		{"cp a.ts b.ts", []string{"b.ts"}},
		{"rm secrets.env", []string{"secrets.env"}},
		{"touch new.ts", []string{"new.ts"}},
		{"mkdir -p src/deep", []string{"src/deep"}},
		{"sed -i s/a/b/ config.ts", []string{"config.ts"}},
		{"sd foo bar -i config.ts", []string{"config.ts"}},
		{"/usr/bin/rm x.ts", []string{"x.ts"}},
	}
	for _, c := range cases {
		got := WriteTargets(c.cmd)
		if !sameSet(got, c.want) {
			t.Errorf("WriteTargets(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

func TestWriteTargetsIgnoresReads(t *testing.T) {
	// Over-reporting costs one explained denial, but reporting a read as a
	// write would deny ordinary exploration and make vector unusable.
	for _, cmd := range []string{
		"cat src/a.ts", "rg pattern src/", "ls -la", "git status",
		"sed s/a/b/ config.ts", "go test ./...", "pnpm run build",
	} {
		if got := WriteTargets(cmd); len(got) != 0 {
			t.Errorf("WriteTargets(%q) = %v, want none", cmd, got)
		}
	}
}

func TestWriteTargetsSkipsUnexpandableWords(t *testing.T) {
	// A path vector cannot resolve must not be reported as a concrete file:
	// denying "$OUT" would be denying something nobody can verify.
	for _, cmd := range []string{"echo x > $OUT", "echo x > `mktemp`"} {
		if got := WriteTargets(cmd); len(got) != 0 {
			t.Errorf("WriteTargets(%q) = %v, want none", cmd, got)
		}
	}
}

// newRepo builds a repository with a policy and an active scope.
func newRepo(t *testing.T, write []string) string {
	t.Helper()
	root := t.TempDir()
	cmd := exec.Command("git", "init", "-q", "-b", "main")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	mk := func(rel, content string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk(".vector/policy.toml",
		"[scope]\nalways_forbidden = [\".vector/**\", \".env\"]\n\n[mode]\nenforcement = \"advisory\"\n")
	mk(".vector/scope/task.toml",
		"objective = \"add a filter\"\nwrite = [\""+strings.Join(write, "\", \"")+"\"]\n")
	mk(".vector/current", "task\n")
	return root
}

func runEvent(t *testing.T, root, event, payload string) hookOutput {
	t.Helper()
	var out bytes.Buffer
	if err := Run(event, strings.NewReader(payload), &out, root); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Len() == 0 {
		return hookOutput{}
	}
	var r response
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatalf("unmarshal %q: %v", out.String(), err)
	}
	if r.Hook == nil {
		return hookOutput{}
	}
	return *r.Hook
}

func TestPreToolNeverAllows(t *testing.T) {
	// Returning "allow" would auto-approve the call and quietly weaken the
	// permission rules the user configured. vector denies or stays quiet.
	root := newRepo(t, []string{"src/**"})
	for _, path := range []string{"src/ok.ts", "elsewhere.ts", ".env"} {
		payload := `{"cwd":"` + root + `","tool_name":"Write","tool_input":{"file_path":"` + path + `"}}`
		if got := runEvent(t, root, "pre-tool", payload).PermissionDecision; got == "allow" {
			t.Errorf("path %q produced permissionDecision=allow", path)
		}
	}
}

func TestPreToolStaysSilentInsideTheScope(t *testing.T) {
	root := newRepo(t, []string{"src/**"})
	payload := `{"cwd":"` + root + `","tool_name":"Write","tool_input":{"file_path":"src/ok.ts"}}`
	if got := runEvent(t, root, "pre-tool", payload); got != (hookOutput{}) {
		t.Errorf("got %+v, want no output for an in-scope write", got)
	}
}

func TestForbiddenIsDeniedEvenInAdvisoryMode(t *testing.T) {
	// Advisory softens the boundary, not the rules that stop the boundary from
	// being edited away.
	root := newRepo(t, []string{"src/**"})
	payload := `{"cwd":"` + root + `","tool_name":"Write","tool_input":{"file_path":".env"}}`
	got := runEvent(t, root, "pre-tool", payload)
	if got.PermissionDecision != "deny" {
		t.Fatalf("decision = %q, want deny", got.PermissionDecision)
	}
	if !strings.Contains(got.PermissionDecisionReason, ".env") {
		t.Errorf("reason = %q, want the path named", got.PermissionDecisionReason)
	}
}

func TestOutOfScopeIsReportedNotBlockedInAdvisoryMode(t *testing.T) {
	root := newRepo(t, []string{"src/**"})
	payload := `{"cwd":"` + root + `","tool_name":"Write","tool_input":{"file_path":"other/x.ts"}}`
	got := runEvent(t, root, "pre-tool", payload)
	if got.PermissionDecision != "" {
		t.Errorf("decision = %q, want none in advisory mode", got.PermissionDecision)
	}
	if !strings.Contains(got.AdditionalContext, "other/x.ts") {
		t.Errorf("context = %q, want the path named", got.AdditionalContext)
	}
	if !strings.Contains(got.AdditionalContext, "vector observe") {
		t.Errorf("context = %q, want the defer path offered", got.AdditionalContext)
	}
}

func TestStrictModeDeniesOutOfScope(t *testing.T) {
	root := newRepo(t, []string{"src/**"})
	p := filepath.Join(root, ".vector", "policy.toml")
	if err := os.WriteFile(p,
		[]byte("[scope]\nalways_forbidden = [\".vector/**\"]\n\n[mode]\nenforcement = \"strict\"\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	payload := `{"cwd":"` + root + `","tool_name":"Write","tool_input":{"file_path":"other/x.ts"}}`
	got := runEvent(t, root, "pre-tool", payload)
	if got.PermissionDecision != "deny" {
		t.Fatalf("decision = %q, want deny in strict mode", got.PermissionDecision)
	}
	if !strings.Contains(got.PermissionDecisionReason, "scope expand") {
		t.Errorf("reason = %q, want the recorded way forward", got.PermissionDecisionReason)
	}
}

func TestBashWritesAreDecidedToo(t *testing.T) {
	// The whole point of reading the command: a heredoc reaches the same file
	// the Write tool would have.
	root := newRepo(t, []string{"src/**"})
	payload := `{"cwd":"` + root + `","tool_name":"Bash","tool_input":{"command":"cat > .env << EOF"}}`
	if got := runEvent(t, root, "pre-tool", payload).PermissionDecision; got != "deny" {
		t.Errorf("decision = %q, want deny for a heredoc into a forbidden path", got)
	}
}

func TestSessionStartExplainsHowToDeclareAScope(t *testing.T) {
	root := t.TempDir()
	cmd := exec.Command("git", "init", "-q", "-b", "main")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	got := runEvent(t, root, "session-start", `{"cwd":"`+root+`"}`)
	if !strings.Contains(got.AdditionalContext, "vector scope new") {
		t.Errorf("context = %q, want instructions for declaring a scope", got.AdditionalContext)
	}
}

func TestOutsideARepositoryVectorSaysNothing(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := Run("pre-tool", strings.NewReader(`{"cwd":"`+dir+`"}`), &out, dir); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("output = %q, want none outside a repository", out.String())
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, v := range a {
		seen[v]++
	}
	for _, v := range b {
		seen[v]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}
