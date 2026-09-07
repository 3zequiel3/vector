package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func readSettings(t *testing.T, root string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestInstallRegistersEveryEvent(t *testing.T) {
	root := newRepo(t)
	res, err := InstallClaudeHooks(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Added) != len(wanted) {
		t.Fatalf("added = %v, want all %d events", res.Added, len(wanted))
	}
	hooks, _ := readSettings(t, root)["hooks"].(map[string]any)
	for _, w := range wanted {
		if _, ok := hooks[w.event]; !ok {
			t.Errorf("event %q missing", w.event)
		}
	}
}

func TestInstallIsIdempotent(t *testing.T) {
	// Re-running init must not stack a second copy of every hook.
	root := newRepo(t)
	if _, err := InstallClaudeHooks(root); err != nil {
		t.Fatal(err)
	}
	res, err := InstallClaudeHooks(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Added) != 0 {
		t.Errorf("added = %v on a second run, want none", res.Added)
	}
	hooks, _ := readSettings(t, root)["hooks"].(map[string]any)
	list, _ := hooks["PreToolUse"].([]any)
	if len(list) != 1 {
		t.Errorf("PreToolUse has %d entries, want 1", len(list))
	}
}

func TestInstallPreservesEverythingElse(t *testing.T) {
	// Someone's permissions, their own hooks and their unrelated settings must
	// survive. A tool that eats configuration is a tool people remove.
	root := newRepo(t)
	existing := `{
      "permissions": {"deny": ["Read(./secrets/**)"]},
      "outputStyle": "Gentleman",
      "hooks": {
        "PreToolUse": [{"matcher":"Bash","hooks":[{"type":"command","command":"my-own-guard"}]}],
        "SessionEnd": [{"hooks":[{"type":"command","command":"cleanup"}]}]
      }
    }`
	if err := os.MkdirAll(filepath.Join(root, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".claude", "settings.json"),
		[]byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := InstallClaudeHooks(root); err != nil {
		t.Fatal(err)
	}
	got := readSettings(t, root)
	if got["outputStyle"] != "Gentleman" {
		t.Error("outputStyle was lost")
	}
	if _, ok := got["permissions"]; !ok {
		t.Error("permissions were lost")
	}
	hooks, _ := got["hooks"].(map[string]any)
	if _, ok := hooks["SessionEnd"]; !ok {
		t.Error("an unrelated hook event was lost")
	}
	list, _ := hooks["PreToolUse"].([]any)
	if len(list) != 2 {
		t.Fatalf("PreToolUse has %d entries, want the existing one plus vector's", len(list))
	}
	if !hasCommand(list, "my-own-guard") {
		t.Error("the user's own PreToolUse hook was dropped")
	}
}

func TestUninstallRemovesOnlyVector(t *testing.T) {
	root := newRepo(t)
	existing := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"my-own-guard"}]}]}}`
	if err := os.MkdirAll(filepath.Join(root, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".claude", "settings.json"),
		[]byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallClaudeHooks(root); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveClaudeHooks(root); err != nil {
		t.Fatal(err)
	}

	hooks, _ := readSettings(t, root)["hooks"].(map[string]any)
	list, _ := hooks["PreToolUse"].([]any)
	if len(list) != 1 || !hasCommand(list, "my-own-guard") {
		t.Errorf("PreToolUse = %v, want only the user's own hook left", list)
	}
	if _, ok := hooks["Stop"]; ok {
		t.Error("an event vector added on its own was left behind")
	}
}

func TestUninstallOnACleanRepoIsANoOp(t *testing.T) {
	res, err := RemoveClaudeHooks(newRepo(t))
	if err != nil {
		t.Fatalf("removing nothing is not an error: %v", err)
	}
	if len(res.Added) != 0 {
		t.Errorf("reported %v removed, want none", res.Added)
	}
}

func TestInstallRefusesToGuessAtBrokenJSON(t *testing.T) {
	root := newRepo(t)
	if err := os.MkdirAll(filepath.Join(root, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".claude", "settings.json"),
		[]byte("{ this is not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallClaudeHooks(root); err == nil {
		t.Error("a malformed settings file was overwritten instead of reported")
	}
}
