// Package verify runs the project's own verification commands and combines
// their result with scope conformance into a single verdict.
//
// It is the evidence half of a verdict. Without it, an audit can truthfully
// report "in scope" about a change that does not compile — which is the exact
// shape of false confidence this tool exists to refuse.
//
// Two rules govern the verdict:
//
//   - Nothing is VERIFIED unless something actually ran. "Not checked" never
//     becomes "fine".
//   - Scope outranks evidence. A change that passes every test but touched
//     files nobody declared is still out of scope; passing tests do not
//     retroactively authorize the work.
package verify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/3zequiel3/vector/internal/audit"
	"github.com/3zequiel3/vector/internal/detect"
	"github.com/3zequiel3/vector/internal/gitx"
	"github.com/3zequiel3/vector/internal/scope"
)

// Verdict is the full answer: where the change went, and whether it works.
type Verdict string

const (
	// Verified means the change stayed in scope and every declared command passed.
	Verified Verdict = "VERIFIED"
	// PartiallyVerified means everything that ran passed, but the project
	// declares no way to check something that matters — typically no tests.
	PartiallyVerified Verdict = "PARTIALLY_VERIFIED"
	// Failed means a verification command returned non-zero.
	Failed Verdict = "FAILED"
	// OutOfScope means the change left its declared boundary. Reported even
	// when everything passes.
	OutOfScope Verdict = "OUT_OF_SCOPE"
	// Unverified means nothing ran. It is never a synonym for "fine".
	Unverified Verdict = "UNVERIFIED"
)

// Exit codes. 0 only when the verdict is genuinely good.
const (
	ExitOK       = 0
	ExitProblem  = 1
	ExitInternal = 2
)

// Check is one command that ran, or was asked for and could not.
type Check struct {
	Name     string `json:"name"`
	Command  string `json:"command"`
	Passed   bool   `json:"passed"`
	ExitCode int    `json:"exit_code"`
	Duration string `json:"duration"`
	Output   string `json:"output,omitempty"`
	Skipped  string `json:"skipped,omitempty"`
}

// Report is the machine-readable verdict.
type Report struct {
	Schema  string       `json:"schema"`
	Verdict Verdict      `json:"verdict"`
	Reason  string       `json:"reason"`
	Scope   audit.Report `json:"scope"`
	Checks  []Check      `json:"checks"`
}

// ExitCode maps a verdict to a process exit code.
func (r Report) ExitCode() int {
	switch r.Verdict {
	case Verified, PartiallyVerified:
		return ExitOK
	default:
		return ExitProblem
	}
}

// Options controls a run.
type Options struct {
	Dir     string
	TaskID  string
	Only    []string      // run just these checks, by name
	Timeout time.Duration // per command
}

// order runs the cheap checks first. A type error surfaces in seconds and
// usually explains the test failures that would follow, so paying for the slow
// signal before the fast one wastes time on a result you can already predict.
var order = []string{"typecheck", "lint", "test", "build"}

// Run performs the verification and returns the combined verdict.
func Run(opts Options) (Report, error) {
	root, err := gitx.Root(opts.Dir)
	if err != nil {
		return Report{}, err
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Minute
	}

	scopeRep, err := audit.Run(audit.Options{Dir: root, TaskID: opts.TaskID})
	if err != nil {
		return Report{}, err
	}

	// Commands come from the project's own manifests, never from a guess.
	stack := detect.Detect(root)
	cmds := detect.DetectCommands(root, stack.PM)
	byName := map[string]string{
		"test": cmds.Test, "typecheck": cmds.Typecheck,
		"build": cmds.Build, "lint": cmds.Lint,
	}

	rep := Report{Schema: "vector.verify/v1", Scope: scopeRep, Checks: []Check{}}
	for _, name := range order {
		if len(opts.Only) > 0 && !contains(opts.Only, name) {
			continue
		}
		cmd := byName[name]
		if cmd == "" {
			rep.Checks = append(rep.Checks, Check{
				Name: name, Skipped: "not declared by the project",
			})
			continue
		}
		rep.Checks = append(rep.Checks, run(root, name, cmd, opts.Timeout))
	}

	rep.Verdict, rep.Reason = decide(scopeRep, rep.Checks)
	return rep, nil
}

// decide combines scope conformance and evidence. The ordering of these cases
// is the policy: scope first, then failure, then absence of evidence.
func decide(s audit.Report, checks []Check) (Verdict, string) {
	switch s.Status {
	case audit.OutOfScope, audit.Forbidden:
		return OutOfScope, fmt.Sprintf(
			"the change left its declared boundary (%s); passing checks do not authorize that",
			strings.ToLower(string(s.Status)))
	}

	var ran, failed []string
	for _, c := range checks {
		if c.Skipped != "" {
			continue
		}
		ran = append(ran, c.Name)
		if !c.Passed {
			failed = append(failed, c.Name)
		}
	}

	if len(failed) > 0 {
		return Failed, "failed: " + strings.Join(failed, ", ")
	}
	if len(ran) == 0 {
		return Unverified, "nothing ran; the project declares no verification commands"
	}
	// A project with no test command has not been shown to work, however green
	// the rest is. Saying VERIFIED here would be the claim this tool refuses.
	if !contains(ran, "test") {
		return PartiallyVerified, "passed: " + strings.Join(ran, ", ") + " — but no test command is declared"
	}
	if s.Status == audit.NoScopeDeclared {
		return PartiallyVerified, "passed: " + strings.Join(ran, ", ") + " — but no scope was declared, so conformance was not checked"
	}
	return Verified, "in scope; passed: " + strings.Join(ran, ", ")
}

// run executes one command under a timeout. A hung suite must not hang vector.
func run(root, name, command string, timeout time.Duration) Check {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	start := time.Now()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	c := Check{
		Name:     name,
		Command:  command,
		Duration: time.Since(start).Round(time.Millisecond).String(),
		Passed:   err == nil,
	}
	if ctx.Err() == context.DeadlineExceeded {
		c.Passed = false
		c.ExitCode = -1
		c.Output = fmt.Sprintf("timed out after %s", timeout)
		return c
	}
	if ee, ok := err.(*exec.ExitError); ok {
		c.ExitCode = ee.ExitCode()
	}
	if !c.Passed {
		c.Output = tail(string(out), 40)
	}
	return c
}

// tail keeps the end of a command's output, where the failure usually is, and
// caps it so a verbose suite cannot flood a report or a context window.
func tail(s string, lines int) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	parts := strings.Split(s, "\n")
	if len(parts) <= lines {
		return s
	}
	return "…\n" + strings.Join(parts[len(parts)-lines:], "\n")
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
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

// WriteText emits the report for a person.
func (r Report) WriteText(w io.Writer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s — %s\n\n", r.Verdict, r.Reason)

	for _, c := range r.Checks {
		switch {
		case c.Skipped != "":
			fmt.Fprintf(&b, "  ---- %-10s %s\n", c.Name, c.Skipped)
		case c.Passed:
			fmt.Fprintf(&b, "  ok   %-10s %s  (%s)\n", c.Name, c.Command, c.Duration)
		default:
			fmt.Fprintf(&b, "  FAIL %-10s %s  (exit %d, %s)\n", c.Name, c.Command, c.ExitCode, c.Duration)
		}
	}
	if out := failureOutput(r.Checks); out != "" {
		fmt.Fprintf(&b, "\n%s\n", indent(out))
	}

	if len(r.Scope.Findings) > 0 {
		b.WriteString("\nscope\n")
		for _, f := range r.Scope.Findings {
			fmt.Fprintf(&b, "  %-14s %s\n", f.Kind, f.Path)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func failureOutput(checks []Check) string {
	for _, c := range checks {
		if !c.Passed && c.Output != "" {
			return c.Output
		}
	}
	return ""
}

func indent(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "    " + l
	}
	return strings.Join(lines, "\n")
}

// unused keeps the scope import honest if the verdict logic is trimmed later.
var _ = scope.Allowed
