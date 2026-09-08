package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func forbidden() []string {
	return []string{".vector/**", ".claude/settings.json", ".env", ".env.*"}
}

func denyWrite(t *testing.T, root string) []string {
	t.Helper()
	s := readSettings(t, root)
	sandbox, _ := s["sandbox"].(map[string]any)
	fs, _ := sandbox["filesystem"].(map[string]any)
	raw, _ := fs["denyWrite"].([]any)
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		if v, ok := e.(string); ok {
			out = append(out, v)
		}
	}
	return out
}

func TestTranslateAnchorsAtTheProjectRoot(t *testing.T) {
	// In a project's .claude/settings.json a bare or "./" path resolves against
	// the project root, which is what vector's patterns are written against.
	// Getting this wrong writes a config that silently protects nothing.
	cases := map[string]string{
		".vector/**":            "./.vector/**",
		"./.env":                "./.env",
		"src/generated/**":      "./src/generated/**",
		"/etc/hosts":            "/etc/hosts",
		"~/.ssh/id_rsa":         "~/.ssh/id_rsa",
		"  .claude/hooks/**   ": "./.claude/hooks/**",
	}
	for in, want := range cases {
		if got := sandboxTranslate(in); got != want {
			t.Errorf("sandboxTranslate(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSandboxForbiddenLeavesGitMetadataReadable(t *testing.T) {
	got := SandboxForbidden([]string{
		".vector/**", ".gitignore", "**/.gitignore", ".git/info/exclude",
		".gitattributes", ".env", ".claude/settings.json",
	})
	want := []string{".vector/**", ".env", ".claude/settings.json"}
	if len(got) != len(want) {
		t.Fatalf("SandboxForbidden() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("SandboxForbidden()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSandboxTurnsOnAndDeniesTheForbiddenPaths(t *testing.T) {
	root := newRepo(t)
	if _, err := InstallClaudeSandbox(root, forbidden()); err != nil {
		t.Fatal(err)
	}
	s := readSettings(t, root)
	sandbox, _ := s["sandbox"].(map[string]any)
	if enabled, _ := sandbox["enabled"].(bool); !enabled {
		t.Error("sandbox.enabled was not turned on")
	}
	if got := len(denyWrite(t, root)); got != len(forbidden()) {
		t.Errorf("denyWrite has %d entries, want %d", got, len(forbidden()))
	}
}

func TestSandboxIsIdempotent(t *testing.T) {
	root := newRepo(t)
	if _, err := InstallClaudeSandbox(root, forbidden()); err != nil {
		t.Fatal(err)
	}
	res, err := InstallClaudeSandbox(root, forbidden())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Added) != 0 {
		t.Errorf("added = %v on a second run, want none", res.Added)
	}
	if got := len(denyWrite(t, root)); got != len(forbidden()) {
		t.Errorf("denyWrite has %d entries after two runs, want %d", got, len(forbidden()))
	}
}

func TestSandboxPreservesEverythingElse(t *testing.T) {
	// Filesystem arrays merge across settings scopes, so vector's contribution
	// can only add. It must not quietly drop a network policy or a permission
	// rule on the way in.
	root := newRepo(t)
	existing := `{
      "outputStyle": "Gentleman",
      "permissions": {"deny": ["Read(./private/**)"]},
      "sandbox": {
        "network": {"allowedDomains": ["github.com"]},
        "filesystem": {"denyWrite": ["./mine/**"], "allowWrite": ["/tmp/build"]}
      }
    }`
	if err := os.MkdirAll(filepath.Join(root, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".claude", "settings.json"),
		[]byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallClaudeSandbox(root, forbidden()); err != nil {
		t.Fatal(err)
	}

	s := readSettings(t, root)
	if s["outputStyle"] != "Gentleman" {
		t.Error("outputStyle was lost")
	}
	if _, ok := s["permissions"]; !ok {
		t.Error("permissions were lost")
	}
	sandbox, _ := s["sandbox"].(map[string]any)
	if _, ok := sandbox["network"]; !ok {
		t.Error("the existing network policy was lost")
	}
	fs, _ := sandbox["filesystem"].(map[string]any)
	if _, ok := fs["allowWrite"]; !ok {
		t.Error("an existing allowWrite list was lost")
	}
	var sawMine bool
	for _, e := range denyWrite(t, root) {
		if e == "./mine/**" {
			sawMine = true
		}
	}
	if !sawMine {
		t.Error("the user's own denyWrite entry was dropped")
	}
}

func TestUninstallWithdrawsOnlyVectorsEntriesAndLeavesEnabledAlone(t *testing.T) {
	// Someone may want the sandbox for reasons unrelated to vector. Turning off
	// a security boundary on the way out is not an uninstaller's decision.
	root := newRepo(t)
	if err := os.MkdirAll(filepath.Join(root, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".claude", "settings.json"),
		[]byte(`{"sandbox":{"filesystem":{"denyWrite":["./mine/**"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallClaudeSandbox(root, forbidden()); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveClaudeSandbox(root, forbidden()); err != nil {
		t.Fatal(err)
	}

	got := denyWrite(t, root)
	if len(got) != 1 || got[0] != "./mine/**" {
		t.Errorf("denyWrite = %v, want only the user's own entry", got)
	}
	if !SandboxEnabled(root) {
		t.Error("sandbox.enabled was turned off by uninstall")
	}
}

func TestSandboxRefusesToGuessAtBrokenJSON(t *testing.T) {
	root := newRepo(t)
	if err := os.MkdirAll(filepath.Join(root, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".claude", "settings.json"),
		[]byte("{ not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallClaudeSandbox(root, forbidden()); err == nil {
		t.Error("a malformed settings file was overwritten instead of reported")
	}
}

func TestSandboxIsOffByDefaultAndPersistsWhenSet(t *testing.T) {
	// Enabling the sandbox changes how every Bash command in the session runs,
	// so it is opt-in — and a later bare init must not silently revert it.
	root := newRepo(t)
	res, err := Init(root)
	if err != nil {
		t.Fatal(err)
	}
	if res.Policy.Mode.Sandbox {
		t.Error("the sandbox was on by default")
	}

	on := true
	if _, err := InitWith(root, &on); err != nil {
		t.Fatal(err)
	}
	res, err = Init(root)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Policy.Mode.Sandbox {
		t.Error("a bare init reverted the sandbox setting")
	}

	off := false
	if res, err = InitWith(root, &off); err != nil {
		t.Fatal(err)
	} else if res.Policy.Mode.Sandbox {
		t.Error("-no-sandbox did not turn it off")
	}
}

func TestSandboxEnabledReadsTheProjectFile(t *testing.T) {
	root := newRepo(t)
	if SandboxEnabled(root) {
		t.Error("reported enabled with no settings file at all")
	}
	if _, err := InstallClaudeSandbox(root, forbidden()); err != nil {
		t.Fatal(err)
	}
	if !SandboxEnabled(root) {
		t.Error("reported disabled right after being turned on")
	}
}

var _ = json.Marshal

// The rule is the platform's, not vector's: on Linux and WSL2 the sandbox
// mounts concrete paths and discards a write entry that still holds a wildcard
// once a trailing "/**" is removed. macOS honours all of them. Both halves are
// asserted here so a change to one is not mistaken for the other.
func TestSandboxInertNamesWhatThePlatformDiscards(t *testing.T) {
	cases := []struct {
		entry string
		inert bool
	}{
		{"./.env", false},
		{"./.vector/**", false},       // trailing /** is stripped, nothing left
		{"./.claude/hooks/**", false}, // same
		{"./.git/config", false},
		{"./.env.*", true},        // wildcard survives the strip
		{"./**/.gitignore", true}, // the /** is not trailing
		{"./cache?", true},
		{"./log[0-9]", true},
	}
	for _, c := range cases {
		want := c.inert && runtime.GOOS != "darwin"
		if got := sandboxInert(c.entry); got != want {
			t.Errorf("sandboxInert(%q) on %s = %v, want %v", c.entry, runtime.GOOS, got, want)
		}
	}
}

func TestSandboxInertReadsTheSettingsFile(t *testing.T) {
	root := newRepo(t)
	if got := SandboxInert(root); got != nil {
		t.Errorf("with no settings file at all: %v", got)
	}
	if _, err := InstallClaudeSandbox(root, forbidden()); err != nil {
		t.Fatal(err)
	}
	got := SandboxInert(root)
	if runtime.GOOS == "darwin" {
		if got != nil {
			t.Errorf("macOS honours wildcards, so nothing should be reported: %v", got)
		}
		return
	}
	if len(got) != 1 || got[0] != "./.env.*" {
		t.Errorf("got %v, want only ./.env.* — the other three survive the strip", got)
	}
}
