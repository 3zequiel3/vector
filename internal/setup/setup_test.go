package setup

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/3zequiel3/vector/internal/detect"
	"github.com/3zequiel3/vector/internal/scope"
)

func newRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestNewScopeRefusesToClobber(t *testing.T) {
	root := newRepo(t)
	if _, err := NewScope(root, "task", "objetivo", []string{"src/**"}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewScope(root, "task", "otro", []string{"otro/**"}); err == nil {
		t.Error("an existing scope was overwritten")
	}
}

func TestNewScopeRejectsPathTraversalInTheID(t *testing.T) {
	// The id becomes a filename. Letting it carry separators would let a scope
	// be written anywhere in the tree.
	root := newRepo(t)
	for _, id := range []string{"../escape", "a/b", `a\b`, "a.b"} {
		if _, err := NewScope(root, id, "", []string{"src/**"}); err == nil {
			t.Errorf("id %q was accepted", id)
		}
	}
}

func TestExpandRequiresEvidenceWhenThePolicySaysSo(t *testing.T) {
	root := newRepo(t)
	if _, err := NewScope(root, "task", "objetivo", []string{"src/**"}); err != nil {
		t.Fatal(err)
	}

	// A boundary that widens on assertion alone is not a boundary.
	if _, err := ExpandScope(root, "task", "blocking", "", []string{"lib/**"}, true); err == nil {
		t.Error("the scope widened without evidence under a policy that requires it")
	}
	if _, err := ExpandScope(root, "task", "blocking", "", []string{"lib/**"}, false); err != nil {
		t.Errorf("the policy did not require evidence and it still failed: %v", err)
	}
}

func TestExpandRejectsAnInventedReason(t *testing.T) {
	root := newRepo(t)
	if _, err := NewScope(root, "task", "", []string{"src/**"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ExpandScope(root, "task", "me-parecio", "porque sí", []string{"lib/**"}, true); err == nil {
		t.Error("a reason outside the permitted set was accepted")
	}
}

func TestExpandRefusesAnUndeclaredTask(t *testing.T) {
	root := newRepo(t)
	if _, err := ExpandScope(root, "fantasma", "blocking", "evidencia", []string{"lib/**"}, true); err == nil {
		t.Error("a scope that was never declared got widened")
	}
}

func TestExpansionIsRecordedNotMerged(t *testing.T) {
	// The original declaration must stay legible, and the file must show how
	// the boundary grew and on what grounds.
	root := newRepo(t)
	if _, err := NewScope(root, "task", "objetivo", []string{"src/**"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ExpandScope(root, "task", "blocking", "typecheck fails in lib/types.ts",
		[]string{"lib/**"}, true); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(root, ".vector", "scope", "task.toml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, `"src/**"`) {
		t.Error("the original declaration was lost")
	}
	for _, want := range []string{"[[expansion]]", "blocking", "typecheck fails in lib/types.ts", `"lib/**"`} {
		if !strings.Contains(text, want) {
			t.Errorf("the file does not record %q", want)
		}
	}

	sc, err := scope.LoadScope(root, "task")
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.Write) != 1 {
		t.Errorf("Write = %v, want the original declaration untouched", sc.Write)
	}
	if got := sc.EffectiveWrite(); len(got) != 2 {
		t.Errorf("EffectiveWrite = %v, want the original plus the expansion", got)
	}

	// And the widened boundary must actually take effect.
	r := scope.BuildRuleset(scope.DefaultPolicy(), sc)
	if d, _ := r.Decide("lib/types.ts"); d != scope.Allowed {
		t.Errorf("Decide(lib/types.ts) = %v, want Allowed after the expansion", d)
	}
	if r.Expansions != 1 {
		t.Errorf("Expansions = %d, want 1", r.Expansions)
	}
}

func TestInitPreservesTheHumanOwnedSections(t *testing.T) {
	// Regenerating detected values must never discard someone's decisions.
	root := newRepo(t)
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".vector", "policy.toml")
	custom := "\n[scope]\nalways_forbidden = [\"solo/esto/**\"]\nexpansion_requires_evidence = false\n\n[mode]\nenforcement = \"strict\"\n"
	if err := os.WriteFile(path, []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Init(root)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Existed {
		t.Error("the existing policy was not detected")
	}
	if res.Policy.Mode.Enforcement != "strict" {
		t.Errorf("enforcement = %q, want strict preserved", res.Policy.Mode.Enforcement)
	}
	if res.Policy.Scope.ExpansionRequiresEvidence {
		t.Error("expansion_requires_evidence reverted to true; the human decision was lost")
	}
	// The customization survives. The deny list is the one exception to
	// "preserved verbatim": it also grows, because it is the list whose whole
	// purpose is that an agent cannot weaken its own constraints, and a
	// repository frozen at the protections of the day it was set up is not
	// protected by the ones added since. Additions are announced.
	if !contains(res.Policy.Scope.AlwaysForbidden, "solo/esto/**") {
		t.Errorf("always_forbidden = %v, lost the customized entry", res.Policy.Scope.AlwaysForbidden)
	}
	if !contains(res.Policy.Scope.AlwaysForbidden, ".vector/**") {
		t.Errorf("always_forbidden = %v, did not adopt the defaults", res.Policy.Scope.AlwaysForbidden)
	}
}

func TestNewScopeSelectsTheTaskItDeclares(t *testing.T) {
	// Without this, every later command needs -task and the hook enforces
	// nothing, because it looks up the active task and finds none.
	root := newRepo(t)
	if _, err := NewScope(root, "task", "objective", []string{"src/**"}); err != nil {
		t.Fatal(err)
	}
	if got := scope.Current(root); got != "task" {
		t.Errorf("Current = %q, want the scope just declared", got)
	}
}

func TestCurrentIgnoresAPointerToADeletedScope(t *testing.T) {
	// A stale pointer would silently enforce a boundary nobody declared for
	// this work, which is worse than enforcing none.
	root := newRepo(t)
	if _, err := NewScope(root, "task", "objective", []string{"src/**"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, ".vector", "scope", "task.toml")); err != nil {
		t.Fatal(err)
	}
	if got := scope.Current(root); got != "" {
		t.Errorf("Current = %q, want empty when the scope file is gone", got)
	}
}

func TestInitGitignoresLocalState(t *testing.T) {
	// These are per-developer working state, like .git/HEAD. Committing them
	// would make every teammate's checkout fight over whose task is current —
	// and both would show up as unexplained changes in every audit.
	root := newRepo(t)
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".vector", ".gitignore"))
	if err != nil {
		t.Fatalf("no .vector/.gitignore was written: %v", err)
	}
	for _, want := range localState {
		if !strings.Contains(string(data), want) {
			t.Errorf("contents = %q, want %q ignored", string(data), want)
		}
	}
}

func TestInitDoesNotClobberAnExistingVectorGitignore(t *testing.T) {
	root := newRepo(t)
	if err := os.MkdirAll(filepath.Join(root, ".vector"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, ".vector", ".gitignore")
	if err := os.WriteFile(p, []byte("current\nscratch/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	if !strings.Contains(string(data), "scratch/") {
		t.Error("a customized .vector/.gitignore was overwritten")
	}
	// A repository set up before an entry existed must still get it, which is
	// why init merges rather than only writing the file when absent.
	if !strings.Contains(string(data), "nudged") {
		t.Error("a missing entry was not added to an existing .vector/.gitignore")
	}
}

func TestRenderedCommandsAreCommentedOut(t *testing.T) {
	// This is the guard on the trap that makes [commands] readable at all.
	//
	// Policy now overrides detection. If init wrote the detected command as
	// live TOML, that value would become an override the instant it was
	// written, freezing the project at whatever its manifests said the day
	// init ran — with nothing on screen to say so. Detection belongs in this
	// file as a comment: visible, and inert until a human acts on it.
	out := render(
		detect.Stack{},
		detect.Commands{Test: "go test ./...", Lint: "go vet ./..."},
		scope.DefaultPolicy(),
	)

	if !strings.Contains(out, `# test = "go test ./..."  (detected)`) {
		t.Errorf("detected test command not rendered as a labelled comment:\n%s", out)
	}
	if !strings.Contains(out, `# typecheck = ""  (no local evidence)`) {
		t.Errorf("undetected command should stay distinguishable from an empty declaration:\n%s", out)
	}
}

func TestRenderedCommandsDoNotParseAsDeclarations(t *testing.T) {
	// The claim above, proven by the parser rather than by string matching:
	// a freshly rendered policy must override nothing.
	out := render(
		detect.Stack{},
		detect.Commands{Test: "go test ./...", Build: "go build ./...",
			Typecheck: "tsc --noEmit", Lint: "go vet ./..."},
		scope.DefaultPolicy(),
	)

	var p scope.Policy
	if err := toml.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("rendered policy does not parse: %v", err)
	}
	if p.Commands != (detect.Commands{}) {
		t.Errorf("a freshly rendered policy declares command overrides: %+v", p.Commands)
	}
}

func TestInitPreservesACommandOverride(t *testing.T) {
	// The override exists to be durable. `vector init` is documented as safe
	// to re-run — it refreshes what was detected — so an override that did
	// not survive it would be withdrawn by the very command a user is told
	// costs them nothing, and withdrawn silently.
	root := newRepo(t)
	if err := os.WriteFile(filepath.Join(root, "go.mod"),
		[]byte("module example.com/x\n\ngo 1.27\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The lockfile is the identity detection resolves the toolchain from.
	if err := os.WriteFile(filepath.Join(root, "go.sum"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Init(root); err != nil {
		t.Fatalf("first Init: %v", err)
	}

	p := filepath.Join(root, ".vector", "policy.toml")
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	// What a human does: uncomment the line and correct it.
	edited := strings.Replace(string(body),
		`# lint = "go vet ./..."  (detected)`, `lint = "make lint"`, 1)
	if edited == string(body) {
		t.Fatalf("the detected lint line was not where the test expected it:\n%s", body)
	}
	if err := os.WriteFile(p, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Init(root); err != nil {
		t.Fatalf("second Init: %v", err)
	}

	pol, err := scope.LoadPolicy(root)
	if err != nil {
		t.Fatalf("LoadPolicy after re-init: %v", err)
	}
	if pol.Commands.Lint != "make lint" {
		t.Errorf("override did not survive re-init: Commands.Lint = %q", pol.Commands.Lint)
	}
	// And what it displaced stays on the page, so the choice can be revisited.
	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), `(detected, overridden below)`) {
		t.Errorf("the displaced detected command is no longer shown:\n%s", after)
	}
	// A command nobody overrode must still be regenerated as a comment.
	if pol.Commands.Test != "" {
		t.Errorf("re-init declared an override nobody asked for: Test = %q", pol.Commands.Test)
	}
}

// gitRepo is a real repository, because Inventory asks git what exists and a
// stub would test the stub.
func gitRepo(t *testing.T) string {
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

func TestInventoryListsWhatAlreadyLivesInTheBoundary(t *testing.T) {
	// The whole of vector's answer to an agent about to rebuild something the
	// repository already has: at the one moment it has said where it will
	// write and has not written yet, tell it what is there.
	root := gitRepo(t)
	for _, f := range []string{
		"src/import/CustomerValidator.ts",
		"src/import/RecordParser.ts",
		"src/dashboard/Filter.tsx",
	} {
		mkFile(t, root, f, "export const x = 1\n")
	}

	files, total := Inventory(root, []string{"src/import/**"})
	if total != 2 {
		t.Fatalf("total = %d, want 2 — only the declared boundary", total)
	}
	if len(files) != 2 || files[0] != "src/import/CustomerValidator.ts" {
		t.Errorf("files = %v, want the two importer files", files)
	}
}

func TestInventoryLeavesVectorsOwnFootprintOut(t *testing.T) {
	// A wider exclusion than the audit makes, and deliberately so: the audit
	// reports a change to policy.toml because editing the enforcement contract
	// is the event it exists to surface. "There is a policy.toml" tells an
	// agent nothing it can use.
	root := gitRepo(t)
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	mkFile(t, root, "src/a.ts", "export const a = 1\n")

	files, total := Inventory(root, []string{"**"})
	for _, f := range files {
		if strings.HasPrefix(f, ".vector/") {
			t.Errorf("files = %v, want vector's own files left out", files)
		}
	}
	if total != 1 {
		t.Errorf("total = %d, want only the project's own file", total)
	}
}

func TestInventoryCapsALongList(t *testing.T) {
	// A list nobody reads puts the neighbourhood in front of the agent no
	// better than no list. Past the cap the honest answer is the count, and
	// the caller says the boundary is probably broader than the task.
	root := gitRepo(t)
	for i := 0; i < maxInventory+10; i++ {
		mkFile(t, root, fmt.Sprintf("src/f%02d.ts", i), "export const x = 1\n")
	}

	files, total := Inventory(root, []string{"src/**"})
	if total != maxInventory+10 {
		t.Errorf("total = %d, want every file counted", total)
	}
	if len(files) != maxInventory {
		t.Errorf("len(files) = %d, want it capped at %d", len(files), maxInventory)
	}
}

func TestInventoryOnEmptyGround(t *testing.T) {
	// A boundary over ground that does not exist yet is the ordinary case for
	// a new feature, and must not be reported as a failure.
	root := gitRepo(t)
	mkFile(t, root, "src/a.ts", "export const a = 1\n")

	files, total := Inventory(root, []string{"src/reports/**"})
	if total != 0 || len(files) != 0 {
		t.Errorf("Inventory = %v (%d), want nothing on empty ground", files, total)
	}
}

func TestInventoryOutsideARepositorySaysNothing(t *testing.T) {
	// git is the only source of truth here. Without it there is no answer,
	// and inventing one would be worse than staying quiet.
	files, total := Inventory(t.TempDir(), []string{"**"})
	if files != nil || total != 0 {
		t.Errorf("Inventory = %v (%d), want silence outside a repository", files, total)
	}
}

func mkFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAgentSuppliedTextCannotProduceAnUnparseableScope(t *testing.T) {
	// Go's %q is not a TOML encoder: it escapes BEL as \a and vertical tab as
	// \v, neither of which TOML defines. One control character in an objective
	// produced a scope file nothing could read, which everything downstream
	// treated as "there is no boundary".
	root := gitRepo(t)
	nasty := "add a filter\x07 and \x0b more\x00 \"quoted\" back\\slash\ttab"

	if _, err := NewScope(root, "task", nasty, []string{"src/**", "x\x07y/**"}); err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	if _, err := ExpandScope(root, "task", "blocking", "evidence\x07with a bell", []string{"lib/**"}, true); err != nil {
		t.Fatalf("ExpandScope: %v", err)
	}

	sc, err := scope.LoadScope(root, "task")
	if err != nil {
		t.Fatalf("the scope file does not parse: %v", err)
	}
	if sc == nil {
		t.Fatal("LoadScope returned nothing for a scope that exists")
	}
	if strings.ContainsAny(sc.Objective, "\x07\x0b\x00") {
		t.Errorf("objective = %q, want control characters dropped", sc.Objective)
	}
	if !strings.Contains(sc.Objective, `"quoted"`) || !strings.Contains(sc.Objective, `back\slash`) {
		t.Errorf("objective = %q, want ordinary punctuation preserved", sc.Objective)
	}
	if len(sc.Expansions) != 1 {
		t.Errorf("Expansions = %v, want the appended block to have parsed", sc.Expansions)
	}
}

func TestInitAdoptsForbiddenPathsAPolicyPredates(t *testing.T) {
	// [scope] is preserved verbatim across regeneration, which is right — and
	// also meant frozen. A repository initialised before a protection existed
	// never received it: every deny path added after the day someone ran
	// `vector init` reached new repositories only. The gap is invisible, since
	// the policy parses and doctor is happy; the path is simply not denied.
	//
	// Found by registering the hooks and watching a write to .gitignore come
	// back as merely out of scope, hours after .gitignore was added to the
	// defaults.
	root := gitRepo(t)
	old := "[scope]\nalways_forbidden = [\".vector/**\", \".env\"]\n\n[mode]\nenforcement = \"advisory\"\n"
	mkFile(t, root, ".vector/policy.toml", old)

	res, err := Init(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.NewForbidden) == 0 {
		t.Fatal("nothing was adopted; a policy from before today kept the shorter list")
	}
	// Named, because a protection that arrives silently is one nobody knows
	// they have and one somebody may have removed on purpose.
	if !contains(res.NewForbidden, ".gitignore") {
		t.Errorf("NewForbidden = %v, want the git-visibility paths among them", res.NewForbidden)
	}

	pol, err := scope.LoadPolicy(root)
	if err != nil {
		t.Fatal(err)
	}
	r := scope.BuildRuleset(pol, nil)
	for _, p := range []string{".gitignore", ".mcp.json", ".git/hooks/pre-commit", ".claude/agents/x.md"} {
		if d, _ := r.Decide(p); d != scope.Forbidden {
			t.Errorf("Decide(%q) = %v, want Forbidden after adoption", p, d)
		}
	}
	// What was already there survives; adoption adds, it never rewrites.
	if d, _ := r.Decide(".env"); d != scope.Forbidden {
		t.Error(".env stopped being denied")
	}
}

func TestInitAdoptsNothingWhenThePolicyIsCurrent(t *testing.T) {
	// The second run must be quiet, or every init would report a migration
	// that already happened.
	root := gitRepo(t)
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	res, err := Init(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.NewForbidden) != 0 {
		t.Errorf("NewForbidden = %v, want nothing to adopt on a current policy", res.NewForbidden)
	}
}

func TestInitLeavesHighRiskAlone(t *testing.T) {
	// high_risk documents `= []` as a real opt-out for a project whose layout
	// makes the defaults noisy. Adopting into it the way always_forbidden does
	// would take that away.
	root := gitRepo(t)
	mkFile(t, root, ".vector/policy.toml", "[scope]\nhigh_risk = []\n")

	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	pol, err := scope.LoadPolicy(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(pol.Scope.HighRisk) != 0 {
		t.Errorf("high_risk = %v, want the opt-out honoured", pol.Scope.HighRisk)
	}
}

func TestHookOnlySurvivesAPolicyRegeneration(t *testing.T) {
	// [scope] is the project's, and this key decides how much of it the kernel
	// enforces. A regeneration that reset it would quietly hand paths back to
	// the sandbox — or take them away — on a run someone started to refresh
	// the detected stack.
	root := gitRepo(t)
	mkFile(t, root, ".vector/policy.toml",
		"[scope]\nalways_forbidden = [\".vector/**\", \".gitignore\", \".env\"]\n"+
			"hook_only = [\".env\"]\n")

	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	pol, err := scope.LoadPolicy(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(pol.Scope.HookOnly) != 1 || pol.Scope.HookOnly[0] != ".env" {
		t.Fatalf("hook_only = %v, want the project's own choice kept", pol.Scope.HookOnly)
	}
	// And the rendered file says so, so the next run reads back what this one
	// decided rather than the defaults.
	raw, err := os.ReadFile(filepath.Join(root, ".vector", "policy.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "hook_only = [\n  \".env\",\n]") {
		t.Errorf("policy.toml did not render hook_only:\n%s", raw)
	}
	// The default git paths are not re-added: growing this list withdraws a
	// path from the sandbox, and that is not a change init gets to make.
	if contains(pol.Scope.HookOnly, ".gitignore") {
		t.Errorf("hook_only = %v, want init not to widen it", pol.Scope.HookOnly)
	}
}

func TestInitLeavesHookOnlyEmptyWhenTheProjectEmptiedIt(t *testing.T) {
	// `hook_only = []` asks for every protection to reach the OS sandbox too.
	// It is the strict direction, and an opt-out init must honour rather than
	// read as an absent key.
	root := gitRepo(t)
	mkFile(t, root, ".vector/policy.toml", "[scope]\nhook_only = []\n")

	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	pol, err := scope.LoadPolicy(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(pol.Scope.HookOnly) != 0 {
		t.Errorf("hook_only = %v, want the opt-out honoured", pol.Scope.HookOnly)
	}
	if len(SandboxForbidden(pol.Scope.AlwaysForbidden, pol.Scope.HookOnly)) != len(pol.Scope.AlwaysForbidden) {
		t.Error("something was still withheld from the sandbox with hook_only emptied")
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
