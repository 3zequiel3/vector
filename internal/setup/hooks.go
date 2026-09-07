package setup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// hookEntry is one registration vector wants present in an agent's config.
type hookEntry struct {
	event   string
	matcher string
	command string
}

// wanted lists what vector registers. It is intentionally short: three events,
// one binary, no configuration for the user to get wrong.
var wanted = []hookEntry{
	{"SessionStart", "", "vector hook session-start"},
	{"PreToolUse", "Edit|Write|MultiEdit|NotebookEdit|Bash", "vector hook pre-tool"},
	{"Stop", "", "vector hook stop"},
}

// HookResult reports what an install changed.
type HookResult struct {
	Path    string
	Added   []string
	Present []string
}

// InstallClaudeHooks registers vector in the project's Claude Code settings.
//
// The file is merged, never rewritten: someone else's hooks, permissions and
// settings survive untouched, and re-running init is a no-op rather than a
// second copy of every entry. It writes the project file rather than the user's
// global one, because a boundary belongs to a repository and should travel with
// it.
func InstallClaudeHooks(root string) (HookResult, error) {
	path := filepath.Join(root, ".claude", "settings.json")
	res := HookResult{Path: rel(root, path)}

	settings := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			return res, fmt.Errorf("%s: %w (fix it, or run init --no-hooks)", res.Path, err)
		}
	} else if !os.IsNotExist(err) {
		return res, err
	}

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}

	for _, w := range wanted {
		list, _ := hooks[w.event].([]any)
		if hasCommand(list, w.command) {
			res.Present = append(res.Present, w.event)
			continue
		}
		entry := map[string]any{
			"hooks": []any{map[string]any{"type": "command", "command": w.command}},
		}
		if w.matcher != "" {
			entry["matcher"] = w.matcher
		}
		hooks[w.event] = append(list, entry)
		res.Added = append(res.Added, w.event)
	}
	settings["hooks"] = hooks

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

// hasCommand reports whether an event already runs the given command, so that
// re-running init never duplicates a registration.
func hasCommand(list []any, command string) bool {
	for _, raw := range list {
		entry, _ := raw.(map[string]any)
		inner, _ := entry["hooks"].([]any)
		for _, h := range inner {
			m, _ := h.(map[string]any)
			if cmd, _ := m["command"].(string); cmd == command {
				return true
			}
		}
	}
	return false
}

// RemoveClaudeHooks unregisters vector, leaving every other hook in place.
// A control layer that cannot be removed cleanly is one people avoid adopting.
func RemoveClaudeHooks(root string) (HookResult, error) {
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
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		return res, nil
	}

	events := make([]string, 0, len(hooks))
	for e := range hooks {
		events = append(events, e)
	}
	sort.Strings(events)

	for _, event := range events {
		list, _ := hooks[event].([]any)
		kept := make([]any, 0, len(list))
		removed := false
		for _, raw := range list {
			if isVectorEntry(raw) {
				removed = true
				continue
			}
			kept = append(kept, raw)
		}
		if !removed {
			continue
		}
		res.Added = append(res.Added, event)
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
	if len(res.Added) == 0 {
		return res, nil
	}
	if len(hooks) == 0 {
		delete(settings, "hooks")
	} else {
		settings["hooks"] = hooks
	}
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return res, err
	}
	return res, writeAtomic(path, string(out)+"\n")
}

func isVectorEntry(raw any) bool {
	entry, _ := raw.(map[string]any)
	inner, _ := entry["hooks"].([]any)
	if len(inner) != 1 {
		return false
	}
	m, _ := inner[0].(map[string]any)
	cmd, _ := m["command"].(string)
	for _, w := range wanted {
		if cmd == w.command {
			return true
		}
	}
	return false
}
