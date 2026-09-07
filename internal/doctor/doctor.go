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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/3zequiel3/vector/internal/detect"
	"github.com/3zequiel3/vector/internal/gitx"
	"github.com/3zequiel3/vector/internal/scope"
	"github.com/3zequiel3/vector/internal/setup"
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

	// The three fields below are set only by the integrations group, which
	// answers a different question than the rest of the report: not "is
	// enforcement working" but "what is here, and what would it buy me".
	// They are omitempty, so a consumer written against the original shape of
	// vector.doctor/v1 reads exactly the bytes it read before — which is why
	// the schema name is not bumped for them.
	Present bool   `json:"present,omitempty"`
	Unlocks string `json:"unlocks,omitempty"`
	Use     string `json:"use,omitempty"`
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
	b.checks = append(b.checks, Check{
		Group: group, Name: name, Level: l.String(), Detail: detail, Hint: hint,
	})
}

// addIntegration records an optional ecosystem piece. Its level is always Info:
// nothing in that group can be broken, only present or not, and borrowing Warn
// for "absent" would make a warning mean two different things in one report.
func (b *builder) addIntegration(name, detail, unlocks string, u use, present bool) {
	b.checks = append(b.checks, Check{
		Group:   "integrations",
		Name:    name,
		Level:   Info.String(),
		Detail:  detail,
		Present: present,
		Unlocks: unlocks,
		Use:     string(u),
	})
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
	checkIntegrations(b, root)

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

	// The sandbox is a different axis from the hook, not a stronger version of
	// it: it confines what a process can write regardless of whether vector
	// recognised the write. Reporting them together is the only honest summary.
	sandboxed := setup.SandboxEnabled(root)
	if sandboxed {
		b.add("enforcement", "sandbox", OK,
			"T3 confinement — the OS denies forbidden writes, including from scripts vector cannot see",
			"")
	} else {
		b.add("enforcement", "sandbox", Warn,
			"off — a script that opens a file is invisible to a tool-level hook",
			"turn it on with: vector init -sandbox")
	}

	if wired {
		tier := "T5"
		detail := "T5 interception — prevents before the write, with known leaks (~5%)"
		if sandboxed {
			tier = "T5+T3"
			detail = "T5 interception plus T3 confinement — the sandbox covers what the hook cannot see"
		}
		b.add("enforcement", "tier", Info, detail, "")
		return tier
	}
	if sandboxed {
		b.add("enforcement", "tier", Info,
			"T3 confinement without a hook — forbidden writes are denied by the OS, "+
				"but per-task scope is only detected after the fact", "")
		return "T3"
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

// use is Vector's actual relationship with an optional integration. The
// distinction it draws is the whole reason the group exists: "installed" and
// "wired up" are different facts, and a report that lets the first read as the
// second manufactures exactly the false confidence Vector refuses elsewhere.
type use string

const (
	// consumed means the code that ships today reads it. Nothing below is
	// consumed yet. The constant exists so that the day something is, the
	// report can say so — rather than the distinction being a comment that
	// quietly stops being true.
	consumed use = "consumed"
	// notYet means Vector could read it and no code does. This is the honest
	// answer for every integration except engram.
	notYet use = "not-yet"
	// unreadable means there is no external read path at all, so this one does
	// not become notYet by writing more Vector code.
	unreadable use = "unreadable"
)

// presentSuffix is what "it is here" is allowed to imply.
func (u use) presentSuffix() string {
	switch u {
	case consumed:
		return "present, and vector reads it"
	case unreadable:
		return "present, and vector cannot read it"
	default:
		return "present, and vector does not read it yet"
	}
}

// integration is one optional ecosystem piece: how to tell it is here, what it
// would unlock, and whether Vector reads it.
type integration struct {
	name string
	// evidence returns what was actually found — a resolved path, a version
	// the binary reported — or "" when the piece is absent. It reports
	// observations; it never infers presence from a name.
	evidence func(root string) string
	// looked names where evidence searched, so "not found" is a statement
	// about a concrete search rather than a shrug.
	looked  string
	unlocks string
	use     use
	// caveat qualifies presence when presence alone would mislead.
	caveat string
}

// integrations is the full optional surface. Every entry here is something
// Vector could use and — today — does not; see the use field on each.
var integrations = []integration{
	{
		name:     "gentle-ai",
		evidence: binaryEvidence("gentle-ai"),
		looked:   "no gentle-ai on PATH",
		unlocks: "seeding a task scope from `gentle-ai sdd-status <change> --cwd <repo> --json`, " +
			"whose actionContext.allowedEditRoots is already a declared boundary",
		use: notYet,
	},
	{
		name: "openspec",
		// Either half is enough: the binary without the directory is a tool
		// looking for a project, and the directory without the binary is a
		// project whose tool is one install away.
		evidence: openspecEvidence,
		looked:   "no openspec on PATH and no openspec/ directory in the repository",
		unlocks:  "structural validation of change artifacts via `openspec validate --changes --json`",
		use:      notYet,
	},
	{
		name: "chronicle",
		// The fingerprints file, not the directory: an empty .ledger/ proves
		// only that something once intended to track drift.
		evidence: repoPathEvidence(".ledger/fingerprints.json"),
		looked:   "no .ledger/fingerprints.json in the repository",
		unlocks:  "doc/code drift as an evidence source, read from the fingerprints chronicle already keeps",
		use:      notYet,
	},
	{
		name:     "atlas",
		evidence: repoPathEvidence("CHANGES.md"),
		looked:   "no CHANGES.md at the repository root",
		unlocks:  `seeding a task scope from a change's "Leer antes" pointers`,
		use:      notYet,
	},
	{
		name:     "engram",
		evidence: engramEvidence,
		looked:   "no ~/.engram/engram.db",
		unlocks:  "nothing vector can reach",
		use:      unreadable,
		caveat:   "no CLI and no documented external read path, so vector neither reads it nor should",
	},
}

// checkIntegrations reports which optional ecosystem pieces are on this machine
// and what each would unlock.
//
// Two rules shape the output. Absence is never a warning: these are all
// optional, and a column of warnings would read as a list of unmet dependencies
// for a tool that has exactly one. And presence is never reported on its own,
// because "installed" and "used by vector" are different facts — every piece
// here is currently the first and not the second, and a reader who takes one
// for the other has been given a capability that does not exist.
func checkIntegrations(b *builder, root string) {
	// git has already answered by the time this runs — Run resolved the
	// repository root through it. Saying so first is what makes the rest of the
	// group legible as optional rather than as things still to install.
	b.add("integrations", "git", OK,
		"the only hard requirement — it resolved this repository's root", "")

	for _, in := range integrations {
		ev := in.evidence(root)
		if ev == "" {
			b.addIntegration(in.name,
				in.looked+" — optional; vector works without it",
				in.unlocks, in.use, false)
			continue
		}
		detail := ev + " — " + in.use.presentSuffix()
		if in.caveat != "" {
			detail += ": " + in.caveat
		}
		b.addIntegration(in.name, detail, in.unlocks, in.use, true)
	}
}

// binaryEvidence reports where a binary resolved, with the version it claims
// when it will say.
func binaryEvidence(bin string) func(string) string {
	return func(string) string {
		path, err := exec.LookPath(bin)
		if err != nil {
			return ""
		}
		if v := binaryVersion(bin); v != "" {
			return v + " at " + path
		}
		return path + " (version not reported)"
	}
}

// binaryVersion asks a binary what version it is, trying the two spellings that
// are actually common.
//
// Failing to get one is not a finding: the flag differs between tools and
// between versions of the same tool, and the binary is still there either way.
// Reporting the path without a version is more truthful than reporting a usage
// message as though it were one, which is why an answer has to be short and
// contain a digit to be believed. The timeout is because doctor is run to
// diagnose a broken setup, and a diagnostic that hangs on a wedged third-party
// binary has become part of the problem.
func binaryVersion(bin string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	for _, args := range [][]string{{"--version"}, {"version"}} {
		out, err := exec.CommandContext(ctx, bin, args...).Output()
		if err != nil {
			continue
		}
		line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
		// Tools commonly answer with their own name first ("gentle-ai 2.4.0");
		// repeating it next to the path would be noise.
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), bin))
		if line == "" || len(line) > 40 || !strings.ContainsAny(line, "0123456789") {
			continue
		}
		return line
	}
	return ""
}

func openspecEvidence(root string) string {
	var found []string
	if path, err := exec.LookPath("openspec"); err == nil {
		found = append(found, path)
	}
	if info, err := os.Stat(filepath.Join(root, "openspec")); err == nil && info.IsDir() {
		found = append(found, "openspec/ in the repository")
	}
	return strings.Join(found, ", ")
}

// repoPathEvidence answers with the repository-relative path, not an absolute
// one: the reader already knows which repository this is, and the absolute form
// would say more about the machine than about the project.
func repoPathEvidence(rel string) func(string) string {
	return func(root string) string {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			return ""
		}
		return rel
	}
}

// engramEvidence looks in the home directory rather than the repository:
// engram's store is per-developer, and finding it says nothing about this
// project. It is reported anyway so that "vector does not read it" is an
// explicit answer instead of a silence someone has to interpret.
func engramEvidence(string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	path := filepath.Join(home, ".engram", "engram.db")
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
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
		// What an integration would unlock is the answer to "does not having it
		// cost me anything", so it belongs next to the line that says whether
		// it is there — including when it is, since the reader still has to
		// learn that vector is not using it for that yet.
		if c.Unlocks != "" {
			fmt.Fprintf(&b, "       %-18s → would unlock: %s\n", "", c.Unlocks)
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
