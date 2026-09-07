// Package audit answers one question: did the change stay inside the boundary
// that was declared for it?
//
// The answer is a set difference over normalized paths. It does not depend on
// any hook having fired, on any agent cooperating, or on any model being
// honest. That makes it the least clever and most reliable part of Vector.
package audit

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/3zequiel3/vector/internal/gitx"
	"github.com/3zequiel3/vector/internal/scope"
)

// Status is the scope-conformance half of a task verdict. The evidence half
// (tests, typecheck, build) is deliberately not claimed here.
type Status string

const (
	// InScope means every changed path matched a declared write pattern.
	InScope Status = "IN_SCOPE"
	// OutOfScope means the change touched paths the task never claimed.
	OutOfScope Status = "OUT_OF_SCOPE"
	// Forbidden means the change touched an explicitly denied path. This is
	// reported separately because writing to enforcement config or secrets is
	// a different kind of event from ordinary drift.
	Forbidden Status = "FORBIDDEN"
	// NoScopeDeclared means no task boundary existed, so only the
	// always-forbidden rules were checked. It is not a pass.
	NoScopeDeclared Status = "NO_SCOPE_DECLARED"
	// NoChanges means the working tree is clean.
	NoChanges Status = "NO_CHANGES"
)

// Exit codes. Callers must branch on Status or on these codes, never on the
// mere presence of output.
const (
	ExitOK        = 0
	ExitViolation = 1
)

// Finding is one path that did not belong.
type Finding struct {
	Path    string `json:"path"`
	Kind    string `json:"kind"`
	Pattern string `json:"pattern,omitempty"`
}

// Report is the full machine-readable result of an audit.
type Report struct {
	Schema      string    `json:"schema"`
	Status      Status    `json:"status"`
	TaskID      string    `json:"task_id,omitempty"`
	Objective   string    `json:"objective,omitempty"`
	Enforcement string    `json:"enforcement"`
	Base        string    `json:"base,omitempty"`
	Expansions  int       `json:"expansions"`
	Changed     []string  `json:"changed_files"`
	InScope     []string  `json:"in_scope"`
	Findings    []Finding `json:"findings"`
}

func (r Report) count(kind string) int {
	n := 0
	for _, f := range r.Findings {
		if f.Kind == kind {
			n++
		}
	}
	return n
}

// ExitCode maps a report to a process exit code.
func (r Report) ExitCode() int {
	switch r.Status {
	case OutOfScope, Forbidden:
		return ExitViolation
	default:
		return ExitOK
	}
}

// Options controls a single audit run.
type Options struct {
	Dir    string
	TaskID string
	Base   string
}

// Run performs the audit against the repository containing opts.Dir.
func Run(opts Options) (Report, error) {
	root, err := gitx.Root(opts.Dir)
	if err != nil {
		return Report{}, err
	}

	policy, err := scope.LoadPolicy(root)
	if err != nil {
		return Report{}, err
	}
	// An empty TaskID means "whatever task is active", so that no everyday
	// command has to carry a -task flag.
	taskID := opts.TaskID
	if taskID == "" {
		taskID = scope.Current(root)
	}
	sc, err := scope.LoadScope(root, taskID)
	if err != nil {
		return Report{}, err
	}
	rules := scope.BuildRuleset(policy, sc)

	changed, err := gitx.ChangedFiles(root, opts.Base)
	if err != nil {
		return Report{}, err
	}

	rep := Report{
		Schema:      "vector.audit/v1",
		TaskID:      rules.TaskID,
		Objective:   rules.Objective,
		Enforcement: policy.Mode.Enforcement,
		Base:        opts.Base,
		Expansions:  rules.Expansions,
		Changed:     changed,
		InScope:     []string{},
		Findings:    []Finding{},
	}

	var sawForbidden, sawOutOfScope bool
	for _, f := range changed {
		rel, err := scope.Normalize(root, f)
		if err != nil {
			// A path we cannot place is a path we cannot clear.
			rep.Findings = append(rep.Findings, Finding{Path: f, Kind: "unresolvable"})
			sawOutOfScope = true
			continue
		}
		switch d, pat := rules.Decide(rel); d {
		case scope.Forbidden:
			rep.Findings = append(rep.Findings, Finding{Path: rel, Kind: "forbidden", Pattern: pat})
			sawForbidden = true
		case scope.OutOfScope:
			rep.Findings = append(rep.Findings, Finding{Path: rel, Kind: "out_of_scope"})
			sawOutOfScope = true
		default:
			rep.InScope = append(rep.InScope, rel)
		}
	}

	switch {
	case sawForbidden:
		rep.Status = Forbidden
	case sawOutOfScope:
		rep.Status = OutOfScope
	case len(changed) == 0:
		rep.Status = NoChanges
	case !rules.Declared:
		rep.Status = NoScopeDeclared
	default:
		rep.Status = InScope
	}
	return rep, nil
}

// WriteJSON emits the report for machine consumers.
func (r Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteText emits the report for a person reading a terminal. It says what
// happened and names the rule that decided it; it never says "safe".
func (r Report) WriteText(w io.Writer) error {
	var b strings.Builder

	switch r.Status {
	case NoChanges:
		b.WriteString("no changes in the working tree\n")
	case NoScopeDeclared:
		fmt.Fprintf(&b, "NO SCOPE DECLARED — %d file(s) changed\n", len(r.Changed))
		b.WriteString("  only forbidden paths were checked; scope conformance was not evaluated\n")
	case InScope:
		fmt.Fprintf(&b, "IN SCOPE — %d file(s), all within what was declared\n", len(r.InScope))
	case OutOfScope:
		fmt.Fprintf(&b, "OUT OF SCOPE — %d of %d file(s) were not declared\n",
			r.count("out_of_scope"), len(r.Changed))
	case Forbidden:
		// Forbidden outranks out-of-scope, but it must not hide it: a run that
		// touched both needs to report both, or the milder finding is lost.
		fmt.Fprintf(&b, "FORBIDDEN — %d denied path(s)", r.count("forbidden"))
		if n := r.count("out_of_scope"); n > 0 {
			fmt.Fprintf(&b, " and %d out of scope", n)
		}
		fmt.Fprintf(&b, ", across %d changed file(s)\n", len(r.Changed))
	}

	if r.Objective != "" {
		fmt.Fprintf(&b, "  objective: %s\n", r.Objective)
	}
	// A boundary that was widened is still a boundary, but the reader deserves
	// to know it moved.
	if r.Expansions > 0 {
		times := "time"
		if r.Expansions > 1 {
			times = "times"
		}
		fmt.Fprintf(&b, "  scope widened %d %s (see .vector/scope/%s.toml)\n",
			r.Expansions, times, r.TaskID)
	}
	for _, f := range r.Findings {
		if f.Pattern != "" {
			fmt.Fprintf(&b, "  %-14s %s  (regla: %s)\n", f.Kind, f.Path, f.Pattern)
		} else {
			fmt.Fprintf(&b, "  %-14s %s\n", f.Kind, f.Path)
		}
	}
	if r.Status == Forbidden || r.Status == OutOfScope {
		if r.Enforcement == "advisory" {
			b.WriteString("\n  advisory mode: reported, not blocked\n")
		}
	}

	_, err := io.WriteString(w, b.String())
	return err
}
