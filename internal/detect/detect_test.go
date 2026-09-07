package detect

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// write creates a file (and its parents) under dir.
func write(t *testing.T, dir, rel, content string) string {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLockfileIdentityPicksPackageManager(t *testing.T) {
	cases := []struct{ lockfile, wantPM, wantLang string }{
		{"pnpm-lock.yaml", "pnpm", "javascript"},
		{"yarn.lock", "yarn", "javascript"},
		{"package-lock.json", "npm", "javascript"},
		{"bun.lock", "bun", "javascript"},
		{"uv.lock", "uv", "python"},
		{"poetry.lock", "poetry", "python"},
		{"Cargo.lock", "cargo", "rust"},
		{"go.sum", "go", "go"},
	}
	for _, c := range cases {
		t.Run(c.lockfile, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, c.lockfile, "")
			langs := map[string]bool{}
			pm := detectPackageManager(dir, langs)
			if pm.Name != c.wantPM {
				t.Errorf("name = %q, want %q", pm.Name, c.wantPM)
			}
			if pm.Source != "lockfile" {
				t.Errorf("source = %q, want %q", pm.Source, "lockfile")
			}
			if !langs[c.wantLang] {
				t.Errorf("language %q not inferred from %q", c.wantLang, c.lockfile)
			}
		})
	}
}

func TestLockfileBeatsPackageManagerField(t *testing.T) {
	// The field states intent; the lockfile is what the project last actually
	// installed with. Evidence outranks intent.
	dir := t.TempDir()
	write(t, dir, "pnpm-lock.yaml", "")
	write(t, dir, "package.json", `{"packageManager":"yarn@4.0.0"}`)

	pm := detectPackageManager(dir, map[string]bool{})
	if pm.Name != "pnpm" {
		t.Errorf("name = %q, want pnpm (lockfile must win)", pm.Name)
	}
	if pm.Declared != "yarn@4.0.0" {
		t.Errorf("declared = %q, want the field preserved as declared intent", pm.Declared)
	}
}

func TestPackageManagerFieldWithoutLockfile(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package.json", `{"packageManager":"pnpm@9.1.0"}`)

	pm := detectPackageManager(dir, map[string]bool{})
	if pm.Name != "pnpm" || pm.Source != "packageManager field" {
		t.Errorf("got %q from %q, want pnpm from the packageManager field", pm.Name, pm.Source)
	}
}

func TestDevEnginesFallback(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package.json", `{"devEngines":{"packageManager":{"name":"bun","version":"1.2.0"}}}`)

	if got := packageManagerField(dir); got != "bun@1.2.0" {
		t.Errorf("packageManagerField = %q, want bun@1.2.0", got)
	}
}

func TestUpwardTraversalFindsMonorepoLockfile(t *testing.T) {
	// In a workspace the lockfile lives above the package being worked on.
	// Stopping at the current directory would report "unknown" for every
	// package in the repository.
	root := t.TempDir()
	write(t, root, "pnpm-lock.yaml", "")
	pkg := filepath.Join(root, "apps", "web")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}

	pm := detectPackageManager(pkg, map[string]bool{})
	if pm.Name != "pnpm" {
		t.Fatalf("name = %q, want pnpm found by walking upward", pm.Name)
	}
	if pm.Lockfile != "../../pnpm-lock.yaml" {
		t.Errorf("lockfile = %q, want a path relative to the starting directory", pm.Lockfile)
	}
}

func TestFrameworksSeparateDeclaredFromInstalled(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package.json", `{"dependencies":{"next":"^16","react":"19.2.4"}}`)
	write(t, dir, "node_modules/next/package.json", `{"version":"16.2.12"}`)
	// react is declared but not installed: that absence is itself reportable.

	fw := nodeFrameworks(dir)
	if got := fw["next"]; got.Declared != "^16" || got.Installed != "16.2.12" {
		t.Errorf("next = %+v, want declared ^16 and installed 16.2.12 kept apart", got)
	}
	if got := fw["react"]; got.Declared != "19.2.4" || got.Installed != "" {
		t.Errorf("react = %+v, want an empty installed rather than a guess", got)
	}
	if _, present := fw["vue"]; present {
		t.Error("vue reported although nothing declares or installs it")
	}
}

func TestDisagreesOnlyClaimsWhatItCanProve(t *testing.T) {
	cases := []struct {
		declared, installed string
		want                bool
		why                 string
	}{
		{"^4", "4.3.3", false, "a caret range is satisfied by a newer patch"},
		{"^5", "5.9.3", false, "same, and flagging it would assert an unchecked claim"},
		{"~1.2.0", "1.2.9", false, "tilde ranges behave the same way"},
		{">=18", "20.11.1", false, "comparator ranges are not disagreements"},
		{"4.x", "4.3.3", false, "x-ranges are ranges"},
		{"16.2.12", "16.2.12", false, "an exact pin that matches"},
		{"16.2.12", "16.1.0", true, "an exact pin that does not match is provable"},
		{"", "4.3.3", false, "no declaration means nothing to disagree with"},
		{"^4", "", false, "no installation means nothing to compare"},
	}
	for _, c := range cases {
		if got := Disagrees(c.declared, c.installed); got != c.want {
			t.Errorf("Disagrees(%q, %q) = %v, want %v — %s",
				c.declared, c.installed, got, c.want, c.why)
		}
	}
}

func TestStalenessOnlyForEcosystemsThatInstallLocally(t *testing.T) {
	// Go modules and cargo crates live outside the repository, so there is no
	// local directory to compare against and no staleness claim to make.
	dir := t.TempDir()
	write(t, dir, "go.sum", "")
	if notes := staleness(dir, PackageManager{Name: "go", Lockfile: "go.sum"}); len(notes) != 0 {
		t.Errorf("notes = %v, want none for an ecosystem without a local install dir", notes)
	}
}

func TestStalenessReportsAMissingInstallDir(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "pnpm-lock.yaml", "")
	notes := staleness(dir, PackageManager{Name: "pnpm", Lockfile: "pnpm-lock.yaml"})
	if len(notes) != 1 {
		t.Fatalf("notes = %v, want one note about the missing node_modules", notes)
	}
}

func TestStalenessReportsALockfileNewerThanTheInstall(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	lock := write(t, dir, "pnpm-lock.yaml", "")
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(lock, future, future); err != nil {
		t.Fatal(err)
	}

	notes := staleness(dir, PackageManager{Name: "pnpm", Lockfile: "pnpm-lock.yaml"})
	if len(notes) != 1 {
		t.Fatalf("notes = %v, want one note that installed versions may be stale", notes)
	}
}

func TestCommandsComeFromTheProjectNotFromGuesses(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package.json", `{"scripts":{"test":"vitest","build":"next build"}}`)

	c := nodeCommands(dir, "pnpm")
	if c.Test != "pnpm run test" || c.Build != "pnpm run build" {
		t.Errorf("got %+v, want the declared scripts prefixed with the package manager", c)
	}
	if c.Lint != "" {
		t.Errorf("lint = %q, want empty because no lint script exists", c.Lint)
	}
	if c.Typecheck != "" {
		t.Errorf("typecheck = %q, want empty with no script and no tsconfig", c.Typecheck)
	}
}

func TestTypecheckFallsBackToTsconfig(t *testing.T) {
	// A project with types but no typecheck script still has the cheapest
	// verification gate available to it.
	dir := t.TempDir()
	write(t, dir, "package.json", `{"scripts":{}}`)
	write(t, dir, "tsconfig.json", `{}`)

	if got := nodeCommands(dir, "pnpm").Typecheck; got != "pnpm exec tsc --noEmit" {
		t.Errorf("typecheck = %q, want a tsconfig-derived fallback", got)
	}
}

func TestGoDirectiveAndPythonRequires(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.com/x\n\ngo 1.27.1\n\nrequire (\n)\n")
	if got := goDirective(dir); got != "1.27.1" {
		t.Errorf("goDirective = %q, want 1.27.1", got)
	}

	py := t.TempDir()
	write(t, py, "pyproject.toml", "[project]\nname = \"x\"\nrequires-python = \">=3.12\"\n")
	if got := pythonRequires(py); got != ">=3.12" {
		t.Errorf("pythonRequires = %q, want >=3.12", got)
	}
}

func TestNoEvidenceIsReportedAsSuch(t *testing.T) {
	// An empty directory must not produce an invented answer.
	dir := t.TempDir()
	pm := detectPackageManager(dir, map[string]bool{})
	if pm.Name != "unknown" && pm.Source == "lockfile" {
		t.Errorf("got %q from %q in an empty tree, want no fabricated evidence", pm.Name, pm.Source)
	}
}

func TestTraversalStopsAtTheRepositoryBoundary(t *testing.T) {
	// A manifest above the repository must never decide the project's
	// toolchain: a stray package.json in a parent directory is not evidence
	// about this repository.
	outer := t.TempDir()
	write(t, outer, "package-lock.json", "")

	repo := filepath.Join(outer, "inner")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	pm := detectPackageManager(repo, map[string]bool{})
	if pm.Name != "unknown" {
		t.Errorf("name = %q from %q, want unknown — detection escaped the repository",
			pm.Name, pm.Lockfile)
	}
}
