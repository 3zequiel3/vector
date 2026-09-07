// Package doctor reports whether Vector is actually doing anything.
//
// The failure it exists to catch is the quiet one: enforcement that is
// configured but dead. A hook whose name changed between agent versions, a
// scope pattern with a typo that silently matches nothing, a verification
// command naming a binary that is not installed — each of these leaves a
// repository looking governed while nothing is being checked.
//
// Every check answers with what it verified, not with reassurance. Nothing
// here reports "safe"; the strongest thing it says is which enforcement tier
// was actually reached.
package doctor

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/3zequiel3/vector/internal/detect"
	"github.com/3zequiel3/vector/internal/gitx"
	"github.com/3zequiel3/vector/internal/scope"
)

// Level is the severity of a single check.
type Level int

const (
	// Info states a fact without judging it.
	Info Level = iota
	// OK means the check verified something works.
	OK
	// Warn means degraded but functional.
	Warn
	// Fail means something claims to work and does not.
	Fail
)

func (l Level) String() string {
	switch l {
	case OK:
		return "ok"
	case Warn:
		return "warn"
	case Fail:
		return "fail"
	default:
		return "info"
	}
}

// Check is one diagnostic result.
type Check struct {
	Group  string `json:"group"`
	Name   string `json:"name"`
	Level  string `json:"level"`
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

// Report is the full diagnostic.
type Report struct {
	Schema string  `json:"schema"`
	Root   string  `json:"root"`
	Tier   string  `json:"enforcement_tier"`
	Checks []Check `json:"checks"`
}

// ExitCode is 1 when any check failed. A warning is not a failure: degraded
// enforcement is still enforcement, and treating it as broken would train
// people to ignore the output.
func (r Report) ExitCode() int {
	for _, c := range r.Checks {
		if c.Level == Fail.String() {
			return 1
		}
	}
	return 0
}

type builder struct{ checks []Check }

func (b *builder) add(group, name string, l Level, detail, hint string) {
	b.checks = append(b.checks, Check{group, name, l.String(), detail, hint})
}

// Run performs every diagnostic against the repository containing dir.
func Run(dir string) (Report, error) {
	root, err := gitx.Root(dir)
	if err != nil {
		return Report{}, err
	}
	b := &builder{}

	pol := checkPolicy(b, root)
	stack := checkStack(b, root)
	checkCommands(b, root, stack)
	checkScopes(b, root, pol)
	tier := checkEnforcement(b, root)
	checkBinary(b)

	return Report{
		Schema: "vector.doctor/v1",
		Root:   root,
		Tier:   tier,
		Checks: b.checks,
	}, nil
}

func checkPolicy(b *builder, root string) scope.Policy {
	path := filepath.Join(root, ".vector", "policy.toml")
	if _, err := os.Stat(path); err != nil {
		b.add("repository", "policy.toml", Warn,
			"missing; falling back to defaults",
			"run: vector init")
		return scope.DefaultPolicy()
	}
	pol, err := scope.LoadPolicy(root)
	if err != nil {
		b.add("repository", "policy.toml", Fail, err.Error(),
			"fix the TOML; until then no policy applies")
		return scope.DefaultPolicy()
	}
	b.add("repository", "policy.toml", OK, "parses correctly", "")

	switch pol.Mode.Enforcement {
	case "strict":
		b.add("repository", "mode", Info, "strict — a violation is a hard failure", "")
	case "advisory":
		b.add("repository", "mode", Info, "advisory — reported, not blocked", "")
	default:
		b.add("repository", "mode", Warn,
			fmt.Sprintf("unknown value %q; treated as advisory", pol.Mode.Enforcement),
			"use advisory or strict")
	}

	// A forbidden list that does not cover Vector's own configuration leaves
	// the documented self-weakening hole open: an agent that can edit the rules
	// that constrain it is not constrained.
	if !covers(pol.Scope.AlwaysForbidden, ".vector/policy.toml") {
		b.add("repository", "self-protection", Fail,
			"always_forbidden does not cover .vector/ — the agent can rewrite its own policy",
			`add ".vector/**" to always_forbidden`)
	} else {
		b.add("repository", "self-protection", OK,
			"vector's own config is outside the writable scope", "")
	}
	return pol
}

func checkStack(b *builder, root string) detect.Stack {
	s := detect.Detect(root)
	if s.PM.Name == "" || s.PM.Name == "unknown" {
		b.add("stack", "package manager", Warn,
			"no local evidence; verification commands cannot be derived",
			"if the project uses one, declare it under [commands] in policy.toml")
	} else {
		detail := s.PM.Name + " (" + s.PM.Source
		if s.PM.Lockfile != "" {
			detail += ": " + s.PM.Lockfile
		}
		detail += ")"
		b.add("stack", "package manager", OK, detail, "")
	}

	for name, v := range s.Frameworks {
		if detect.Disagrees(v.Declared, v.Installed) {
			b.add("stack", name, Warn,
				fmt.Sprintf("declared %s, installed %s", v.Declared, v.Installed),
				"installed is what runs; the manifest asks for something else")
		}
	}
	for _, n := range s.Notes {
		b.add("stack", "freshness", Warn, n, "")
	}
	return s
}

func checkCommands(b *builder, root string, s detect.Stack) {
	cmds := detect.DetectCommands(root, s.PM)
	named := []struct {
		name, cmd string
	}{
		{"test", cmds.Test},
		{"typecheck", cmds.Typecheck},
		{"build", cmds.Build},
		{"lint", cmds.Lint},
	}
	any := false
	for _, c := range named {
		if c.cmd == "" {
			continue
		}
		any = true
		// A command naming a binary that is not installed would fail at
		// verification time and look like a broken change instead of a broken
		// setup. Checking it now is the difference between the two.
		bin := strings.Fields(c.cmd)[0]
		if _, err := exec.LookPath(bin); err != nil {
			b.add("verification", c.name, Fail,
				fmt.Sprintf("%q, but %q is not on PATH", c.cmd, bin),
				"install the tool or fix [commands] in policy.toml")
			continue
		}
		b.add("verification", c.name, OK, c.cmd, "")
	}
	if !any {
		b.add("verification", "commands", Warn,
			"none detected; a verdict cannot rest on evidence",
			"declare them under [commands] in policy.toml")
	}
}

func checkScopes(b *builder, root string, pol scope.Policy) {
	dir := filepath.Join(root, ".vector", "scope")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) || len(entries) == 0 {
		b.add("scopes", "declared", Info,
			"none; audit can only check forbidden paths", "")
		return
	}
	if err != nil {
		b.add("scopes", "read", Fail, err.Error(), "")
		return
	}

	inventory, terr := gitx.AllFiles(root)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".toml")
		sc, err := scope.LoadScope(root, id)
		if err != nil {
			b.add("scopes", id, Fail, err.Error(), "fix the scope TOML")
			continue
		}
		if sc == nil || len(sc.EffectiveWrite()) == 0 {
			b.add("scopes", id, Fail,
				"declares no writable path; everything would be out of scope",
				"add write patterns")
			continue
		}

		// A pattern that matches nothing is almost always a typo, and a typo
		// here is the worst outcome available: the boundary looks declared and
		// silently excludes everything the task actually needs.
		if terr == nil && len(inventory) > 0 {
			var dead []string
			for _, pat := range sc.EffectiveWrite() {
				if !matchesAny(pat, inventory) {
					dead = append(dead, pat)
				}
			}
			if len(dead) > 0 {
				b.add("scopes", id, Warn,
					"patterns matching no file in the repo: "+strings.Join(dead, ", "),
					"usually a typo; check the path")
				continue
			}
		}

		detail := fmt.Sprintf("%d pattern(s)", len(sc.EffectiveWrite()))
		if n := len(sc.Expansions); n > 0 {
			detail += fmt.Sprintf(", widened %d time(s)", n)
		}
		b.add("scopes", id, OK, detail, "")
	}
	if cur := scope.Current(root); cur != "" {
		b.add("scopes", "active", Info, cur+" — used when -task is omitted", "")
	} else {
		b.add("scopes", "active", Warn,
			"none selected; audit falls back to checking forbidden paths only",
			"run: vector scope new <id> -w <pattern>")
	}
	if !pol.Scope.ExpansionRequiresEvidence {
		b.add("scopes", "expansion", Warn,
			"expansion_requires_evidence = false; the boundary can widen on assertion alone",
			"set it to true to require evidence")
	}
}

// agent describes where a coding agent keeps the configuration that would
// register a Vector hook.
type agent struct {
	name    string
	bin     string
	configs []string // relative to the repo, or ~-prefixed
}

var agents = []agent{
	{"Claude Code", "claude", []string{".claude/settings.json", ".claude/settings.local.json", "~/.claude/settings.json"}},
	{"Codex CLI", "codex", []string{".codex/hooks.json", "~/.codex/hooks.json", "~/.codex/config.toml"}},
	{"Cursor", "cursor-agent", []string{".cursor/hooks.json", "~/.cursor/hooks.json"}},
	{"Gemini CLI", "gemini", []string{".gemini/settings.json", "~/.gemini/settings.json"}},
	{"OpenCode", "opencode", []string{"opencode.json", ".opencode/plugin"}},
	{"Amp", "amp", []string{".amp/plugins", "~/.config/amp/settings.json"}},
}

// checkEnforcement reports which agents are present and whether any of them
// would actually invoke Vector, then names the tier genuinely reached.
func checkEnforcement(b *builder, root string) string {
	wired := false
	present := 0
	for _, a := range agents {
		if _, err := exec.LookPath(a.bin); err != nil {
			continue
		}
		present++
		if where := hookRegistered(root, a.configs); where != "" {
			wired = true
			b.add("enforcement", a.name, OK, "vector hook registered in "+where, "")
		} else {
			b.add("enforcement", a.name, Warn,
				"installed, no vector hook registered",
				"without a hook there is no prevention; audit still detects after the fact")
		}
	}
	if present == 0 {
		b.add("enforcement", "agents", Info,
			"none detected on PATH", "")
	}

	if wired {
		b.add("enforcement", "tier", Info,
			"T5 interception — prevents before the write, with known leaks (~5%)", "")
		return "T5"
	}
	b.add("enforcement", "tier", Info,
		"T2 observation — vector audit detects 100% after the fact, but prevents nothing", "")
	return "T2"
}

// hookRegistered looks for Vector's name inside an agent's configuration. It
// deliberately does not parse each agent's schema: the schemas differ, they
// change between versions, and the question here is only whether Vector is
// mentioned at all.
func hookRegistered(root string, configs []string) string {
	for _, c := range configs {
		path := c
		if strings.HasPrefix(c, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				continue
			}
			path = filepath.Join(home, c[2:])
		} else {
			path = filepath.Join(root, filepath.FromSlash(c))
		}

		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if info.IsDir() {
			if found, _ := dirMentionsVector(path); found {
				return c
			}
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if mentionsVectorHook(data) {
			return c
		}
	}
	return ""
}

func dirMentionsVector(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if mentionsVectorHook(data) {
			return true, nil
		}
	}
	return false, nil
}

// mentionsVectorHook looks for an invocation of the vector binary rather than
// the bare word, so a comment or an unrelated "vector" in a path does not read
// as working enforcement.
func mentionsVectorHook(data []byte) bool {
	text := string(data)
	for _, needle := range []string{"vector hook", "vector audit", "vector-hook", "/vector "} {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func checkBinary(b *builder) {
	path, err := exec.LookPath("vector")
	if err != nil {
		b.add("installation", "binary", Warn,
			"vector is not on PATH",
			"a hook invokes it by name; install it or use an absolute path in the config")
		return
	}
	b.add("installation", "binary", OK, path, "")
}

func covers(patterns []string, path string) bool {
	r := scope.Ruleset{Forbidden: patterns}
	d, _ := r.Decide(path)
	return d == scope.Forbidden
}

func matchesAny(pattern string, files []string) bool {
	r := scope.Ruleset{Write: []string{pattern}, Declared: true}
	for _, f := range files {
		if d, _ := r.Decide(f); d == scope.Allowed {
			return true
		}
	}
	return false
}

// WriteJSON emits the report for machine consumers.
func (r Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteText emits the report grouped, in the order the checks were produced.
func (r Report) WriteText(w io.Writer) error {
	var b strings.Builder
	symbols := map[string]string{"ok": "ok  ", "warn": "warn", "fail": "FAIL", "info": "    "}

	group := ""
	for _, c := range r.Checks {
		if c.Group != group {
			group = c.Group
			fmt.Fprintf(&b, "\n%s\n", strings.ToUpper(group))
		}
		fmt.Fprintf(&b, "  %s %-18s %s\n", symbols[c.Level], c.Name, c.Detail)
		if c.Hint != "" && c.Level != OK.String() {
			fmt.Fprintf(&b, "       %-18s → %s\n", "", c.Hint)
		}
	}

	var fails, warns int
	for _, c := range r.Checks {
		switch c.Level {
		case "fail":
			fails++
		case "warn":
			warns++
		}
	}
	fmt.Fprintf(&b, "\n%d failure(s), %d warning(s) — enforcement at %s\n", fails, warns, r.Tier)

	_, err := io.WriteString(w, b.String())
	return err
}
