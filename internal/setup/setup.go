// Package setup writes a repository's Vector configuration.
//
// Regeneration is merge-safe: the detected sections are refreshed from local
// evidence, and the sections a human owns are carried over untouched. A tool
// that silently discards someone's decisions on every run is a tool they stop
// running.
package setup

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/3zequiel3/vector/internal/detect"
	"github.com/3zequiel3/vector/internal/gitx"
	"github.com/3zequiel3/vector/internal/scope"
)

// Result describes what an Init run established and wrote.
type Result struct {
	PolicyPath string
	Existed    bool
	Stack      detect.Stack
	Commands   detect.Commands
	Policy     scope.Policy
	// NewForbidden are default deny paths this run added to a policy that
	// predated them. Reported so a protection never arrives silently.
	NewForbidden []string
}

// Init detects the stack and writes .vector/policy.toml under root.
//
// When a policy already exists, [scope] and [mode] are preserved verbatim and
// only the detected sections are refreshed.
func Init(root string) (Result, error) {
	return InitWith(root, nil)
}

// InitWith is Init with an optional change to a human-owned setting.
//
// Flipping the sandbox has to go through init rather than a separate write,
// because init is what renders the policy file: a standalone writer would be
// overwritten by the next regeneration, and a value that quietly reverts is
// worse than one that was never set.
func InitWith(root string, sandbox *bool) (Result, error) {
	policyPath := filepath.Join(root, ".vector", "policy.toml")
	_, statErr := os.Stat(policyPath)
	existed := statErr == nil

	// LoadPolicy falls back to defaults when the file is absent, so this one
	// call covers both the first run and every refresh after it.
	pol, err := scope.LoadPolicy(root)
	if err != nil {
		return Result{}, err
	}
	if sandbox != nil {
		pol.Mode.Sandbox = *sandbox
	}
	added := adoptNewForbidden(&pol)

	stack := detect.Detect(root)
	cmds := detect.DetectCommands(root, stack.PM)

	if err := os.MkdirAll(filepath.Join(root, ".vector", "scope"), 0o755); err != nil {
		return Result{}, err
	}
	if err := ensureIgnored(root, localState); err != nil {
		return Result{}, err
	}
	if err := writeAtomic(policyPath, render(stack, cmds, pol)); err != nil {
		return Result{}, err
	}

	return Result{
		PolicyPath:   policyPath,
		Existed:      existed,
		Stack:        stack,
		Commands:     cmds,
		Policy:       pol,
		NewForbidden: added,
	}, nil
}

// adoptNewForbidden adds any default forbidden path the policy does not
// already list, and reports which.
//
// [scope] is preserved verbatim across regeneration, which is right: a tool
// that discards someone's decisions on every run is one they stop running. But
// preserved verbatim also meant frozen. A repository initialised before a
// protection existed never received it — every self-protection rule added
// after the day someone ran `vector init` reached new repositories only, and
// the ones already running vector kept a list that was correct in the past.
// The gap is invisible: the policy parses, doctor is happy, and the path is
// simply not denied.
//
// So the defaults are additive, exactly as .vector/.gitignore already is: init
// adds what is missing and rewrites nothing. A path someone deliberately
// removed comes back, which is the safe direction for a list whose whole
// purpose is that an agent cannot weaken its own constraints — and init says
// out loud what it added, so removing it again is a decision rather than a
// surprise.
//
// high_risk is deliberately not treated this way. It documents `= []` as a
// real opt-out for a project whose layout makes the defaults noisy, and
// re-adding them would take that away.
//
// hook_only is not either, and there the reason is stronger: growing it means
// withdrawing a path from the OS sandbox. Additive defaults are safe on a deny
// list because the worst case is more protection than was asked for; on this
// one the worst case is less, arriving silently, on a run someone started for
// an unrelated reason. A project that shortens this list has chosen a stricter
// sandbox, and init has no business undoing that.
func adoptNewForbidden(p *scope.Policy) []string {
	have := map[string]bool{}
	for _, f := range p.Scope.AlwaysForbidden {
		have[f] = true
	}
	var added []string
	for _, f := range scope.DefaultPolicy().Scope.AlwaysForbidden {
		if !have[f] {
			p.Scope.AlwaysForbidden = append(p.Scope.AlwaysForbidden, f)
			added = append(added, f)
		}
	}
	return added
}

// localState names the files under .vector/ that are per-developer working
// state rather than repository artifacts.
//
// The policy, the scopes and the observations belong to the repository and
// travel with it. These do not: committing the active-task pointer would make
// every teammate's checkout fight over whose task is current, and both would
// surface in every audit as unexplained changes.
var localState = []string{"current", "nudged", "attempts", "verdicts"}

// ensureIgnored adds any missing entries to .vector/.gitignore without
// disturbing what is already there.
//
// This lives in init rather than in the hook on purpose: the hook runs once per
// tool call, and a hot path that quietly edits a user-owned file is a surprise
// nobody asked for. Init is where a repository is set up, so init is where the
// setup happens — including for repositories configured before an entry existed.
func ensureIgnored(root string, entries []string) error {
	path := filepath.Join(root, ".vector", ".gitignore")
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	present := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		present[strings.TrimSpace(line)] = true
	}

	out := string(data)
	added := false
	for _, e := range entries {
		if present[e] {
			continue
		}
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		out += e + "\n"
		added = true
	}
	if !added {
		return nil
	}
	return writeAtomic(path, out)
}

func render(s detect.Stack, c detect.Commands, p scope.Policy) string {
	var b strings.Builder

	b.WriteString("# Vector — repository contract.\n")
	b.WriteString("#\n")
	b.WriteString("# [stack] and [commands] are detected: `vector init` regenerates them.\n")
	b.WriteString("# [scope] and [mode] are yours and survive every run.\n")
	b.WriteString("# One exception: always_forbidden also grows. A repository frozen at the\n")
	b.WriteString("# protections of the day it was set up is not protected by the ones added\n")
	b.WriteString("# since, so `vector init` adds what is missing and says which.\n\n")

	b.WriteString("[stack]\n")
	kv(&b, "languages", s.Languages)
	b.WriteString("\n[stack.package_manager]\n")
	kvs(&b, "name", s.PM.Name)
	kvs(&b, "source", s.PM.Source)
	kvs(&b, "lockfile", s.PM.Lockfile)
	kvs(&b, "declared", s.PM.Declared)
	kvs(&b, "installed", s.PM.Installed)

	b.WriteString("\n[stack.runtime]\n")
	kvs(&b, "declared", s.Runtime.Declared)
	kvs(&b, "installed", s.Runtime.Installed)

	if len(s.Frameworks) > 0 {
		names := make([]string, 0, len(s.Frameworks))
		for n := range s.Frameworks {
			names = append(names, n)
		}
		sort.Strings(names)
		b.WriteString("\n# declared = what the manifest asks for; installed = what runs today.\n")
		b.WriteString("# When they differ, the difference is the finding — never collapse them.\n")
		for _, n := range names {
			v := s.Frameworks[n]
			fmt.Fprintf(&b, "[stack.frameworks.%q]\n", n)
			kvs(&b, "declared", v.Declared)
			kvs(&b, "installed", v.Installed)
		}
	}

	b.WriteString("\n# Verification commands, read from the project's own manifests.\n")
	b.WriteString("# Vector invokes them; it never invents them.\n")
	b.WriteString("#\n")
	b.WriteString("# Shown commented out, as detected. Detection stays live — vector re-reads\n")
	b.WriteString("# the manifests on every run, so an upgraded project needs no edit here.\n")
	b.WriteString("# Uncomment a line only to correct what detection got wrong: an uncommented\n")
	b.WriteString("# value wins, survives `vector init`, and goes on winning until you\n")
	b.WriteString("# remove it.\n")
	b.WriteString("[commands]\n")
	kvc(&b, "test", c.Test, p.Commands.Test)
	kvc(&b, "typecheck", c.Typecheck, p.Commands.Typecheck)
	kvc(&b, "build", c.Build, p.Commands.Build)
	kvc(&b, "lint", c.Lint, p.Commands.Lint)

	b.WriteString("\n[scope]\n")
	b.WriteString("# Always denied, even under a wide scope: an agent with write access\n")
	b.WriteString("# to the configuration that constrains it can weaken that constraint.\n")
	kv(&b, "always_forbidden", p.Scope.AlwaysForbidden)
	b.WriteString("\n# Written deliberately or not at all. Never denied — a migration and a\n")
	b.WriteString("# workflow are things a change legitimately edits. They are named in every\n")
	b.WriteString("# report, and a boundary of \"**\" does not count as having declared them.\n")
	kv(&b, "high_risk", p.Scope.HighRisk)
	b.WriteString("\n# Protected by the hook, and deliberately not by the OS sandbox. The hook\n")
	b.WriteString("# reads tool arguments and parses shell commands, so it sees\n")
	b.WriteString("# `echo x > .gitignore` but not a write made inside a process, such as\n")
	b.WriteString("# python3 -c \"open('.gitignore','a').write(...)\". The sandbox catches both,\n")
	b.WriteString("# because the kernel enforces it — so a path listed here keeps hook\n")
	b.WriteString("# protection and loses that layer.\n")
	b.WriteString("#\n")
	b.WriteString("# The defaults are here because git has to be able to READ its own ignore\n")
	b.WriteString("# metadata. On some platforms the sandbox's write denial also denied reads,\n")
	b.WriteString("# and the audit is built on `git diff` plus `git ls-files --exclude-standard`:\n")
	b.WriteString("# a git that cannot read those files makes vector report a false inventory\n")
	b.WriteString("# of what changed.\n")
	b.WriteString("#\n")
	b.WriteString("# Adding a path here is a deliberate trade, not a cleanup.\n")
	kv(&b, "hook_only", p.Scope.HookOnly)
	fmt.Fprintf(&b, "\nexpansion_requires_evidence = %t\n", p.Scope.ExpansionRequiresEvidence)

	b.WriteString("\n[mode]\n")
	b.WriteString("# advisory: report and continue. strict: a violation is a hard failure.\n")
	kvs(&b, "enforcement", p.Mode.Enforcement)
	b.WriteString("# sandbox: let the OS deny the writes vector cannot see. A script that\n")
	b.WriteString("# opens a file is invisible to a tool-level hook; the sandbox is not.\n")
	b.WriteString("# Off by default: it changes how every Bash command in the session runs.\n")
	fmt.Fprintf(&b, "sandbox = %t\n", p.Mode.Sandbox)

	if len(s.Notes) > 0 {
		b.WriteString("\n# Detection notes:\n")
		for _, n := range s.Notes {
			fmt.Fprintf(&b, "#   %s\n", n)
		}
	}
	return b.String()
}

// kvc renders one verification command, keeping detection and override apart
// on the page the way the rest of this file keeps declared and installed apart.
//
// A detected value is written as a comment, never as a declaration. That is the
// whole reason [commands] can be read back at all: if init wrote it as live
// TOML it would become an override the moment policy started being consulted,
// and the project would be frozen at whatever its manifests said the day init
// ran, with nothing on screen to suggest that had happened.
//
// An override is written live, and survives regeneration. It is the one thing
// in this section a human decided, and init exists to refresh what was
// detected — not to overrule what was chosen. When detection also found
// something, both are shown: the override is the answer, and what it displaced
// is the context for deciding whether it is still wanted.
func kvc(b *strings.Builder, key, detected, override string) {
	if strings.TrimSpace(override) == "" {
		if detected == "" {
			fmt.Fprintf(b, "# %s = \"\"  (no local evidence)\n", key)
			return
		}
		fmt.Fprintf(b, "# %s = %q  (detected)\n", key, detected)
		return
	}
	if detected != "" {
		fmt.Fprintf(b, "# %s = %q  (detected, overridden below)\n", key, detected)
	}
	fmt.Fprintf(b, "%s = %s\n", key, tomlString(override))
}

func kvs(b *strings.Builder, key, val string) {
	if val == "" {
		fmt.Fprintf(b, "# %s = \"\"  (no local evidence)\n", key)
		return
	}
	fmt.Fprintf(b, "%s = %s\n", key, tomlString(val))
}

func kv(b *strings.Builder, key string, vals []string) {
	if len(vals) == 0 {
		fmt.Fprintf(b, "%s = []\n", key)
		return
	}
	fmt.Fprintf(b, "%s = [\n", key)
	for _, v := range vals {
		fmt.Fprintf(b, "  %s,\n", tomlString(v))
	}
	b.WriteString("]\n")
}

// maxInventory is how many existing files a boundary is answered with.
//
// The point is to put the neighbourhood in front of whoever is about to write
// in it, and a list nobody reads does that no better than no list. Twenty-five
// is roughly a screen: past that the answer is the count, and the honest thing
// to say is that the boundary is too broad for this to help.
const maxInventory = 25

// Inventory lists the files that already exist inside a declared boundary,
// with the total in case it was capped.
//
// This is the whole of vector's answer to an agent rebuilding something the
// repository already has. It does not detect duplication — that needs a symbol
// index, which is state vector would own and git would not give it, and which
// is the line between a control layer and a framework. What it does is remove
// the excuse: at the one moment the agent has declared where it intends to
// write and has not written yet, it is told what is already there.
//
// Be clear about what kind of intervention this is. It is advice to a model,
// and it works only if the model reads it and acts differently — the weakest
// class of control there is, and the one this project spends its README
// arguing against relying on. It is here because it costs a `git ls-files` and
// nothing else, not because it will reliably work.
func Inventory(root string, patterns []string) (files []string, total int) {
	all, err := gitx.AllFiles(root)
	if err != nil {
		return nil, 0
	}
	rules := scope.Ruleset{Write: patterns, Declared: true}
	for _, f := range all {
		// Vector's own footprint, all of it — a wider exclusion than the audit
		// makes, and deliberately so. The audit reports a change to policy.toml
		// because editing the enforcement contract is exactly the event it
		// exists to surface. This is answering a different question, and
		// "there is a policy.toml" tells an agent nothing it can use.
		if f == ".vector" || strings.HasPrefix(f, ".vector/") {
			continue
		}
		if d, _ := rules.Decide(f); d != scope.Allowed {
			continue
		}
		total++
		if len(files) < maxInventory {
			files = append(files, f)
		}
	}
	return files, total
}

// tomlString renders an agent-supplied string as a TOML basic string.
//
// Go's %q is not a TOML encoder, and the gap is not academic. It escapes BEL as
// \a and vertical tab as \v, neither of which TOML defines, so a single
// control character anywhere in an objective or a piece of evidence produced a
// scope file that would not parse. Everything downstream then read that as
// "there is no boundary" — which, until the hook learned to fall back to the
// defaults, silently switched the whole tool off.
//
// Control characters are dropped rather than escaped. They carry no meaning in
// an objective a human is going to read, TOML forbids the raw ones in a basic
// string anyway, and a description of a task is not the place to preserve a
// bell. Tab and newline survive as their defined escapes, because a
// multi-line piece of evidence is a reasonable thing to write.
func tomlString(v string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range v {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// NewScope writes a task scope file, refusing to clobber an existing one.
func NewScope(root, taskID, objective string, write []string) (string, error) {
	if taskID == "" {
		return "", fmt.Errorf("a task id is required")
	}
	if strings.ContainsAny(taskID, `/\.`) {
		return "", fmt.Errorf("task id %q cannot contain / \\ or .", taskID)
	}
	dir := filepath.Join(root, ".vector", "scope")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, taskID+".toml")
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("%s already exists", rel(root, path))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "objective = %s\n\n", tomlString(objective))
	b.WriteString("# Paths this task may write. Everything else is out of scope,\n")
	b.WriteString("# and a boundary that is never drawn cannot be crossed.\n")
	kv(&b, "write", write)

	if err := writeAtomic(path, b.String()); err != nil {
		return "", err
	}
	// Declaring a scope is also choosing it. Nobody declares a boundary they do
	// not intend to work under right now, and making them then select it would
	// be a second step for no decision.
	if err := scope.SetCurrent(root, taskID); err != nil {
		return "", err
	}
	return rel(root, path), nil
}

// ExpandScope widens a task's boundary and records why.
//
// The expansion is appended, never merged into the original write list: the
// initial declaration stays legible and the file becomes the record of how the
// boundary grew. When the policy requires evidence, an expansion without it is
// refused — which is the entire point of the mechanism. A boundary that widens
// on assertion alone is not a boundary.
func ExpandScope(root, taskID, reason, evidence string, write []string, requireEvidence bool) (string, error) {
	if len(write) == 0 {
		return "", fmt.Errorf("at least one -w is required; an empty expansion expands nothing")
	}
	if _, ok := scope.ExpansionReasons[reason]; !ok {
		return "", fmt.Errorf("invalid reason %q; use one of: %s",
			reason, strings.Join(sortedReasons(), ", "))
	}
	if requireEvidence && strings.TrimSpace(evidence) == "" {
		return "", fmt.Errorf(
			"policy requires evidence (expansion_requires_evidence = true); "+
				"pass -evidence with what proves %q applies", reason)
	}

	path := filepath.Join(root, ".vector", "scope", taskID+".toml")
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("no scope declared for %q", taskID)
	}

	var b strings.Builder
	b.WriteString("\n[[expansion]]\n")
	fmt.Fprintf(&b, "at = %q\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "reason = %s  # %s\n", tomlString(reason), scope.ExpansionReasons[reason])
	fmt.Fprintf(&b, "evidence = %s\n", tomlString(strings.TrimSpace(evidence)))
	kv(&b, "write", write)

	// Read, append in memory, write atomically.
	//
	// This appended straight to the open file, which is the one writer in this
	// package that did not. A crash, a full disk or a killed process partway
	// through left a truncated [[expansion]] block — an unparseable scope, and
	// therefore a task whose boundary silently stopped being enforced. The
	// cost of doing it properly is reading a file that is never large.
	old, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if err := writeAtomic(path, string(old)+b.String()); err != nil {
		return "", err
	}
	return rel(root, path), nil
}

func sortedReasons() []string {
	out := make([]string, 0, len(scope.ExpansionReasons))
	for r := range scope.ExpansionReasons {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// ListScopes returns the task ids that have a declared scope.
func ListScopes(root string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, ".vector", "scope"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".toml") {
			ids = append(ids, strings.TrimSuffix(e.Name(), ".toml"))
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// writeAtomic writes via a temporary file and a rename, so a crash mid-write
// cannot leave a half-parsed policy behind.
func writeAtomic(path, content string) error {
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

func rel(base, target string) string {
	r, err := filepath.Rel(base, target)
	if err != nil {
		return target
	}
	return filepath.ToSlash(r)
}
