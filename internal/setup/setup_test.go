package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
