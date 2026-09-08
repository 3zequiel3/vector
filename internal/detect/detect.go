// Package detect establishes what a repository actually is, from local
// evidence only.
//
// It never assumes a stack, a package manager, or a version, and it never asks
// the network what "latest" is. Four kinds of truth are kept apart on purpose,
// because collapsing them into a single "version" is how a tool ends up
// confidently wrong:
//
//	declared   what a manifest asks for        ("next": "^16")
//	resolved   what a lockfile pins            (identity of the lockfile picks
//	                                            the package manager)
//	installed  what is on disk right now       (authoritative for "what runs")
//	upstream   what the registry offers        (deliberately not collected)
package detect

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Version keeps the three locally knowable truths apart. An empty field means
// "no local evidence", which is a different statement from "not installed".
type Version struct {
	Declared  string `toml:"declared,omitempty" json:"declared,omitempty"`
	Installed string `toml:"installed,omitempty" json:"installed,omitempty"`
}

// PackageManager is the resolved toolchain plus the evidence that chose it.
// Source is recorded so the answer can always explain itself.
type PackageManager struct {
	Name      string `toml:"name" json:"name"`
	Source    string `toml:"source" json:"source"`
	Lockfile  string `toml:"lockfile,omitempty" json:"lockfile,omitempty"`
	Declared  string `toml:"declared,omitempty" json:"declared,omitempty"`
	Installed string `toml:"installed,omitempty" json:"installed,omitempty"`
}

// Commands are the verification entry points of the project, taken from its
// own manifests. Vector invokes these; it never invents them.
type Commands struct {
	Test      string `toml:"test,omitempty" json:"test,omitempty"`
	Build     string `toml:"build,omitempty" json:"build,omitempty"`
	Typecheck string `toml:"typecheck,omitempty" json:"typecheck,omitempty"`
	Lint      string `toml:"lint,omitempty" json:"lint,omitempty"`
}

// Stack is the full local picture.
type Stack struct {
	Languages  []string           `toml:"languages" json:"languages"`
	PM         PackageManager     `toml:"package_manager" json:"package_manager"`
	Runtime    Version            `toml:"runtime" json:"runtime"`
	Frameworks map[string]Version `toml:"frameworks,omitempty" json:"frameworks,omitempty"`
	Notes      []string           `toml:"notes,omitempty" json:"notes,omitempty"`
}

// lockfiles maps a lockfile name to the package manager it implies. The
// lockfile's identity is the strongest local evidence there is: it is what the
// project last actually installed with.
var lockfiles = []struct{ file, pm, lang string }{
	{"pnpm-lock.yaml", "pnpm", "javascript"},
	{"bun.lock", "bun", "javascript"},
	{"bun.lockb", "bun", "javascript"},
	{"yarn.lock", "yarn", "javascript"},
	{"package-lock.json", "npm", "javascript"},
	{"uv.lock", "uv", "python"},
	{"poetry.lock", "poetry", "python"},
	{"pdm.lock", "pdm", "python"},
	{"Pipfile.lock", "pipenv", "python"},
	{"Cargo.lock", "cargo", "rust"},
	{"go.sum", "go", "go"},
	{"composer.lock", "composer", "php"},
	{"Gemfile.lock", "bundler", "ruby"},
}

// Detect inspects root and returns everything it can establish locally.
func Detect(root string) Stack {
	s := Stack{Frameworks: map[string]Version{}}

	langs := map[string]bool{}
	s.PM = detectPackageManager(root, langs)
	for _, m := range []struct{ file, lang string }{
		{"package.json", "javascript"},
		{"tsconfig.json", "typescript"},
		{"pyproject.toml", "python"},
		{"requirements.txt", "python"},
		{"go.mod", "go"},
		{"Cargo.toml", "rust"},
		{"composer.json", "php"},
		{"Gemfile", "ruby"},
		{"pom.xml", "java"},
		{"build.gradle", "java"},
		{"build.gradle.kts", "kotlin"},
	} {
		if exists(filepath.Join(root, m.file)) {
			langs[m.lang] = true
		}
	}
	for l := range langs {
		s.Languages = append(s.Languages, l)
	}
	sort.Strings(s.Languages)

	switch {
	case langs["javascript"] || langs["typescript"]:
		s.Runtime = nodeRuntime(root)
		s.Frameworks = nodeFrameworks(root)
	case langs["go"]:
		s.Runtime = Version{Declared: goDirective(root), Installed: toolVersion("go", "version")}
	case langs["python"]:
		s.Runtime = Version{Declared: pythonRequires(root), Installed: toolVersion("python3", "--version")}
	case langs["rust"]:
		s.Runtime = Version{Installed: toolVersion("rustc", "--version")}
	}

	s.Notes = append(s.Notes, staleness(root, s.PM)...)
	return s
}

// detectPackageManager walks from root toward the filesystem root, applying the
// same ordered strategies at each level: lockfile, then the packageManager
// field, then devEngines, then installed metadata. Walking upward matters for
// monorepos, where the lockfile lives above the package being worked on.
func detectPackageManager(start string, langs map[string]bool) PackageManager {
	dir := start
	for {
		for _, lf := range lockfiles {
			if exists(filepath.Join(dir, lf.file)) {
				langs[lf.lang] = true
				pm := PackageManager{
					Name:     lf.pm,
					Source:   "lockfile",
					Lockfile: rel(start, filepath.Join(dir, lf.file)),
				}
				pm.Declared = packageManagerField(dir)
				pm.Installed = pmVersion(lf.pm)
				return pm
			}
		}
		if f := packageManagerField(dir); f != "" {
			name, _, _ := strings.Cut(f, "@")
			return PackageManager{
				Name:      name,
				Source:    "packageManager field",
				Declared:  f,
				Installed: pmVersion(name),
			}
		}
		// The repository is the boundary. Walking past it would let an
		// unrelated manifest in a parent directory — a scratch package.json in
		// a home folder, say — silently decide this project's toolchain.
		if exists(filepath.Join(dir, ".git")) {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return PackageManager{Name: "unknown", Source: "no local evidence"}
}

type packageJSON struct {
	PackageManager string            `json:"packageManager"`
	Version        string            `json:"version"`
	Engines        map[string]string `json:"engines"`
	Scripts        map[string]string `json:"scripts"`
	Dependencies   map[string]string `json:"dependencies"`
	DevDeps        map[string]string `json:"devDependencies"`
	DevEngines     struct {
		PackageManager struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"packageManager"`
	} `json:"devEngines"`
}

func readPackageJSON(dir string) (packageJSON, bool) {
	var p packageJSON
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return p, false
	}
	if json.Unmarshal(data, &p) != nil {
		return p, false
	}
	return p, true
}

func packageManagerField(dir string) string {
	p, ok := readPackageJSON(dir)
	if !ok {
		return ""
	}
	if p.PackageManager != "" {
		return p.PackageManager
	}
	if n := p.DevEngines.PackageManager.Name; n != "" {
		if v := p.DevEngines.PackageManager.Version; v != "" {
			return n + "@" + v
		}
		return n
	}
	return ""
}

func nodeRuntime(root string) Version {
	v := Version{Installed: toolVersion("node", "--version")}
	if p, ok := readPackageJSON(root); ok {
		v.Declared = p.Engines["node"]
	}
	// An idiomatic version file is weaker evidence than engines, but it is
	// still local truth and worth reporting when nothing else says anything.
	if v.Declared == "" {
		if b, err := os.ReadFile(filepath.Join(root, ".nvmrc")); err == nil {
			v.Declared = strings.TrimSpace(string(b))
		}
	}
	return v
}

// nodeFrameworks reports declared versus installed for the dependencies most
// likely to change how code must be written. Installed wins on disagreement,
// and the disagreement itself is the finding.
func nodeFrameworks(root string) map[string]Version {
	out := map[string]Version{}
	p, ok := readPackageJSON(root)
	if !ok {
		return out
	}
	interesting := []string{
		"next", "react", "vue", "svelte", "@angular/core", "nuxt",
		"express", "fastify", "nestjs", "@nestjs/core",
		"typescript", "vite", "prisma", "@prisma/client", "tailwindcss",
	}
	for _, name := range interesting {
		declared := p.Dependencies[name]
		if declared == "" {
			declared = p.DevDeps[name]
		}
		installed := installedNodeVersion(root, name)
		if declared == "" && installed == "" {
			continue
		}
		out[name] = Version{Declared: declared, Installed: installed}
	}
	return out
}

func installedNodeVersion(root, name string) string {
	data, err := os.ReadFile(filepath.Join(root, "node_modules", filepath.FromSlash(name), "package.json"))
	if err != nil {
		return ""
	}
	var p packageJSON
	if json.Unmarshal(data, &p) != nil {
		return ""
	}
	return p.Version
}

func goDirective(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "go "); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func pythonRequires(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "pyproject.toml"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "requires-python"); ok {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "=")), `"'`)
		}
	}
	return ""
}

// pmVersion asks a package manager for its own version. Not every tool agrees
// on the flag: `go --version` does not exist, it is `go version`.
func pmVersion(name string) string {
	switch name {
	case "go":
		return toolVersion("go", "version")
	case "bundler":
		return toolVersion("bundle", "--version")
	default:
		return toolVersion(name, "--version")
	}
}

// installDirs names, per package manager, where installed packages land
// locally. Ecosystems absent from this map install outside the repository
// (Go modules, cargo registry), so there is no local directory to compare a
// lockfile against and no staleness claim to make.
var installDirs = map[string]string{
	"pnpm":     "node_modules",
	"npm":      "node_modules",
	"yarn":     "node_modules",
	"bun":      "node_modules",
	"uv":       ".venv",
	"poetry":   ".venv",
	"pdm":      ".venv",
	"pipenv":   ".venv",
	"composer": "vendor",
	"bundler":  "vendor/bundle",
}

// staleness reports local inconsistencies that would otherwise make every
// other detected value quietly untrustworthy.
func staleness(root string, pm PackageManager) []string {
	var notes []string
	if pm.Lockfile == "" {
		return notes
	}
	installDir, hasLocalInstall := installDirs[pm.Name]
	if !hasLocalInstall {
		return notes
	}
	lock := filepath.Join(root, filepath.FromSlash(pm.Lockfile))
	li, err := os.Stat(lock)
	if err != nil {
		return notes
	}
	di, err := os.Stat(filepath.Join(root, installDir))
	if err != nil {
		return append(notes, pm.Lockfile+" exists but "+installDir+" does not"+
			": no evidence of installed versions")
	}
	if li.ModTime().After(di.ModTime()) {
		notes = append(notes, pm.Lockfile+" is newer than "+installDir+
			": installed versions may not reflect the lockfile")
	}
	return notes
}

// DetectCommands reads the project's own verification entry points. Anything
// not found stays empty rather than being guessed.
func DetectCommands(root string, pm PackageManager) Commands {
	switch pm.Name {
	case "pnpm", "npm", "yarn", "bun":
		return nodeCommands(root, pm.Name)
	case "go":
		return Commands{Test: "go test ./...", Build: "go build ./...", Lint: "go vet ./..."}
	case "cargo":
		return Commands{Test: "cargo test", Build: "cargo build", Lint: "cargo clippy"}
	case "uv", "poetry", "pdm", "pipenv":
		return Commands{Test: pm.Name + " run pytest"}
	}
	return Commands{}
}

func nodeCommands(root, pm string) Commands {
	p, ok := readPackageJSON(root)
	if !ok {
		return Commands{}
	}
	run := func(script string) string {
		if _, has := p.Scripts[script]; !has {
			return ""
		}
		if pm == "yarn" {
			return "yarn " + script
		}
		return pm + " run " + script
	}
	c := Commands{
		Test:  firstNonEmpty(run("test"), run("test:unit")),
		Build: run("build"),
		Lint:  run("lint"),
		Typecheck: firstNonEmpty(
			run("typecheck"), run("type-check"), run("tsc"), run("check-types"),
		),
	}
	// A tsconfig with no typecheck script still has a usable type gate, and a
	// type gate is the cheapest verification a JS project has.
	if c.Typecheck == "" && exists(filepath.Join(root, "tsconfig.json")) {
		c.Typecheck = execPrefix(pm) + " tsc --noEmit"
	}
	return c
}

func execPrefix(pm string) string {
	switch pm {
	case "npm":
		return "npx"
	case "yarn":
		return "yarn"
	case "bun":
		return "bunx"
	default:
		return pm + " exec"
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func rel(base, target string) string {
	r, err := filepath.Rel(base, target)
	if err != nil {
		return filepath.ToSlash(target)
	}
	return filepath.ToSlash(r)
}

// toolVersion asks a tool for its own version. A missing tool is not an error:
// it is the absence of installed evidence, which is itself worth reporting.
// probeTimeout bounds a version probe.
//
// These run on every SessionStart, and they invoke whatever the developer has
// on PATH — which in practice is often a version-manager shim (nvm, pyenv,
// rbenv, a corporate npm wrapper) that may itself reach the network. `doctor`
// learned this and wrapped its own probes; the hot-path caller never did, so a
// wedged shim hung every session start with no limit at all.
//
// Two seconds, matching doctor. A version string is a nice-to-have; it is not
// worth a session that will not start.
const probeTimeout = 2 * time.Second

func toolVersion(bin string, args ...string) string {
	path, err := exec.LookPath(bin)
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, args...).Output()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	// Normalize the usual shapes: "v20.11.1", "go version go1.27.1 linux/amd64".
	line = strings.TrimPrefix(line, "v")
	for _, f := range strings.Fields(line) {
		if len(f) > 0 && f[0] >= '0' && f[0] <= '9' {
			return f
		}
		if v, ok := strings.CutPrefix(f, "go"); ok && v != "" && v[0] >= '0' && v[0] <= '9' {
			return v
		}
	}
	return line
}

// Disagrees reports a mismatch only when one can be proven without a semver
// range solver: an exact pin that differs from what is installed.
//
// A range like "^4" against an installed 4.3.3 is not a disagreement, and
// flagging it would be the tool asserting something it did not check. Ranges
// are reported side by side and left to the reader.
func Disagrees(declared, installed string) bool {
	if declared == "" || installed == "" {
		return false
	}
	if strings.ContainsAny(declared, "^~><*= |xX") {
		return false
	}
	return declared != installed
}
