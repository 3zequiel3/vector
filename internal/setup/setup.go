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
	"github.com/3zequiel3/vector/internal/scope"
)

// Result describes what an Init run established and wrote.
type Result struct {
	PolicyPath string
	Existed    bool
	Stack      detect.Stack
	Commands   detect.Commands
	Policy     scope.Policy
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
		PolicyPath: policyPath,
		Existed:    existed,
		Stack:      stack,
		Commands:   cmds,
		Policy:     pol,
	}, nil
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
	b.WriteString("# [scope] and [mode] are yours: preserved verbatim across runs.\n\n")

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
	b.WriteString("# Vector invokes them; it never invents them. Empty = not found.\n")
	b.WriteString("[commands]\n")
	kvs(&b, "test", c.Test)
	kvs(&b, "typecheck", c.Typecheck)
	kvs(&b, "build", c.Build)
	kvs(&b, "lint", c.Lint)

	b.WriteString("\n[scope]\n")
	b.WriteString("# Always denied, even under a wide scope: an agent with write access\n")
	b.WriteString("# to the configuration that constrains it can weaken that constraint.\n")
	kv(&b, "always_forbidden", p.Scope.AlwaysForbidden)
	fmt.Fprintf(&b, "expansion_requires_evidence = %t\n", p.Scope.ExpansionRequiresEvidence)

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

func kvs(b *strings.Builder, key, val string) {
	if val == "" {
		fmt.Fprintf(b, "# %s = \"\"  (no local evidence)\n", key)
		return
	}
	fmt.Fprintf(b, "%s = %q\n", key, val)
}

func kv(b *strings.Builder, key string, vals []string) {
	if len(vals) == 0 {
		fmt.Fprintf(b, "%s = []\n", key)
		return
	}
	fmt.Fprintf(b, "%s = [\n", key)
	for _, v := range vals {
		fmt.Fprintf(b, "  %q,\n", v)
	}
	b.WriteString("]\n")
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
	fmt.Fprintf(&b, "objective = %q\n\n", objective)
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
	fmt.Fprintf(&b, "reason = %q  # %s\n", reason, scope.ExpansionReasons[reason])
	fmt.Fprintf(&b, "evidence = %q\n", strings.TrimSpace(evidence))
	kv(&b, "write", write)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(b.String()); err != nil {
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
