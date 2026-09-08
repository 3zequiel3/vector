package setup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// The sandbox is the only mechanism that stops a write vector cannot see.
//
// Hooks fire on tool calls, so vector reads Edit and Write arguments and parses
// shell commands for redirections. A Python script that opens a file is
// invisible to all of that. The OS sandbox is enforced for the command and
// every child process, which is a different kind of guarantee: it does not
// depend on vector recognising the shape of the write.
//
// What vector contributes is narrow, and worth stating plainly. Enabling the
// sandbox at all confines writes to the working directory, which closes writes
// outside the repository. Claude Code then protects its own configuration
// natively — the .claude settings files, skills, agents, commands and hooks,
// .mcp.json, .git/hooks and config — and those protections cannot be lifted by
// any allow rule, and already resolve symlinks. Vector's denyWrite adds what
// that list does not cover: secrets, vector's own configuration, and whatever
// the project forbids.
//
// Out-of-scope writes to ordinary files inside the repository stay where they
// were: the hook prevents them, and the diff audit detects them. The sandbox is
// not a per-task boundary — its configuration is read at session start, and the
// scope changes per task.

// sandboxTranslate converts a vector pattern into a sandbox path.
//
// In a project's .claude/settings.json a bare or "./" path resolves against the
// project root, which is what vector's patterns are already written against.
// Wildcards carry over unchanged.
func sandboxTranslate(pattern string) string {
	p := strings.TrimSpace(pattern)
	p = strings.TrimPrefix(p, "./")
	if p == "" || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~") {
		// Absolute and home-relative patterns are already in the sandbox's
		// vocabulary; passing them through untouched is the honest thing to do.
		return p
	}
	return "./" + p
}

// sandboxKey is the form two patterns are compared in.
//
// Both sides go through it, so a policy that writes "./.gitignore" and a
// hook_only list that writes ".gitignore" are talking about the same file.
// Comparing raw strings would make an exclusion depend on a spelling nobody
// was told to match, and it would fail silently — the path would simply reach
// the sandbox anyway.
func sandboxKey(pattern string) string {
	return strings.TrimPrefix(strings.TrimSpace(pattern), "./")
}

// SandboxForbidden returns the protections that go to Claude's OS sandbox:
// everything the policy forbids, minus what it marked hook_only.
//
// The exclusion is the policy's call, not vector's. Which layer enforces a
// protection is a trade a project has to be able to see and change — the
// defaults are in scope.DefaultPolicy, and the reasoning for them is there
// with them. The hook goes on denying every path either way; this decides only
// what the kernel is also asked to stop.
func SandboxForbidden(forbidden, hookOnly []string) []string {
	excluded := make(map[string]bool, len(hookOnly))
	for _, pattern := range hookOnly {
		if key := sandboxKey(pattern); key != "" {
			excluded[key] = true
		}
	}
	out := make([]string, 0, len(forbidden))
	for _, pattern := range forbidden {
		if excluded[sandboxKey(pattern)] {
			continue
		}
		out = append(out, pattern)
	}
	return out
}

// InstallClaudeSandbox turns the sandbox on and denies writes to the paths the
// policy forbids.
//
// Filesystem arrays merge across settings scopes rather than replacing one
// another, so a project-level denyWrite can only ever add. It cannot weaken a
// stricter rule set elsewhere, which is why writing it here is safe.
func InstallClaudeSandbox(root string, forbidden []string) (HookResult, error) {
	path := filepath.Join(root, ".claude", "settings.json")
	res := HookResult{Path: rel(root, path)}

	settings := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			return res, fmt.Errorf("%s: %w (fix it, or run init --no-sandbox)", res.Path, err)
		}
	} else if !os.IsNotExist(err) {
		return res, err
	}

	sandbox, _ := settings["sandbox"].(map[string]any)
	if sandbox == nil {
		sandbox = map[string]any{}
	}
	if enabled, _ := sandbox["enabled"].(bool); !enabled {
		sandbox["enabled"] = true
		res.Added = append(res.Added, "enabled")
	} else {
		res.Present = append(res.Present, "enabled")
	}

	fs, _ := sandbox["filesystem"].(map[string]any)
	if fs == nil {
		fs = map[string]any{}
	}
	deny, _ := fs["denyWrite"].([]any)

	existing := map[string]bool{}
	for _, e := range deny {
		if s, ok := e.(string); ok {
			existing[s] = true
		}
	}
	for _, f := range forbidden {
		t := sandboxTranslate(f)
		if t == "" || existing[t] {
			continue
		}
		deny = append(deny, t)
		existing[t] = true
		res.Added = append(res.Added, t)
	}

	fs["denyWrite"] = deny
	sandbox["filesystem"] = fs
	settings["sandbox"] = sandbox

	if len(res.Added) == 0 {
		return res, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return res, err
	}
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return res, err
	}
	return res, writeAtomic(path, string(out)+"\n")
}

// RemoveClaudeSandbox withdraws vector's denyWrite entries.
//
// It deliberately leaves sandbox.enabled alone. Someone may want the sandbox
// for reasons that have nothing to do with vector, and turning off a security
// boundary on the way out is not a decision an uninstaller should make.
func RemoveClaudeSandbox(root string, forbidden []string) (HookResult, error) {
	path := filepath.Join(root, ".claude", "settings.json")
	res := HookResult{Path: rel(root, path)}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return res, nil
	}
	if err != nil {
		return res, err
	}
	settings := map[string]any{}
	if err := json.Unmarshal(data, &settings); err != nil {
		return res, fmt.Errorf("%s: %w", res.Path, err)
	}
	sandbox, _ := settings["sandbox"].(map[string]any)
	if sandbox == nil {
		return res, nil
	}
	fs, _ := sandbox["filesystem"].(map[string]any)
	if fs == nil {
		return res, nil
	}
	deny, _ := fs["denyWrite"].([]any)

	ours := map[string]bool{}
	for _, f := range forbidden {
		ours[sandboxTranslate(f)] = true
	}
	kept := make([]any, 0, len(deny))
	for _, e := range deny {
		s, _ := e.(string)
		if ours[s] {
			res.Added = append(res.Added, s)
			continue
		}
		kept = append(kept, e)
	}
	if len(res.Added) == 0 {
		return res, nil
	}

	if len(kept) == 0 {
		delete(fs, "denyWrite")
	} else {
		fs["denyWrite"] = kept
	}
	if len(fs) == 0 {
		delete(sandbox, "filesystem")
	} else {
		sandbox["filesystem"] = fs
	}
	if len(sandbox) == 0 {
		delete(settings, "sandbox")
	} else {
		settings["sandbox"] = sandbox
	}

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return res, err
	}
	return res, writeAtomic(path, string(out)+"\n")
}

// sandboxInert reports whether this platform's sandbox discards an entry.
//
// On Linux and WSL2 the sandbox mounts concrete paths, so Claude Code removes a
// trailing "/**" and then skips any write entry still containing "*", "?" or
// "[" — that entry has no effect at all. macOS Seatbelt matches patterns and
// honours every one. Documented under "Sandbox path prefixes" in
// https://code.claude.com/docs/en/settings-reference
//
// Vector writes the same entries on every platform on purpose: the settings
// file is committed, and a colleague on macOS gets the protection. What vector
// must not do is let a Linux user believe a rule is enforced when the OS threw
// it away — a configuration that is present and dead is exactly the failure
// this tool exists to catch.
func sandboxInert(entry string) bool {
	if runtime.GOOS == "darwin" {
		return false
	}
	return strings.ContainsAny(strings.TrimSuffix(entry, "/**"), "*?[")
}

// SandboxInert reports which of the project's denyWrite entries this platform's
// sandbox discards.
//
// It reads the settings file rather than the policy, because the question is
// what the OS was actually handed — including entries vector did not write.
func SandboxInert(root string) []string {
	data, err := os.ReadFile(filepath.Join(root, ".claude", "settings.json"))
	if err != nil {
		return nil
	}
	var settings struct {
		Sandbox struct {
			Filesystem struct {
				DenyWrite []string `json:"denyWrite"`
			} `json:"filesystem"`
		} `json:"sandbox"`
	}
	if json.Unmarshal(data, &settings) != nil {
		return nil
	}
	var inert []string
	for _, e := range settings.Sandbox.Filesystem.DenyWrite {
		if sandboxInert(e) {
			inert = append(inert, e)
		}
	}
	return inert
}

// SandboxEnabled reports whether the project's settings turn the sandbox on.
// It reads only the project file, so it answers "did this repository ask for
// it", not "is it running" — a user or managed setting could enable it too.
func SandboxEnabled(root string) bool {
	data, err := os.ReadFile(filepath.Join(root, ".claude", "settings.json"))
	if err != nil {
		return false
	}
	settings := map[string]any{}
	if json.Unmarshal(data, &settings) != nil {
		return false
	}
	sandbox, _ := settings["sandbox"].(map[string]any)
	enabled, _ := sandbox["enabled"].(bool)
	return enabled
}
