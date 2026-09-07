// Package hook answers an agent's lifecycle events.
//
// It is the only part of vector that runs on the hot path — once per tool call —
// so it does the least work that produces a correct answer, and it never
// reaches the network or a model.
//
// One rule governs everything here: **vector never returns "allow"**. Allowing
// a tool call would auto-approve it and quietly weaken the permission rules the
// user configured. vector either denies, or says nothing and lets the agent's
// own permission system decide.
package hook

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/3zequiel3/vector/internal/audit"
	"github.com/3zequiel3/vector/internal/detect"
	"github.com/3zequiel3/vector/internal/gitx"
	"github.com/3zequiel3/vector/internal/scope"
)

// Event is the subset of an agent's hook payload that vector reads. The field
// names follow Claude Code, and other agents are mapped onto them by adapters.
type Event struct {
	HookEventName string          `json:"hook_event_name"`
	CWD           string          `json:"cwd"`
	ToolName      string          `json:"tool_name"`
	ToolInput     json.RawMessage `json:"tool_input"`
}

type toolInput struct {
	FilePath     string `json:"file_path"`
	NotebookPath string `json:"notebook_path"`
	Command      string `json:"command"`
	Path         string `json:"path"`
}

// response is the agent-facing reply. Empty fields are omitted so that a
// no-opinion answer serialises to nothing meaningful.
type response struct {
	Hook *hookOutput `json:"hookSpecificOutput,omitempty"`
}

type hookOutput struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
	AdditionalContext        string `json:"additionalContext,omitempty"`
}

// Run dispatches one event and writes the reply. The error return is for
// genuine failures; a hook that cannot decide stays silent rather than
// guessing, because a wrong denial is worse than a missed one.
func Run(event string, in io.Reader, out io.Writer, dir string) error {
	var e Event
	if err := json.NewDecoder(in).Decode(&e); err != nil && err != io.EOF {
		return fmt.Errorf("hook payload: %w", err)
	}
	if e.CWD != "" {
		dir = e.CWD
	}
	root, err := gitx.Root(dir)
	if err != nil {
		// Outside a repository vector has nothing to say.
		return nil
	}

	var resp response
	switch event {
	case "pre-tool":
		resp.Hook = preTool(root, e)
	case "session-start":
		resp.Hook = sessionStart(root)
	case "stop":
		resp.Hook = stop(root)
	default:
		return fmt.Errorf("unknown hook event %q", event)
	}
	if resp.Hook == nil {
		return nil
	}
	return json.NewEncoder(out).Encode(resp)
}

// preTool decides a single write before it happens.
func preTool(root string, e Event) *hookOutput {
	paths := targets(e)
	if len(paths) == 0 {
		return nil
	}

	pol, err := scope.LoadPolicy(root)
	if err != nil {
		return nil
	}
	sc, err := scope.LoadScope(root, scope.Current(root))
	if err != nil {
		return nil
	}
	rules := scope.BuildRuleset(pol, sc)
	strict := pol.Mode.Enforcement == "strict"

	var outOfScope []string
	for _, p := range paths {
		rel, err := scope.Normalize(root, p)
		if err != nil {
			continue // outside the repository; not vector's boundary to police
		}
		switch d, pat := rules.Decide(rel); d {
		case scope.Forbidden:
			// A forbidden path is denied in every mode. These are the rules
			// that keep enforcement from being edited away, and they are not
			// advisory in any meaningful sense.
			return &hookOutput{
				HookEventName:            "PreToolUse",
				PermissionDecision:       "deny",
				PermissionDecisionReason: fmt.Sprintf("vector: %s is forbidden by %s", rel, pat),
			}
		case scope.OutOfScope:
			outOfScope = append(outOfScope, rel)
		}
	}
	if len(outOfScope) == 0 {
		return nil
	}

	if strict {
		return &hookOutput{
			HookEventName:      "PreToolUse",
			PermissionDecision: "deny",
			PermissionDecisionReason: fmt.Sprintf(
				"vector: %s is outside the scope of %q.\n"+
					"If it is genuinely required, record why:\n"+
					"  vector scope expand %s -w \"<pattern>\" -reason blocking -evidence \"<what proves it>\"",
				strings.Join(outOfScope, ", "), rules.Objective, rules.TaskID),
		}
	}
	// Advisory mode reports without blocking, so a wrong boundary never stops
	// someone from working.
	return &hookOutput{
		HookEventName: "PreToolUse",
		AdditionalContext: fmt.Sprintf(
			"vector: %s is outside the declared scope. Continuing (advisory mode). "+
				"If this belongs to the task, run `vector scope expand %s -w \"<pattern>\" "+
				"-reason blocking -evidence \"<what proves it>\"`; if it does not, leave it "+
				"and record it with `vector observe \"<note>\"`.",
			strings.Join(outOfScope, ", "), rules.TaskID),
	}
}

// targets returns the paths a tool call would write.
func targets(e Event) []string {
	var ti toolInput
	if len(e.ToolInput) > 0 {
		_ = json.Unmarshal(e.ToolInput, &ti)
	}
	switch e.ToolName {
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		for _, p := range []string{ti.FilePath, ti.NotebookPath, ti.Path} {
			if p != "" {
				return []string{p}
			}
		}
	case "Bash", "BashOutput", "Shell":
		return WriteTargets(ti.Command)
	}
	return nil
}

// sessionStart hands the agent the small amount of project truth that keeps it
// from inventing commands, plus how to declare a boundary.
//
// This is kept deliberately short. Everything here is paid for on every session,
// and a long preamble is how a control layer becomes more expensive than the
// waste it prevents.
func sessionStart(root string) *hookOutput {
	var b strings.Builder
	b.WriteString("vector is active in this repository.\n")

	s := detect.Detect(root)
	if s.PM.Name != "" && s.PM.Name != "unknown" {
		fmt.Fprintf(&b, "Package manager: %s.", s.PM.Name)
		c := detect.DetectCommands(root, s.PM)
		var cmds []string
		for _, pair := range [][2]string{
			{"test", c.Test}, {"typecheck", c.Typecheck},
			{"build", c.Build}, {"lint", c.Lint},
		} {
			if pair[1] != "" {
				cmds = append(cmds, pair[0]+": "+pair[1])
			}
		}
		if len(cmds) > 0 {
			fmt.Fprintf(&b, " Verification — %s.", strings.Join(cmds, "; "))
		}
		b.WriteString("\n")
	}

	if cur := scope.Current(root); cur != "" {
		fmt.Fprintf(&b, "Active scope: %s. Writes outside it are reported.\n", cur)
	} else {
		b.WriteString(
			"No scope is declared. Once you know which files this task needs — " +
				"after exploring, before your first edit — declare it:\n" +
				"  vector scope new <short-id> -o \"<objective>\" -w \"<path pattern>\"\n" +
				"Then work normally. If you notice an unrelated problem, do not fix it: " +
				"record it with `vector observe \"<note>\"` and continue.\n")
	}
	return &hookOutput{HookEventName: "SessionStart", AdditionalContext: b.String()}
}

// stop reports scope conformance as the turn ends, so the result is seen
// without anyone remembering to ask for it.
func stop(root string) *hookOutput {
	rep, err := audit.Run(audit.Options{Dir: root})
	if err != nil {
		return nil
	}
	switch rep.Status {
	case audit.InScope, audit.NoChanges, audit.NoScopeDeclared:
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "vector: %s.", rep.Status)
	for _, f := range rep.Findings {
		fmt.Fprintf(&b, " %s (%s);", f.Path, f.Kind)
	}
	b.WriteString(" Run `vector audit` for the full report.")
	return &hookOutput{HookEventName: "Stop", AdditionalContext: b.String()}
}
