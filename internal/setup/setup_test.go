package setup

import (
	"os"
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
	if len(res.Policy.Scope.AlwaysForbidden) != 1 ||
		res.Policy.Scope.AlwaysForbidden[0] != "solo/esto/**" {
		t.Errorf("always_forbidden = %v, want the customized list", res.Policy.Scope.AlwaysForbidden)
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
