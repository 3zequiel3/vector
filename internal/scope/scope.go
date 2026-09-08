// Package scope models the declared write boundary of a task and decides,
// deterministically, whether a given path falls inside it.
//
// Every decision here is a pure set operation over normalized paths. No model
// call, no heuristic, no network. That is the whole point: this is the one
// layer that cannot be talked out of its answer.
package scope

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/bmatcuk/doublestar/v4"

	"github.com/3zequiel3/vector/internal/detect"
)

// Decision is the outcome of matching a path against a Ruleset.
type Decision int

const (
	// Allowed means the path matches a declared write pattern.
	Allowed Decision = iota
	// OutOfScope means the path matches nothing; the task never claimed it.
	OutOfScope
	// Forbidden means the path matches an explicit deny pattern. Deny wins
	// over every allow, always.
	Forbidden
)

func (d Decision) String() string {
	switch d {
	case Allowed:
		return "allowed"
	case Forbidden:
		return "forbidden"
	default:
		return "out_of_scope"
	}
}

// Policy is the per-repository contract, versioned in .vector/policy.toml.
// It holds only what a human actually decides; everything else is detected.
type Policy struct {
	Scope struct {
		AlwaysForbidden           []string `toml:"always_forbidden"`
		ExpansionRequiresEvidence bool     `toml:"expansion_requires_evidence"`
		// HighRisk names paths that are written deliberately or not at all.
		//
		// It is not a second forbidden list. A migration, a workflow, an auth
		// module and a terraform plan are all things a change legitimately
		// edits; what separates them from a component is that nobody edits one
		// by accident, and the cost of finding out later is not the same. So
		// these are never denied — they are named, every time, and they cannot
		// be swept up by a boundary that authorized everything.
		HighRisk []string `toml:"high_risk"`
	} `toml:"scope"`
	Mode struct {
		Enforcement string `toml:"enforcement"` // "advisory" | "strict"
		Sandbox     bool   `toml:"sandbox"`
	} `toml:"mode"`
	// Commands corrects detection, and is empty in a repository where
	// detection is right — which is most of them.
	//
	// Detection reads the project's own manifests and is deliberately
	// conservative: it knows package-manager scripts and a handful of
	// ecosystem defaults. A Makefile, a bespoke runner, or a monorepo whose
	// tests live behind a wrapper are all invisible to it, and until now a
	// repository had no way to say so. `vector doctor` would report the
	// command it could not find and offer no way to correct it.
	Commands detect.Commands `toml:"commands"`
}

// MergeCommands lays the policy's overrides over what detection found, field
// by field.
//
// Per field rather than wholesale: a repository that has to correct one
// command should not thereby have to restate the three that were already
// right, and then keep them current by hand forever.
//
// Detection stays live underneath. `vector init` writes the detected commands
// into policy.toml commented out, so an upgraded project needs no edit here;
// only a value someone deliberately uncommented wins, and it goes on winning
// until they remove it. That asymmetry is the point — an override is a claim a
// human made, and vector should not quietly withdraw it.
func (p Policy) MergeCommands(detected detect.Commands) detect.Commands {
	over := func(dst *string, src string) {
		if s := strings.TrimSpace(src); s != "" {
			*dst = s
		}
	}
	over(&detected.Test, p.Commands.Test)
	over(&detected.Typecheck, p.Commands.Typecheck)
	over(&detected.Build, p.Commands.Build)
	over(&detected.Lint, p.Commands.Lint)
	return detected
}

// DefaultPolicy is what a repository gets before anyone edits policy.toml.
// The forbidden set exists to close a documented failure mode: an agent that
// can write to its own enforcement config can weaken its own enforcement.
func DefaultPolicy() Policy {
	var p Policy
	p.Scope.AlwaysForbidden = []string{
		".vector/**",
		".claude/settings.json",
		".claude/settings.local.json",
		".claude/hooks/**",
		".codex/hooks.json",
		".cursor/hooks.json",
		".env",
		".env.*",
	}
	// Deliberately short, and shorter than the obvious version. Every entry is
	// a path whose name says what it does in more than one ecosystem, and a
	// list that tries to be exhaustive goes stale and starts being wrong out
	// loud. A project adds its own; this is the set nobody would argue with.
	//
	// Three obvious candidates are deliberately absent, because each one fires
	// on ordinary work and a rule that cries wolf on Tuesday is off by Friday.
	// "**/auth/**" is the canonical example in every write-up of this failure
	// and also matches src/auth/LoginButton.tsx, which is a front-end folder,
	// not a security boundary. "**/migrate/**" matches any Go package named
	// migrate and every vendored copy of golang-migrate. "**/*.key" matches
	// Keynote decks and licence placeholders, since an extension is not
	// content. A project that means them can add them in one line.
	p.Scope.HighRisk = []string{
		"**/migrations/**",
		".github/workflows/**",
		".gitlab-ci.yml",
		"Jenkinsfile",
		"**/*.tf",
		"**/*.tfvars",
		"Dockerfile",
		"**/docker-compose*.yml",
		"**/*.pem",
		"**/*.p12",
		"**/*.pfx",
	}
	p.Scope.ExpansionRequiresEvidence = true
	p.Mode.Enforcement = "advisory"
	return p
}

// Scope is the declared boundary of a single task, in .vector/scope/<id>.toml.
type Scope struct {
	TaskID     string      `toml:"-"`
	Objective  string      `toml:"objective"`
	Write      []string    `toml:"write"`
	Forbidden  []string    `toml:"forbidden"`
	Expansions []Expansion `toml:"expansion"`
}

// Expansion is one recorded widening of a task's boundary.
//
// Expansions are kept separate from the original Write list on purpose. The
// initial declaration stays legible, and the file itself becomes the record of
// how — and on what evidence — the boundary grew. Silently editing Write in
// place would erase exactly the history that makes an expansion reviewable.
type Expansion struct {
	At       string   `toml:"at"`
	Reason   string   `toml:"reason"`
	Evidence string   `toml:"evidence"`
	Write    []string `toml:"write"`
}

// ExpansionReasons are the only grounds on which a boundary may widen.
// Anything outside this list is drift wearing a justification.
var ExpansionReasons = map[string]string{
	"blocking":     "prevents completing the requested task",
	"security":     "concrete security or data-integrity risk introduced by the change",
	"invariant":    "violates an explicit project invariant",
	"verification": "deterministic verification proves it necessary",
	"authorized":   "explicitly authorized by a human",
}

// EffectiveWrite is the original declaration plus every recorded expansion.
func (s *Scope) EffectiveWrite() []string {
	if s == nil {
		return nil
	}
	out := append([]string{}, s.Write...)
	for _, e := range s.Expansions {
		out = append(out, e.Write...)
	}
	return out
}

// Ruleset is a Policy and a Scope collapsed into the two lists that matter.
// A nil Scope is legitimate: it means no task boundary was declared, so only
// the always-forbidden rules apply.
type Ruleset struct {
	Write      []string
	Forbidden  []string
	Declared   bool // false when no scope file was loaded
	Objective  string
	TaskID     string
	Expansions int // how many times this boundary was widened
	// HighRisk are the policy's dangerous-path patterns, carried here so a
	// decision and its severity are answered from the same place.
	HighRisk []string
}

// BuildRuleset merges a policy and an optional scope into a decidable ruleset.
func BuildRuleset(p Policy, s *Scope) Ruleset {
	r := Ruleset{Forbidden: append([]string{}, p.Scope.AlwaysForbidden...)}
	if s == nil {
		return r
	}
	r.Declared = true
	r.Objective = s.Objective
	r.TaskID = s.TaskID
	r.Write = append(r.Write, s.EffectiveWrite()...)
	r.Forbidden = append(r.Forbidden, s.Forbidden...)
	r.Expansions = len(s.Expansions)
	r.HighRisk = append([]string{}, p.Scope.HighRisk...)
	return r
}

// Risky reports whether a path is one the policy calls high risk, and the
// pattern that says so.
//
// It is independent of Decide. A path can be perfectly in scope and still be a
// migration, and those are different facts about it — collapsing them would
// force a tool that wants to say "allowed, and worth looking at" to pick one.
func (r Ruleset) Risky(rel string) (string, bool) {
	return matchAny(r.HighRisk, rel)
}

// Undeclared returns the high-risk paths among rel that the boundary allows
// without ever having named, and for each, the pattern that swept it up.
//
// The rule is one line: the pattern that authorizes a dangerous path must
// itself be a dangerous path. "migrations/**" is matched by the risk pattern
// "**/migrations/**" and so declares a migration; "src/**" is not, and so does
// not, however many migrations happen to live under src.
//
// The first version of this asked a different question — whether the boundary
// was literally "**" — and it was worth almost nothing. "**/**" matches
// everything too and was not on the list; enumerating the top-level
// directories reaches the same coverage with no catch-all in sight; and most
// repositories put the whole application under one src/, so the check was
// really "did you type two asterisks". Asking about the pattern's own risk
// instead has the property the other one lacked: the only way to evade it is
// to declare "migrations/**", which is the declaration that was wanted.
//
// Nothing here denies. It reports which dangerous paths were covered by
// something that was not about them.
func (r Ruleset) Undeclared(rel string) (string, bool) {
	risk, risky := r.Risky(rel)
	if !risky || !r.Declared {
		return "", false
	}
	for _, w := range r.Write {
		if _, ok := matchAny([]string{w}, rel); !ok {
			continue
		}
		// The pattern let it through. Does the pattern say so?
		if _, named := matchAny(r.HighRisk, normalizePattern(w)); named {
			return "", false
		}
	}
	return risk, true
}

// Decide classifies a repo-relative path. It returns the decision and the
// pattern responsible for it, so every answer can explain itself without an LLM.
//
// Deny is evaluated before allow. An undeclared ruleset cannot report
// OutOfScope, because a boundary that was never drawn cannot be crossed.
func (r Ruleset) Decide(rel string) (Decision, string) {
	if pat, ok := matchAny(r.Forbidden, rel); ok {
		return Forbidden, pat
	}
	if pat, ok := matchAny(r.Write, rel); ok {
		return Allowed, pat
	}
	if !r.Declared {
		return Allowed, ""
	}
	return OutOfScope, ""
}

func matchAny(patterns []string, rel string) (string, bool) {
	for _, raw := range patterns {
		pat := normalizePattern(raw)
		if pat == "" {
			continue
		}
		if ok, err := doublestar.Match(pat, rel); err == nil && ok {
			return raw, true
		}
		// A bare directory pattern covers everything beneath it, so that
		// "src/dashboard" behaves the way a person expects it to.
		if !strings.ContainsAny(pat, "*?[") {
			if rel == pat || strings.HasPrefix(rel, pat+"/") {
				return raw, true
			}
		}
	}
	return "", false
}

func normalizePattern(p string) string {
	p = strings.TrimSpace(p)
	p = filepath.ToSlash(p)
	p = strings.TrimPrefix(p, "./")
	return strings.TrimSuffix(p, "/")
}

// ErrEscapesRoot reports a path that resolves outside the repository. Refusing
// these is not paranoia: a matcher that tests a raw, uncleaned path turns
// "../" into a pattern-authoring problem instead of an invariant.
var ErrEscapesRoot = errors.New("path escapes repository root")

// Normalize converts any path into the cleaned, slash-separated,
// repo-relative form that every rule is written against.
func Normalize(root, p string) (string, error) {
	p = filepath.ToSlash(strings.TrimSpace(p))
	if p == "" {
		return "", ErrEscapesRoot
	}
	abs := p
	if !filepath.IsAbs(p) {
		abs = filepath.Join(root, p)
	}
	rel, err := filepath.Rel(root, filepath.Clean(abs))
	if err != nil {
		return "", ErrEscapesRoot
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", ErrEscapesRoot
	}
	return rel, nil
}

// LoadPolicy reads .vector/policy.toml, falling back to defaults when the file
// does not exist. A missing policy is a valid state, not an error.
func LoadPolicy(root string) (Policy, error) {
	path := filepath.Join(root, ".vector", "policy.toml")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return DefaultPolicy(), nil
	}
	if err != nil {
		return Policy{}, err
	}
	p := DefaultPolicy()
	if err := toml.Unmarshal(data, &p); err != nil {
		return Policy{}, fmt.Errorf("%s: %w", path, err)
	}
	if p.Mode.Enforcement == "" {
		p.Mode.Enforcement = "advisory"
	}
	return p, nil
}

// LoadScope reads .vector/scope/<taskID>.toml. A nil result with a nil error
// means no scope was declared for this task.
func LoadScope(root, taskID string) (*Scope, error) {
	if taskID == "" {
		return nil, nil
	}
	path := filepath.Join(root, ".vector", "scope", taskID+".toml")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s Scope
	if err := toml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s.TaskID = taskID
	return &s, nil
}

// currentFile points at the task whose scope is in force. It exists so that no
// everyday command needs a -task flag: typing an id on every invocation is the
// kind of friction that gets a tool uninstalled.
func currentFile(root string) string {
	return filepath.Join(root, ".vector", "current")
}

// SetCurrent records which task is active.
func SetCurrent(root, taskID string) error {
	if err := os.MkdirAll(filepath.Join(root, ".vector"), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(currentFile(root), taskID+"\n")
}

// Current returns the active task, or "" when none is set. A missing pointer is
// a normal state, not an error.
func Current(root string) string {
	data, err := os.ReadFile(currentFile(root))
	if err != nil {
		return ""
	}
	id := strings.TrimSpace(string(data))
	if id == "" {
		return ""
	}
	// A pointer to a scope that no longer exists is stale, and answering with
	// it would silently enforce a boundary nobody declared for this work.
	if _, err := os.Stat(filepath.Join(root, ".vector", "scope", id+".toml")); err != nil {
		return ""
	}
	return id
}

// ClearCurrent forgets the active task.
func ClearCurrent(root string) error {
	err := os.Remove(currentFile(root))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// writeFileAtomic writes via a temporary file and a rename, so a crash cannot
// leave a half-written pointer behind.
func writeFileAtomic(path, content string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".vector-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Targets returns every repo-relative path a write to p would actually reach:
// p itself, and — when p or one of its parent directories is a symlink — what
// it resolves to.
//
// Checking only the lexical path is a documented way this class of tool leaks.
// A matcher that tests the path as written accepts `ln -s .claude/settings.json
// src/notes.json` followed by a write to src/notes.json: the link sits inside
// the declared scope, the target does not, and the rule that was supposed to
// keep enforcement from being edited away never fires. The same works one level
// up, with a symlinked directory.
//
// A link resolving outside the repository is reported as an escape rather than
// as a path, because no rule written against repo-relative patterns can
// meaningfully decide it.
func Targets(root, p string) ([]string, error) {
	rel, err := Normalize(root, p)
	if err != nil {
		return nil, err
	}
	out := []string{rel}

	abs := filepath.Join(root, filepath.FromSlash(rel))
	resolved, err := resolveExisting(abs)
	if err != nil || resolved == abs {
		// Nothing resolvable, or nothing to resolve. The lexical path stands.
		return out, nil
	}
	target, err := Normalize(root, resolved)
	if err != nil {
		// The link leaves the repository. That is an escape, not a second path.
		return nil, ErrEscapesRoot
	}
	if target != rel {
		out = append(out, target)
	}
	return out, nil
}

// resolveExisting resolves symlinks for a path that may not exist yet. A file
// about to be created has no link of its own, but the directory holding it may
// be one, so resolution walks up to the nearest existing ancestor and rebuilds
// the remainder onto it.
func resolveExisting(abs string) (string, error) {
	rest := ""
	cur := abs
	for i := 0; i < 64; i++ {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(resolved, rest), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", os.ErrNotExist
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
	return "", os.ErrNotExist
}

// bookkeeping names the files vector writes while doing its job.
//
// A scope file appears the moment a task declares its boundary, and an
// observation appears the moment the agent is told to record rather than act.
// Both are vector operating, not the change under review — and because
// .vector/** is forbidden so the agent cannot edit its own constraints, every
// task was opening with a FORBIDDEN verdict about vector's own footprint. A
// tool whose first answer on every task is a false alarm teaches people to
// ignore its answers.
//
// policy.toml is deliberately absent from this list. It is the enforcement
// contract, and a change to it is exactly the thing worth reporting.
var bookkeeping = []string{
	".vector/scope/**",
	".vector/observations.md",
	".vector/.gitignore",
	// Per-developer working state. init gitignores these, so normally they
	// never reach a diff at all — but a repository set up before an entry
	// existed still has them untracked, and reporting vector's own scratch
	// files as violations of someone's change would be the same false alarm
	// by a different route.
	".vector/current",
	".vector/nudged",
	".vector/attempts",
	".vector/verdicts",
}

// IsBookkeeping reports whether a repo-relative path is one vector writes for
// itself. The hook still denies the agent writing any of them; this only keeps
// vector's own footprint out of a report about someone else's change.
func IsBookkeeping(rel string) bool {
	_, ok := matchAny(bookkeeping, rel)
	return ok
}
