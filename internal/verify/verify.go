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

	"github.com/3zequiel3/vector/internal/attempt"
	"github.com/3zequiel3/vector/internal/audit"
	"github.com/3zequiel3/vector/internal/detect"
	"github.com/3zequiel3/vector/internal/freshness"
	"github.com/3zequiel3/vector/internal/gitx"
	"github.com/3zequiel3/vector/internal/scope"
	"github.com/3zequiel3/vector/internal/suite"
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
	// Retry is set only when this task has been failing repeatedly. Whoever
	// ran verify by hand is exactly the person who should hear that, and they
	// were only being told through the Stop hook.
	Retry string `json:"retry,omitempty"`
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
	Dir    string
	TaskID string
	// Base is the ref the change is measured against. Empty means HEAD,
	// which answers "what has this session done" and is right at a terminal.
	//
	// It is wrong in CI, and silently so: a fresh checkout has a working tree
	// identical to HEAD, so every diff is empty and a branch that gutted its
	// tests three commits ago looks like no change at all. CI wants the ref it
	// branched from.
	Base    string
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

	scopeRep, err := audit.Run(audit.Options{Dir: root, TaskID: opts.TaskID, Base: opts.Base})
	if err != nil {
		return Report{}, err
	}

	// The tree is hashed here, before a single command runs. A verdict is a
	// claim about the code the commands were handed, not about whatever the
	// tree became while a five-minute suite was running; hashing afterwards
	// would quietly extend the claim to cover edits nothing ever checked.
	verified := freshness.Hash(root, scopeRep.InScope)

	// Commands come from the project's own manifests, never from a guess —
	// and, where the project said detection got it wrong, from policy.toml.
	//
	// A policy that will not load is not a reason to refuse to verify: the
	// zero Policy overrides nothing, so verification falls back to pure
	// detection rather than failing on a file that is not required to exist.
	stack := detect.Detect(root)
	pol, _ := scope.LoadPolicy(root)
	cmds := pol.MergeCommands(detect.DetectCommands(root, stack.PM))
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

	// Whether this change subtracted from its own tests, measured against the
	// same base the audit used — the two halves of a verdict must be about the
	// same change, or the sentence they combine into is not about anything.
	//
	// A git failure here is not a reason to refuse a verdict: the zero Delta
	// claims nothing, and verify falls back to the answer it gave before this
	// existed.
	numstat, _ := gitx.Numstat(root, opts.Base)
	delta, weakened := suite.Weakened(numstat)

	rep.Verdict, rep.Reason = decide(scopeRep, rep.Checks, delta, weakened)
	rep.Retry = record(root, scopeRep, rep)
	remember(root, scopeRep, rep, verified)
	return rep, nil
}

// remember files the tree this verdict was about, so a later turn can tell a
// verdict that still describes the working tree from one that has been edited
// out from under it.
//
// verify reports nothing about staleness of its own. It has just re-verified:
// the snapshot it writes is the tree in front of it by construction, and the
// only stale verdict it could name is the one it is replacing in the same
// breath. Saying "your previous answer is out of date" while handing over the
// new one is noise, and it would put a line in front of the reader that is
// already false by the time they read it. The Stop hook is where the question
// has an answer worth hearing, because that is where nobody re-verified.
//
// A run with no task records nothing, exactly as the attempt log does: a
// snapshot belongs to the verdict it was taken for, and there is no key to
// file it under. Every failure to write is swallowed — a lost snapshot costs a
// reminder nobody gets, and the verdict is what verify owes its caller.
func remember(root string, scopeRep audit.Report, rep Report, files map[string]string) {
	if scopeRep.TaskID == "" {
		return
	}
	_ = freshness.Record(root, freshness.Snapshot{
		At:      time.Now(),
		Task:    scopeRep.TaskID,
		Verdict: string(rep.Verdict),
		Files:   files,
		// Exactly what the exit code says, so the snapshot and the process can
		// never disagree about whether this run went well.
		Passed: rep.ExitCode() == ExitOK,
	})
}

// record files this run against the task, so a later turn can tell a task that
// is converging from one that is retrying in circles.
//
// It runs after the verdict and cannot change it. A run that is not attached
// to a task records nothing — there would be nothing to count it against — and
// every failure to write is swallowed: the verdict is the answer verify owes
// its caller, and losing a tally must never cost them that.
//
// "Passed" here is exactly what the exit code says, so the log and the process
// can never disagree about whether a run went well.
// It returns the retry signal for this task, judged after recording so the
// count includes the run being reported.
func record(root string, scopeRep audit.Report, rep Report) string {
	if scopeRep.TaskID == "" {
		return ""
	}
	// The same base audit just compared against, so the size recorded beside a
	// verdict is the size that verdict was about.
	files, lines := attempt.Volume(root, scopeRep.Base)
	_ = attempt.Record(root, attempt.Outcome{
		At:      time.Now(),
		Task:    scopeRep.TaskID,
		Verdict: string(rep.Verdict),
		Files:   files,
		Lines:   lines,
		Passed:  rep.ExitCode() == ExitOK,
	})
	return attempt.Judge(root, scopeRep.TaskID).Message()
}

// decide combines scope conformance and evidence. The ordering of these cases
// is the policy: scope first, then failure, then absence of evidence.
func decide(s audit.Report, checks []Check, delta suite.Delta, weakened bool) (Verdict, string) {
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
	// The twin of the rule above, and it is checked here because it only means
	// anything once the test command has actually run: a passing suite that
	// this change shrank is not evidence about this change.
	//
	// It sits before the no-scope case because it is the more specific finding
	// and the more alarming one — but both can be true, and dropping either
	// would hide a finding behind an unrelated one. The reader gets both.
	if weakened {
		reason := "passed: " + strings.Join(ran, ", ") + " — " + delta.Reason()
		if s.Status == audit.NoScopeDeclared {
			reason += "; and no scope was declared, so conformance was not checked either"
		}
		return PartiallyVerified, reason
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

	// The retry signal goes last: it is context about the shape of the work,
	// not a result of this run, and putting it above the findings would push
	// the thing the reader asked for down the page.
	if r.Retry != "" {
		fmt.Fprintf(&b, "\n%s\n", r.Retry)
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
