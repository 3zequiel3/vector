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

// newRepoWithoutScope builds a repository where vector is installed and
// nothing has been declared. This is the state the whole feature exists for:
// before scope-on-first-write, an agent that ignored the session-start
// suggestion landed here and enforcement had nothing left to enforce.
//
// It returns the root and the writer that made it, so a caller can add a scope
// or overwrite the policy.
func newRepoWithoutScope(t *testing.T) (string, func(rel, content string)) {
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
	// `vector setup` leaves this behind, and the nudge state has to reach it.
	mk(".vector/.gitignore", "current\n")
	return root, mk
}

// newRepo builds a repository with a policy and an active scope.
func newRepo(t *testing.T, write []string) string {
	t.Helper()
	root, mk := newRepoWithoutScope(t)
	mk(".vector/scope/task.toml",
		"objective = \"add a filter\"\nwrite = [\""+strings.Join(write, "\", \"")+"\"]\n")
	mk(".vector/current", "task\n")
	return root
}

// writePayload is one agent write, as the hook receives it.
func writePayload(root, session, path string) string {
	return `{"cwd":"` + root + `","session_id":"` + session +
		`","tool_name":"Write","tool_input":{"file_path":"` + path + `"}}`
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

func TestFirstWriteWithoutAScopeIsDenied(t *testing.T) {
	// The message has to be actionable without guessing: the literal command,
	// the paths at stake, and the instruction to cover the whole task rather
	// than only the file in hand — otherwise the agent declares a scope of one
	// file and spends the rest of the turn expanding it.
	root, _ := newRepoWithoutScope(t)
	got := runEvent(t, root, "pre-tool", writePayload(root, "", "app/x.ts"))
	if got.PermissionDecision != "deny" {
		t.Fatalf("decision = %q, want deny for the first write with no scope", got.PermissionDecision)
	}
	for _, want := range []string{"vector scope new", "-o ", "-w ", "app/x.ts", "not just this one"} {
		if !strings.Contains(got.PermissionDecisionReason, want) {
			t.Errorf("reason = %q, want it to contain %q", got.PermissionDecisionReason, want)
		}
	}
	// Committing the state would tell a teammate's fresh session that it had
	// already been asked.
	// The hook must not edit a user-owned file. It runs once per tool call, and
	// a hot path that quietly rewrites configuration is a surprise nobody asked
	// for; keeping .gitignore current is init's job.
	ignore, err := os.ReadFile(filepath.Join(root, ".vector", ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if string(ignore) != "current\n" {
		t.Errorf(".vector/.gitignore = %q, want it untouched by the hook", ignore)
	}
}

func TestTheAskIsDeliveredOnlyOnce(t *testing.T) {
	// An agent that does not act on the message must still be able to work. A
	// denial vector cannot stop repeating is a whole turn spent against a
	// wall, and `vector audit` still reports NO_SCOPE_DECLARED at the end.
	root, _ := newRepoWithoutScope(t)
	if got := runEvent(t, root, "pre-tool", writePayload(root, "", "app/x.ts")); got.PermissionDecision != "deny" {
		t.Fatalf("first write: decision = %q, want deny", got.PermissionDecision)
	}
	for i := 2; i <= 3; i++ {
		if got := runEvent(t, root, "pre-tool", writePayload(root, "", "app/y.ts")); got != (hookOutput{}) {
			t.Errorf("write %d: got %+v, want silence once the ask was delivered", i, got)
		}
	}
}

func TestStrictModeKeepsAskingUntilAScopeExists(t *testing.T) {
	// Strict means the boundary is mandatory, so there is no once-only mercy:
	// undeclared work genuinely cannot proceed.
	root, mk := newRepoWithoutScope(t)
	mk(".vector/policy.toml",
		"[scope]\nalways_forbidden = [\".vector/**\"]\n\n[mode]\nenforcement = \"strict\"\n")
	for i := 1; i <= 3; i++ {
		got := runEvent(t, root, "pre-tool", writePayload(root, "s1", "app/x.ts"))
		if got.PermissionDecision != "deny" {
			t.Fatalf("write %d: decision = %q, want deny in strict mode", i, got.PermissionDecision)
		}
		if !strings.Contains(got.PermissionDecisionReason, "vector scope new") {
			t.Errorf("write %d: reason = %q, want the command to run", i, got.PermissionDecisionReason)
		}
	}
}

func TestADeclaredScopeIsNeverAsked(t *testing.T) {
	// The ask is about the missing boundary, not about the write. A repository
	// that declared one must behave exactly as it did before this existed.
	root := newRepo(t, []string{"src/**"})
	if got := runEvent(t, root, "pre-tool", writePayload(root, "s1", "src/ok.ts")); got != (hookOutput{}) {
		t.Errorf("got %+v, want no output for an in-scope write", got)
	}
	if _, err := os.Stat(filepath.Join(root, ".vector", "nudged")); !os.IsNotExist(err) {
		t.Errorf("nudge state exists (%v), want none when a scope is declared", err)
	}
}

func TestForbiddenDeniesBeforeTheAskAndDoesNotSpendIt(t *testing.T) {
	// Forbidden outranks the missing boundary: it is denied whether or not a
	// scope exists. That denial must also not consume the one ask, or an agent
	// whose first write happens to be .env never learns to declare anything.
	root, _ := newRepoWithoutScope(t)
	got := runEvent(t, root, "pre-tool", writePayload(root, "s1", ".env"))
	if got.PermissionDecision != "deny" {
		t.Fatalf("decision = %q, want deny for a forbidden path", got.PermissionDecision)
	}
	if !strings.Contains(got.PermissionDecisionReason, "forbidden") {
		t.Errorf("reason = %q, want the forbidden rule named", got.PermissionDecisionReason)
	}
	next := runEvent(t, root, "pre-tool", writePayload(root, "s1", "app/x.ts"))
	if !strings.Contains(next.PermissionDecisionReason, "vector scope new") {
		t.Errorf("reason = %q, want the ask still available", next.PermissionDecisionReason)
	}
}

func TestEachSessionGetsItsOwnAsk(t *testing.T) {
	// Two agents in one repository are two pieces of work. Sharing the record
	// would silently exempt whichever one started second.
	root, _ := newRepoWithoutScope(t)
	for _, session := range []string{"session-a", "session-b"} {
		got := runEvent(t, root, "pre-tool", writePayload(root, session, "app/x.ts"))
		if got.PermissionDecision != "deny" {
			t.Errorf("session %q: decision = %q, want deny", session, got.PermissionDecision)
		}
	}
	if got := runEvent(t, root, "pre-tool", writePayload(root, "session-a", "app/y.ts")); got != (hookOutput{}) {
		t.Errorf("session-a second write: got %+v, want silence", got)
	}
}

func TestCorruptNudgeStateMeansNotAskedYet(t *testing.T) {
	// Bookkeeping vector cannot make sense of is bookkeeping it does not
	// trust. One extra ask costs an explained denial; trusting nonsense would
	// silently disable the feature instead.
	root, _ := newRepoWithoutScope(t)
	state := filepath.Join(root, ".vector", "nudged")
	if err := os.WriteFile(state, []byte("\x00\x01 not a session id"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := runEvent(t, root, "pre-tool", writePayload(root, "s1", "app/x.ts")); got.PermissionDecision != "deny" {
		t.Errorf("decision = %q, want deny when the state cannot be trusted", got.PermissionDecision)
	}
}

func TestUnwritableNudgeStateStaysSilentRatherThanLooping(t *testing.T) {
	// If the record cannot be written, the same denial would come back on
	// every write forever. vector would rather deliver no ask at all than trap
	// the turn — and it must not fail the hook over bookkeeping either.
	root, _ := newRepoWithoutScope(t)
	// A directory where the state file belongs is unreadable and unwritable
	// without depending on file modes that a root-run test would ignore.
	if err := os.Mkdir(filepath.Join(root, ".vector", "nudged"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		var out bytes.Buffer
		if err := Run("pre-tool", strings.NewReader(writePayload(root, "s1", "app/x.ts")), &out, root); err != nil {
			t.Fatalf("write %d: Run: %v", i, err)
		}
		if out.Len() != 0 {
			t.Errorf("write %d: output = %q, want silence", i, out.String())
		}
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
