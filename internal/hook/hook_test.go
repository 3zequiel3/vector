package hook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/3zequiel3/vector/internal/attempt"
	"github.com/3zequiel3/vector/internal/freshness"
	"github.com/3zequiel3/vector/internal/observe"
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

// retryRepo is a repository as `vector init` leaves it: the per-developer
// state under .vector/ is gitignored, so the attempt log is not itself a
// change the audit has to explain. Without that line in setup's localState,
// every audit after the first verify would report the log as a forbidden path.
func retryRepo(t *testing.T) string {
	t.Helper()
	root := newRepo(t, []string{"src/**"})
	if err := os.WriteFile(filepath.Join(root, ".vector", ".gitignore"),
		[]byte("current\nnudged\nattempts\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// commitAll leaves the working tree clean, so an audit has nothing to report
// and the Stop hook's only remaining voice is the retry signal.
func commitAll(t *testing.T, root string) {
	t.Helper()
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", "base"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}

// failVerify records n failed verify runs of the active task, the way verify
// itself would have.
func failVerify(t *testing.T, root string, n int, lines ...int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := attempt.Record(root, attempt.Outcome{
			At:      time.Date(2026, 1, 1, 0, i, 0, 0, time.UTC),
			Task:    "task",
			Verdict: "FAILED",
			Files:   4,
			Lines:   lines[i],
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStopIsSilentBelowTheRetryThreshold(t *testing.T) {
	// Two failed verifies is ordinary work. A Stop hook that speaks up on
	// every turn is one people stop reading.
	root := retryRepo(t)
	commitAll(t, root)
	failVerify(t, root, 2, 120, 300)

	if got := runEvent(t, root, "stop", `{"cwd":"`+root+`"}`); got != (hookOutput{}) {
		t.Errorf("got %+v, want silence after two failures", got)
	}
}

func TestStopReportsARetryLoopAndBlocksNothing(t *testing.T) {
	// The message has to be actionable on its own: how many attempts, how far
	// the diff moved, and what to do next. And it stays context — a suspicion
	// vector cannot confirm must never stop a fourth attempt that would work.
	root := retryRepo(t)
	commitAll(t, root)
	failVerify(t, root, 4, 120, 300, 560, 812)

	got := runEvent(t, root, "stop", `{"cwd":"`+root+`"}`)
	if got.PermissionDecision != "" {
		t.Fatalf("decision = %q, want none; the retry signal decides nothing", got.PermissionDecision)
	}
	for _, want := range []string{"4 failed verifies", "task", "120", "812", "Nothing is blocked"} {
		if !strings.Contains(got.AdditionalContext, want) {
			t.Errorf("context = %q, want it to name %q", got.AdditionalContext, want)
		}
	}
}

func TestStopReportsScopeDriftAndTheRetryLoopTogether(t *testing.T) {
	// A task can be circling inside a boundary it also left. Dropping either
	// finding would hide it behind the other.
	root := retryRepo(t)
	mk := filepath.Join(root, "drift.ts")
	if err := os.WriteFile(mk, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	failVerify(t, root, 3, 120, 300, 812)

	got := runEvent(t, root, "stop", `{"cwd":"`+root+`"}`)
	if !strings.Contains(got.AdditionalContext, "vector audit") {
		t.Errorf("context = %q, want the scope finding kept", got.AdditionalContext)
	}
	if !strings.Contains(got.AdditionalContext, "failed verifies") {
		t.Errorf("context = %q, want the retry finding kept", got.AdditionalContext)
	}
}

func TestStopWithNoHistorySaysNothingNew(t *testing.T) {
	// A repository that has never run verify must behave exactly as it did
	// before any of this existed.
	root := retryRepo(t)
	commitAll(t, root)
	if got := runEvent(t, root, "stop", `{"cwd":"`+root+`"}`); got != (hookOutput{}) {
		t.Errorf("got %+v, want silence with no recorded history", got)
	}
}

func TestStopSurvivesACorruptAttemptLog(t *testing.T) {
	// Unreadable bookkeeping means "no history". Erroring out of the Stop hook
	// over it would cost the user the end of their turn.
	root := retryRepo(t)
	commitAll(t, root)
	if err := os.WriteFile(filepath.Join(root, ".vector", "attempts"),
		[]byte("\x00\x01 not a record\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Run("stop", strings.NewReader(`{"cwd":"`+root+`"}`), &out, root); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("output = %q, want silence", out.String())
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

// verdictRepo is retryRepo with the verdict state gitignored too, which is
// what `vector setup` leaves behind. Without that entry the file verify writes
// about a run would itself show up as an undeclared path in the next audit.
func verdictRepo(t *testing.T) string {
	t.Helper()
	root := newRepo(t, []string{"src/**"})
	if err := os.WriteFile(filepath.Join(root, ".vector", ".gitignore"),
		[]byte("current\nnudged\nattempts\nverdicts\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "Filter.tsx"),
		[]byte("export const Filter = 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A clean tree leaves the audit nothing to say, so anything the Stop hook
	// does report came from the verdict state rather than from drift.
	commitAll(t, root)
	return root
}

// recordVerdict files a verdict over the given files, the way verify would.
func recordVerdict(t *testing.T, root, verdict string, passed bool, files ...string) {
	t.Helper()
	if err := freshness.Record(root, freshness.Snapshot{
		At:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Task:    "task",
		Verdict: verdict,
		Passed:  passed,
		Files:   freshness.Hash(root, files),
	}); err != nil {
		t.Fatal(err)
	}
}

func edit(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStopReportsAStaleVerdictAndBlocksNothing(t *testing.T) {
	// The case this exists for: someone ran verify, heard VERIFIED, kept
	// editing, and the last thing they were told is now a claim about a tree
	// that no longer exists. The message has to name the file, or the reader
	// has been told nothing they can check.
	root := verdictRepo(t)
	recordVerdict(t, root, "VERIFIED", true, "src/Filter.tsx")
	edit(t, root, "src/Filter.tsx", "export const Filter = 2;\n")

	got := runEvent(t, root, "stop", `{"cwd":"`+root+`"}`)
	if got.PermissionDecision != "" {
		t.Fatalf("decision = %q, want none; staleness decides nothing", got.PermissionDecision)
	}
	for _, want := range []string{"stale", "src/Filter.tsx", "task", "UNVERIFIED", "nothing is blocked"} {
		if !strings.Contains(got.AdditionalContext, want) {
			t.Errorf("context = %q, want it to name %q", got.AdditionalContext, want)
		}
	}
}

func TestStopIsSilentWhileTheVerdictStillDescribesTheTree(t *testing.T) {
	// A verdict that is still true must produce nothing. A Stop hook that
	// speaks on every turn is one people stop reading.
	root := verdictRepo(t)
	recordVerdict(t, root, "VERIFIED", true, "src/Filter.tsx")

	if got := runEvent(t, root, "stop", `{"cwd":"`+root+`"}`); got != (hookOutput{}) {
		t.Errorf("got %+v, want silence while the verdict still holds", got)
	}
}

func TestStopSaysNothingAboutAStaleFailingVerdict(t *testing.T) {
	// A stale FAILED misleads nobody. It was already bad news, and the editing
	// that made it stale is exactly the response it was asking for; reporting
	// it would be nagging someone for doing the right thing.
	root := verdictRepo(t)
	recordVerdict(t, root, "FAILED", false, "src/Filter.tsx")
	edit(t, root, "src/Filter.tsx", "export const Filter = 2;\n")

	got := runEvent(t, root, "stop", `{"cwd":"`+root+`"}`)
	if strings.Contains(got.AdditionalContext, "stale") {
		t.Errorf("context = %q, want no staleness for a verdict that already failed", got.AdditionalContext)
	}
}

func TestANewFileInsideTheBoundaryMakesTheVerdictStale(t *testing.T) {
	// A file created after the verdict is part of the work and was never
	// verified, even though nothing the verdict covered was touched.
	root := verdictRepo(t)
	recordVerdict(t, root, "VERIFIED", true, "src/Filter.tsx")
	edit(t, root, "src/Added.tsx", "export const Added = 1;\n")

	got := runEvent(t, root, "stop", `{"cwd":"`+root+`"}`)
	if !strings.Contains(got.AdditionalContext, "src/Added.tsx was added") {
		t.Errorf("context = %q, want the new in-scope file named", got.AdditionalContext)
	}
}

func TestEditingOutsideTheBoundaryIsDriftNotStaleness(t *testing.T) {
	// The verdict never covered that file, so it cannot expire it. Drift is
	// the audit's finding, and reporting the same path twice under two names
	// would make both harder to act on.
	root := verdictRepo(t)
	recordVerdict(t, root, "VERIFIED", true, "src/Filter.tsx")
	edit(t, root, "elsewhere/x.ts", "undeclared\n")

	got := runEvent(t, root, "stop", `{"cwd":"`+root+`"}`)
	if !strings.Contains(got.AdditionalContext, "elsewhere/x.ts") {
		t.Fatalf("context = %q, want the drift still reported", got.AdditionalContext)
	}
	if strings.Contains(got.AdditionalContext, "stale") {
		t.Errorf("context = %q, want no staleness from an out-of-scope edit", got.AdditionalContext)
	}
}

func TestStopReportsDriftStalenessAndTheRetryLoopTogether(t *testing.T) {
	// All three can be true at once, and each hides the others if the hook
	// reports only the first it finds. They are ordered by how firm the claim
	// is: paths, then bytes, then a suspicion that closes by suggesting the
	// reader stop.
	root := verdictRepo(t)
	recordVerdict(t, root, "VERIFIED", true, "src/Filter.tsx")
	edit(t, root, "src/Filter.tsx", "export const Filter = 2;\n")
	edit(t, root, "elsewhere/x.ts", "undeclared\n")
	failVerify(t, root, 3, 120, 300, 812)

	ctx := runEvent(t, root, "stop", `{"cwd":"`+root+`"}`).AdditionalContext
	drift := strings.Index(ctx, "vector audit")
	stale := strings.Index(ctx, "is stale")
	retry := strings.Index(ctx, "failed verifies")
	if drift < 0 || stale < 0 || retry < 0 {
		t.Fatalf("context = %q, want all three findings kept", ctx)
	}
	if !(drift < stale && stale < retry) {
		t.Errorf("context = %q, want drift then staleness then the retry signal", ctx)
	}
}

func TestStopSurvivesCorruptVerdictState(t *testing.T) {
	// Unreadable bookkeeping means "no prior verdict". Erroring out of the Stop
	// hook over a state file would cost the user the end of their turn.
	root := verdictRepo(t)
	if err := os.WriteFile(freshness.Path(root),
		[]byte("\x00\x01 not a snapshot\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	edit(t, root, "src/Filter.tsx", "export const Filter = 2;\n")

	var out bytes.Buffer
	if err := Run("stop", strings.NewReader(`{"cwd":"`+root+`"}`), &out, root); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(out.String(), "stale") {
		t.Errorf("output = %q, want no verdict claim from corrupt state", out.String())
	}
}

func TestStopSaysNothingAboutStalenessWithNoRecordedVerdict(t *testing.T) {
	// A repository that has never run verify must behave exactly as it did
	// before any of this existed, however much has been edited since.
	root := verdictRepo(t)
	edit(t, root, "src/Filter.tsx", "export const Filter = 2;\n")

	got := runEvent(t, root, "stop", `{"cwd":"`+root+`"}`)
	if strings.Contains(got.AdditionalContext, "stale") {
		t.Errorf("context = %q, want no staleness without a recorded verdict", got.AdditionalContext)
	}
}

func TestStopNamesTheHalfNobodyChecked(t *testing.T) {
	// The scope half of a verdict is automatic; the evidence half never is,
	// because running a project's suite at the end of every turn would cost more
	// than the drift it prevents. That trade only holds if the gap is visible —
	// silence about "does it work" reads as an answer, and it is not one.
	root, write := newRepoWithoutScope(t)
	commitAll(t, root) // the policy is baseline; only the task moves the tree now
	write(".vector/scope/task.toml", "objective = \"a thing\"\nwrite = [\"src/**\"]\n")
	write(".vector/current", "task\n")
	write("src/a.go", "package src\n")

	got := runEvent(t, root, "stop", `{"cwd":"`+root+`"}`)
	if !strings.Contains(got.AdditionalContext, "vector verify") {
		t.Errorf("context = %q, want it to name the unrun half", got.AdditionalContext)
	}
	if !strings.Contains(got.AdditionalContext, "UNVERIFIED") {
		t.Errorf("context = %q, want the vocabulary's word for unknown", got.AdditionalContext)
	}
	// It must never look like a failure: nothing is blocked and no decision is
	// returned.
	if got.PermissionDecision != "" {
		t.Errorf("decision = %q, want none", got.PermissionDecision)
	}
}

func TestStopSaysNothingWhenThereIsNoBoundaryToVerifyAgainst(t *testing.T) {
	// NO_SCOPE_DECLARED is already its own admission. Adding "and nobody checked
	// whether it works" on top would be two lines saying one thing.
	root, write := newRepoWithoutScope(t)
	commitAll(t, root)
	write("src/a.go", "package src\n")

	got := runEvent(t, root, "stop", `{"cwd":"`+root+`"}`)
	if strings.Contains(got.AdditionalContext, "vector verify") {
		t.Errorf("context = %q, want silence with no scope declared", got.AdditionalContext)
	}
}

func TestSessionStartRecoversTheTaskState(t *testing.T) {
	// What a session loses at every boundary — a compaction, a handoff, a
	// weekend — is the same three things: what was meant, what happened, and
	// what is still unknown. All three were already on disk and nothing read
	// them back.
	root, mk := newRepoWithoutScope(t)
	mk(".vector/scope/task.toml", `objective = "add a date filter"
write = ["src/**"]

[[expansion]]
at = "2026-09-01T10:00:00Z"
reason = "blocking"
evidence = "typecheck fails: src/api/types.ts does not export DateFilter"
write = ["src/api/types.ts"]
`)
	mk(".vector/current", "task\n")
	if err := freshness.Record(root, freshness.Snapshot{
		At: time.Now().Add(-50 * time.Hour), Task: "task",
		Verdict: "PARTIALLY_VERIFIED", Passed: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := observe.Record(root, observe.Observation{
		Task: "task", Category: "architecture", Severity: "medium",
		Note: "auth middleware mixes session and token",
	}); err != nil {
		t.Fatal(err)
	}

	got := runEvent(t, root, "session-start", `{"cwd":"`+root+`"}`).AdditionalContext
	for _, want := range []string{
		"task",                  // which task is live
		"add a date filter",     // what was meant
		"Boundary widened once", // how it grew
		"PARTIALLY_VERIFIED",    // what happened
		"2 day(s) ago",          // and when, so a stale answer reads as stale
		"1 observation",         // what was noticed and left alone
	} {
		if !strings.Contains(got, want) {
			t.Errorf("context = %q\n  missing %q", got, want)
		}
	}
}

func TestSessionStartSaysOnlyWhatItHas(t *testing.T) {
	// A task with no verdict yet, no expansion and no observation must not be
	// described with empty scaffolding. This runs on every session, and a
	// preamble that costs tokens to say nothing is how a control layer becomes
	// more expensive than the waste it prevents.
	root := newRepo(t, []string{"src/**"})

	got := runEvent(t, root, "session-start", `{"cwd":"`+root+`"}`).AdditionalContext
	for _, unwanted := range []string{"Last verdict", "Boundary widened", "observation"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("context = %q\n  should not mention %q with nothing to report", got, unwanted)
		}
	}
	if !strings.Contains(got, "add a filter") {
		t.Errorf("context = %q, want the objective it does have", got)
	}
}

func TestSessionStartCountsOnlyThisTasksObservations(t *testing.T) {
	// Observations are repo-wide and append-only; a repository that has been
	// running a while accumulates them. Reporting the pile every session would
	// be noise about other work, not recovery of this task.
	root := newRepo(t, []string{"src/**"})
	for _, task := range []string{"task", "somebody-elses", ""} {
		if _, err := observe.Record(root, observe.Observation{
			Task: task, Note: "note for " + task,
		}); err != nil {
			t.Fatal(err)
		}
	}

	got := runEvent(t, root, "session-start", `{"cwd":"`+root+`"}`).AdditionalContext
	if !strings.Contains(got, "1 observation(s) recorded during this task") {
		t.Errorf("context = %q, want only this task's observation counted", got)
	}
}

func TestAgoIsCoarse(t *testing.T) {
	// The reader needs to know whether the last answer is from this afternoon
	// or from before the weekend. A timestamp would be more precise and less
	// useful.
	now := time.Now()
	for _, tt := range []struct {
		at   time.Time
		want string
	}{
		{now.Add(-10 * time.Second), "just now"},
		{now.Add(-30 * time.Minute), "30 minute(s) ago"},
		{now.Add(-5 * time.Hour), "5 hour(s) ago"},
		{now.Add(-72 * time.Hour), "3 day(s) ago"},
	} {
		if got := ago(tt.at); got != tt.want {
			t.Errorf("ago(%v) = %q, want %q", tt.at, got, tt.want)
		}
	}
}

func TestSessionStartWillNotHandOverAStaleVerdict(t *testing.T) {
	// The worst place to over-claim. A session that has just started has no
	// other memory: told "VERIFIED, three hours ago" it will reasonably treat
	// the code as proven and not check again. If the tree moved since — the
	// previous session kept editing, or another agent did — that sentence is
	// the false confidence this tool exists to refuse.
	root, mk := newRepoWithoutScope(t)
	mk(".vector/scope/task.toml", "objective = \"o\"\nwrite = [\"src/**\"]\n")
	mk(".vector/current", "task\n")
	mk("src/filter.ts", "export const a = 1\n")

	files := freshness.Hash(root, []string{"src/filter.ts"})
	if err := freshness.Record(root, freshness.Snapshot{
		At: time.Now().Add(-3 * time.Hour), Task: "task",
		Verdict: "VERIFIED", Passed: true, Files: files,
	}); err != nil {
		t.Fatal(err)
	}

	// Unchanged: the verdict still describes the tree and is reported plainly.
	got := runEvent(t, root, "session-start", `{"cwd":"`+root+`"}`).AdditionalContext
	if !strings.Contains(got, "VERIFIED, 3 hour(s) ago.") {
		t.Errorf("context = %q, want the verdict reported as current", got)
	}
	if strings.Contains(got, "UNVERIFIED") {
		t.Errorf("context = %q, called a current verdict stale", got)
	}

	// Edited since: the same verdict is now a claim about a tree that is gone.
	mk("src/filter.ts", "export const a = 2\n")
	got = runEvent(t, root, "session-start", `{"cwd":"`+root+`"}`).AdditionalContext
	if !strings.Contains(got, "UNVERIFIED") {
		t.Errorf("context = %q, want the stale verdict withdrawn", got)
	}
	if !strings.Contains(got, "changed since") {
		t.Errorf("context = %q, want it to say why", got)
	}
}

func TestSessionStartWithdrawsAVerdictWhoseFileIsGone(t *testing.T) {
	// Deleting what was verified is as much a change as editing it, and it is
	// the one a hash comparison could quietly skip.
	root, mk := newRepoWithoutScope(t)
	mk(".vector/scope/task.toml", "objective = \"o\"\nwrite = [\"src/**\"]\n")
	mk(".vector/current", "task\n")
	mk("src/filter.ts", "export const a = 1\n")

	files := freshness.Hash(root, []string{"src/filter.ts"})
	if err := freshness.Record(root, freshness.Snapshot{
		At: time.Now(), Task: "task", Verdict: "VERIFIED", Passed: true, Files: files,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "src", "filter.ts")); err != nil {
		t.Fatal(err)
	}

	got := runEvent(t, root, "session-start", `{"cwd":"`+root+`"}`).AdditionalContext
	if !strings.Contains(got, "UNVERIFIED") {
		t.Errorf("context = %q, want the verdict withdrawn after its file was deleted", got)
	}
}

func TestAgoRefusesToCallAFutureTimestampRecent(t *testing.T) {
	// A verdict dated ahead of now is not recent, it is wrong — a skewed
	// clock, or a snapshot from another machine. Answering the least
	// trustworthy input with "just now" is the worst available lie.
	got := ago(time.Now().Add(2 * time.Hour))
	if strings.Contains(got, "just now") {
		t.Errorf("ago(future) = %q, want it not to read as recent", got)
	}
	if !strings.Contains(got, "future") {
		t.Errorf("ago(future) = %q, want it to name the problem", got)
	}
	// Small negatives are ordinary clock jitter between two processes, not a
	// broken timestamp, and must not produce an alarming sentence.
	if got := ago(time.Now().Add(2 * time.Second)); got != "just now" {
		t.Errorf("ago(+2s) = %q, want ordinary jitter to stay quiet", got)
	}
}

func TestAnUnreadableConfigFallsBackToTheDefaultsInsteadOfGoingSilent(t *testing.T) {
	// The cheapest possible way to defeat the whole tool, before this: one
	// unparseable byte anywhere in policy.toml or a scope file and the hook
	// returned nothing at all — no forbidden-path denial, no strict-mode
	// denial, repository-wide, until a human happened to notice. The rules
	// that exist so an agent cannot weaken its own constraints were the first
	// thing to go.
	root, mk := newRepoWithoutScope(t)
	mk(".vector/policy.toml", "this is not = = = toml\n")

	for _, path := range []string{".env", ".claude/settings.json", ".vector/policy.toml"} {
		got := runEvent(t, root, "pre-tool", writePayload(root, "s1", path))
		if got.PermissionDecision != "deny" {
			t.Errorf("%s: decision = %q, want deny from the built-in defaults", path, got.PermissionDecision)
		}
	}
}

func TestAnUnreadableScopeStillEnforcesTheForbiddenPaths(t *testing.T) {
	// Same rule one level down: a corrupt scope file means "no task boundary",
	// which BuildRuleset already handles, not "no rules at all".
	root, mk := newRepoWithoutScope(t)
	mk(".vector/scope/task.toml", "objective = \"unterminated\n")
	mk(".vector/current", "task\n")

	got := runEvent(t, root, "pre-tool", writePayload(root, "s1", ".env"))
	if got.PermissionDecision != "deny" {
		t.Errorf("decision = %q, want deny", got.PermissionDecision)
	}
}

func TestABrokenConfigIsSaidOutLoud(t *testing.T) {
	// Falling back quietly would be its own false confidence: the declared
	// boundary is not in force, and a session told nothing reads the absence
	// of a complaint as approval.
	root, mk := newRepoWithoutScope(t)
	mk(".vector/policy.toml", "not = = = toml\n")
	mk("src/ok.go", "package ok\n")

	got := runEvent(t, root, "pre-tool", writePayload(root, "s1", "src/ok.go"))
	if !strings.Contains(got.AdditionalContext, "could not be parsed") {
		t.Errorf("context = %q, want the broken config named", got.AdditionalContext)
	}
	if !strings.Contains(got.AdditionalContext, "vector doctor") {
		t.Errorf("context = %q, want it to say how to find out what is wrong", got.AdditionalContext)
	}
	if got.PermissionDecision != "" {
		t.Errorf("decision = %q, want no denial — the write itself is ordinary", got.PermissionDecision)
	}
}

func TestAHookMessageDoesNotGrowWithTheToolInput(t *testing.T) {
	// A hook's reply is injected straight into the agent's context window, and
	// the path list came from the tool input. A shell command with twenty
	// thousand redirections produced a 269 KB reply — about sixty thousand
	// tokens of noise from one tool call. The finding is the same whether it
	// names five paths or five thousand.
	root := newRepo(t, []string{"src/**"})
	var cmd strings.Builder
	for i := 0; i < 3000; i++ {
		fmt.Fprintf(&cmd, "echo x > out%d.txt; ", i)
	}
	payload := `{"cwd":"` + root + `","session_id":"s1","tool_name":"Bash",` +
		`"tool_input":{"command":"` + cmd.String() + `"}}`

	got := runEvent(t, root, "pre-tool", payload)
	if len(got.AdditionalContext) > 2000 {
		t.Errorf("reply is %d bytes for a %d byte command; it must not scale with the input",
			len(got.AdditionalContext), cmd.Len())
	}
	if !strings.Contains(got.AdditionalContext, "more") {
		t.Errorf("context = %q, want the remainder counted rather than dropped", got.AdditionalContext)
	}
}

func TestEveryOperandOfADestructiveCommandIsAWriteTarget(t *testing.T) {
	// This returned only the first operand until it was measured, which made
	// `rm -rf decoy.txt .vector/policy.toml` report decoy.txt and nothing
	// else — a one-line way past every forbidden-path rule, in the function
	// whose own table says "every non-flag argument is written".
	tests := []struct {
		command string
		want    []string
	}{
		{"rm -rf decoy.txt .vector/policy.toml", []string{"decoy.txt", ".vector/policy.toml"}},
		{"touch src/ok.go .env", []string{"src/ok.go", ".env"}},
		{"chmod 777 a.go b.go c.go", []string{"777", "a.go", "b.go", "c.go"}},
		{"mkdir -p one two", []string{"one", "two"}},
		// mv and cp write only their destination: a multi-source copy ends in
		// a directory, and a directory covers everything it receives.
		{"mv a.go b.go dest/", []string{"dest/"}},
		{"cp a.go b.go dest/", []string{"dest/"}},
	}
	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			got := WriteTargets(tt.command)
			if len(got) != len(tt.want) {
				t.Fatalf("WriteTargets(%q) = %v, want %v", tt.command, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("WriteTargets(%q)[%d] = %q, want %q", tt.command, i, got[i], tt.want[i])
				}
			}
		})
	}
}
