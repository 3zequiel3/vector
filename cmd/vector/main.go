// Command vector enforces and audits the declared write boundary of a task.
//
// Vector does not run the agent, hold context, route skills, or remember
// anything. It compiles a declared scope into a decision, and reports what
// actually changed.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/3zequiel3/vector/internal/audit"
	"github.com/3zequiel3/vector/internal/detect"
	"github.com/3zequiel3/vector/internal/doctor"
	"github.com/3zequiel3/vector/internal/gitx"
	"github.com/3zequiel3/vector/internal/hook"
	"github.com/3zequiel3/vector/internal/observe"
	"github.com/3zequiel3/vector/internal/scope"
	"github.com/3zequiel3/vector/internal/setup"
	"github.com/3zequiel3/vector/internal/verify"
)

const usage = `vector — scope boundary for AI coding agents

Usage:
  vector init                       detect the stack, write .vector/policy.toml
  vector scope new <id> -w <pat>    declare a task's boundary
  vector scope expand <id> -w <pat> widen it, with a reason and evidence
  vector scope list                 list declared scopes
  vector observe "<note>"           record something noticed, without acting on it
  vector observe list               list recorded observations
  vector audit [-task <id>]         compare the working tree against the boundary
  vector verify                     run the project's checks and give a verdict
  vector doctor                     check whether vector is actually doing anything
  vector uninstall                  unregister vector's hooks, leaving others intact

init flags:
  -no-hooks      do not register hooks in the detected agent
  -sandbox       also let the OS deny forbidden writes (persisted in policy.toml)
  -no-sandbox    turn that off again

verify flags:
  -only <name>   run just this check (repeatable): typecheck, lint, test, build
  -task <id>     task whose scope to enforce (defaults to the active one)
  -base <ref>    compare against this ref instead of HEAD — use it in CI, where
                 the working tree is HEAD and every diff would otherwise be empty
  -timeout <d>   per-command timeout (default 10m)
  -json          emit vector.verify/v1 JSON

audit flags:
  -task <id>     task whose scope to enforce (.vector/scope/<id>.toml)
  -base <ref>    compare against a git ref instead of the working tree
  -json          emit vector.audit/v1 JSON

scope new flags:
  -w <pattern>   writable path (repeatable)
  -o <text>      the task's objective

scope expand flags:
  -w <pattern>   path to add (repeatable)
  -reason <r>    blocking | security | invariant | verification | authorized
  -evidence <t>  what proves the reason applies

observe flags:
  -task <id>     task during which it was noticed
  -category <c>  architecture | security | performance | correctness |
                 maintainability | other
  -severity <s>  low | medium | high

Global:
  -C <dir>       run as if started in <dir>
  -v, --version  print the version

Exit codes:
  0  in scope, no scope declared, or no changes
  1  out of scope, or a forbidden path touched
  2  usage or configuration error
`

// version is stamped at release time via -ldflags "-X main.version=...".
//
// The --version case below is not decoration: an unreferenced package-level
// string is dead-code-eliminated, and -X then silently does nothing — the build
// still exits 0 and the value is simply absent from the binary.
var version = "dev"

const exitUsage = 2

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(exitUsage)
	}
	switch os.Args[1] {
	case "init":
		os.Exit(runInit(os.Args[2:]))
	case "scope":
		os.Exit(runScope(os.Args[2:]))
	case "observe":
		os.Exit(runObserve(os.Args[2:]))
	case "audit":
		os.Exit(runAudit(os.Args[2:]))
	case "verify":
		os.Exit(runVerify(os.Args[2:]))
	case "doctor":
		os.Exit(runDoctor(os.Args[2:]))
	case "uninstall":
		os.Exit(runUninstall(os.Args[2:]))
	case "hook":
		os.Exit(runHook(os.Args[2:]))
	case "-h", "--help", "help":
		fmt.Print(usage)
		os.Exit(0)
	case "-v", "--version", "version":
		fmt.Println("vector", version)
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "vector: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(exitUsage)
	}
}

// repoRoot resolves the repository once, so every command reports the same
// "not a repository" message instead of failing in its own dialect.
func repoRoot(dir string) (string, int) {
	root, err := gitx.Root(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vector: %v\n", err)
		return "", exitUsage
	}
	return root, 0
}

func runInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir := fs.String("C", ".", "directory to run in")
	noHooks := fs.Bool("no-hooks", false, "do not register hooks in the detected agent")
	sandboxOn := fs.Bool("sandbox", false, "let the OS deny forbidden writes")
	sandboxOff := fs.Bool("no-sandbox", false, "turn the sandbox off again")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	root, code := repoRoot(*dir)
	if code != 0 {
		return code
	}

	// The flag is a one-time decision that persists in policy.toml, so a later
	// bare `init` keeps whatever was chosen rather than silently reverting it.
	var sandbox *bool
	if *sandboxOn || *sandboxOff {
		on := *sandboxOn
		sandbox = &on
	}

	res, err := setup.InitWith(root, sandbox)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vector: %v\n", err)
		return exitUsage
	}

	verb := "written"
	if res.Existed {
		verb = "regenerated ([scope], [mode] and any [commands] override preserved)"
	}
	fmt.Printf("%s %s\n\n", res.PolicyPath, verb)

	// A protection that arrives silently is a protection nobody knows they
	// have, and one someone may have removed on purpose.
	if len(res.NewForbidden) > 0 {
		fmt.Printf("added %d deny path(s) this policy predated:\n", len(res.NewForbidden))
		for _, f := range res.NewForbidden {
			fmt.Printf("  + %s\n", f)
		}
		fmt.Println("  remove any you do not want; init will add them back, so it is a decision")
		fmt.Println()
	}

	s := res.Stack
	line("languages", strings.Join(s.Languages, ", "))
	pm := s.PM.Name
	if s.PM.Source != "" {
		pm += "  (" + s.PM.Source
		if s.PM.Lockfile != "" {
			pm += ": " + s.PM.Lockfile
		}
		pm += ")"
	}
	line("package manager", pm)
	line("  declared", s.PM.Declared)
	line("  installed", s.PM.Installed)
	line("runtime declared", s.Runtime.Declared)
	line("runtime installed", s.Runtime.Installed)

	if len(s.Frameworks) > 0 {
		names := make([]string, 0, len(s.Frameworks))
		for n := range s.Frameworks {
			names = append(names, n)
		}
		sort.Strings(names)
		fmt.Println("\nframeworks            declared         installed")
		for _, n := range names {
			v := s.Frameworks[n]
			mark := " "
			if detect.Disagrees(v.Declared, v.Installed) {
				mark = "!"
			}
			fmt.Printf("%s %-20s %-16s %s\n", mark, n, dash(v.Declared), dash(v.Installed))
		}
	}

	c := res.Commands
	fmt.Println()
	line("test", c.Test)
	line("typecheck", c.Typecheck)
	line("build", c.Build)
	line("lint", c.Lint)

	for _, n := range s.Notes {
		fmt.Printf("\n  note: %s\n", n)
	}

	if !*noHooks {
		hr, err := setup.InstallClaudeHooks(root)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\nvector: hooks not registered: %v\n", err)
			return exitUsage
		}
		fmt.Println()
		switch {
		case len(hr.Added) > 0:
			fmt.Printf("hooks registered in %s: %s\n", hr.Path, strings.Join(hr.Added, ", "))
			fmt.Println("  from here on, just run your agent normally")
		case len(hr.Present) > 0:
			fmt.Printf("hooks already registered in %s\n", hr.Path)
		}
	}

	if res.Policy.Mode.Sandbox {
		sr, err := setup.InstallClaudeSandbox(root, setup.SandboxForbidden(res.Policy.Scope.AlwaysForbidden))
		if err != nil {
			fmt.Fprintf(os.Stderr, "\nvector: sandbox not configured: %v\n", err)
			return exitUsage
		}
		switch {
		case len(sr.Added) > 0:
			fmt.Printf("\nOS sandbox configured in %s\n", sr.Path)
			fmt.Println("  writes are confined to this repository, and the forbidden paths are denied")
			fmt.Println("  Claude Code already protects its own config natively; these are the rest")
			fmt.Println("  run /sandbox in a session to see the resolved rules")
		default:
			fmt.Printf("\nOS sandbox already configured in %s\n", sr.Path)
		}
		if inert := setup.SandboxInert(root); len(inert) > 0 {
			fmt.Printf("  %d of them this platform's sandbox ignores: %s\n",
				len(inert), strings.Join(inert, ", "))
			fmt.Println("  it mounts concrete paths, so a pattern with a wildcard in it is discarded")
			fmt.Println("  the hook still denies these; the OS does not")
		}
	}
	return 0
}

func runUninstall(args []string) int {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir := fs.String("C", ".", "directory to run in")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	root, code := repoRoot(*dir)
	if code != 0 {
		return code
	}
	pol, err := scope.LoadPolicy(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vector: %v\n", err)
		return exitUsage
	}
	sr, err := setup.RemoveClaudeSandbox(root, setup.SandboxForbidden(pol.Scope.AlwaysForbidden))
	if err != nil {
		fmt.Fprintf(os.Stderr, "vector: %v\n", err)
		return exitUsage
	}
	if len(sr.Added) > 0 {
		fmt.Printf("withdrew %d sandbox denyWrite entr(ies) from %s\n", len(sr.Added), sr.Path)
		fmt.Println("  sandbox.enabled left as it was — turning off a boundary is not an uninstaller's call")
	}

	hr, err := setup.RemoveClaudeHooks(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vector: %v\n", err)
		return exitUsage
	}
	if len(hr.Added) == 0 && len(sr.Added) == 0 {
		fmt.Println("no vector hooks were registered")
		return 0
	}
	if len(hr.Added) == 0 {
		return 0
	}
	fmt.Printf("unregistered from %s: %s\n", hr.Path, strings.Join(hr.Added, ", "))
	fmt.Println("  .vector/ is left in place; remove it by hand if you want it gone")
	return 0
}

// runHook is the hot path: one invocation per tool call. It stays silent on
// failure, because a hook that errors loudly on every call is a hook people
// disable.
func runHook(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: vector hook <session-start|pre-tool|stop>")
		return exitUsage
	}
	if err := hook.Run(args[0], os.Stdin, os.Stdout, "."); err != nil {
		fmt.Fprintf(os.Stderr, "vector: %v\n", err)
		return 0
	}
	return 0
}

func runScope(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return exitUsage
	}
	switch args[0] {
	case "new":
		return runScopeNew(args[1:])
	case "expand":
		return runScopeExpand(args[1:])
	case "list":
		return runScopeList(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "vector scope: unknown subcommand %q\n", args[0])
		return exitUsage
	}
}

// patterns collects a repeatable flag, which is what -w needs to be.
type patterns []string

func (p *patterns) String() string     { return strings.Join(*p, ",") }
func (p *patterns) Set(v string) error { *p = append(*p, v); return nil }

const scopeNewUsage = "usage: vector scope new <id> -w <pattern> [-w <pattern>] [-o <objective>]"

func runScopeNew(args []string) int {
	// The task id is taken before parsing, because Go's flag package stops at
	// the first positional argument — flags written after the id would be
	// silently swallowed as arguments.
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(os.Stderr, scopeNewUsage)
		return exitUsage
	}
	taskID := args[0]

	fs := flag.NewFlagSet("scope new", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var write patterns
	fs.Var(&write, "w", "writable path (repeatable)")
	objective := fs.String("o", "", "the task's objective")
	dir := fs.String("C", ".", "directory to run in")
	if err := fs.Parse(args[1:]); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "vector: unexpected argument %q\n%s\n", fs.Arg(0), scopeNewUsage)
		return exitUsage
	}
	if len(write) == 0 {
		fmt.Fprintln(os.Stderr, "vector: at least one -w is required; an empty scope declares nothing")
		return exitUsage
	}
	root, code := repoRoot(*dir)
	if code != 0 {
		return code
	}
	path, err := setup.NewScope(root, taskID, *objective, write)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vector: %v\n", err)
		return exitUsage
	}
	fmt.Printf("%s written\n", path)
	inventory(root, write)
	fmt.Printf("  when done: vector audit -task %s\n", taskID)
	return 0
}

// inventory answers a boundary with what already lives inside it.
//
// It is printed here rather than reported by a hook because this is the one
// moment it can change anything: the agent has just said where it intends to
// write and has not written yet. Ten seconds later it is building something,
// and a list of neighbours is archaeology.
//
// It detects nothing. Vector cannot tell that ClientValidationService is the
// CustomerValidator that was already there — that needs a symbol index, which
// is state vector would own and git would not give it. This removes the excuse
// instead, and it is advice to a model, which is the weakest kind of control
// there is. It earns its place by costing one `git ls-files`.
func inventory(root string, write []string) {
	files, total := setup.Inventory(root, write)
	switch {
	case total == 0:
		fmt.Println("  nothing exists inside this boundary yet")
	case total > len(files):
		fmt.Printf("  %d files already live inside this boundary — too many to be"+
			" worth listing, which usually means the boundary is broader than the task\n", total)
	default:
		fmt.Printf("  %d file(s) already live inside this boundary — read before you add to it:\n", total)
		for _, f := range files {
			fmt.Printf("    %s\n", f)
		}
	}
}

const scopeExpandUsage = "usage: vector scope expand <id> -w <pattern> -reason <reason> -evidence <text>"

func runScopeExpand(args []string) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(os.Stderr, scopeExpandUsage)
		return exitUsage
	}
	taskID := args[0]

	fs := flag.NewFlagSet("scope expand", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var write patterns
	fs.Var(&write, "w", "path to add (repeatable)")
	reason := fs.String("reason", "", "reason for the expansion")
	evidence := fs.String("evidence", "", "what proves the reason applies")
	dir := fs.String("C", ".", "directory to run in")
	if err := fs.Parse(args[1:]); err != nil {
		return exitUsage
	}
	root, code := repoRoot(*dir)
	if code != 0 {
		return code
	}

	// The policy decides whether evidence is mandatory, so the rule lives in
	// the repository rather than in this binary.
	pol, err := scope.LoadPolicy(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vector: %v\n", err)
		return exitUsage
	}

	path, err := setup.ExpandScope(root, taskID, *reason, *evidence, write,
		pol.Scope.ExpansionRequiresEvidence)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vector: %v\n", err)
		return exitUsage
	}
	fmt.Printf("%s widened (%s)\n", path, *reason)
	for _, w := range write {
		fmt.Printf("  + %s\n", w)
	}
	// The same question applies to ground the boundary just grew onto, and it
	// is asked about the new patterns alone: the original ones were answered
	// when the scope was declared.
	inventory(root, write)
	return 0
}

func runObserve(args []string) int {
	if len(args) > 0 && args[0] == "list" {
		return runObserveList(args[1:])
	}

	fs := flag.NewFlagSet("observe", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	task := fs.String("task", "", "task during which it was noticed")
	category := fs.String("category", "", "category")
	severity := fs.String("severity", "", "severity")
	dir := fs.String("C", ".", "directory to run in")

	// The note is positional and may sit before or after the flags, so it is
	// pulled out first rather than fighting the flag parser for it.
	var note string
	var rest []string
	for _, a := range args {
		if note == "" && !strings.HasPrefix(a, "-") && !isFlagValue(args, a) {
			note = a
			continue
		}
		rest = append(rest, a)
	}
	if err := fs.Parse(rest); err != nil {
		return exitUsage
	}
	if note == "" && fs.NArg() > 0 {
		note = fs.Arg(0)
	}
	if note == "" {
		fmt.Fprintln(os.Stderr, `usage: vector observe "<note>" [-task <id>] [-category <c>] [-severity <s>]`)
		return exitUsage
	}

	root, code := repoRoot(*dir)
	if code != 0 {
		return code
	}
	// An observation made during a task belongs to it. Nobody passes -task by
	// hand — the agent recording the note is mid-work and the active task is
	// already on disk — so without this fallback the note is filed against
	// nothing, and a later session has no way to ask what this task noticed
	// and deliberately left alone.
	if *task == "" {
		*task = scope.Current(root)
	}
	o, err := observe.Record(root, observe.Observation{
		Task: *task, Category: *category, Severity: *severity, Note: note,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "vector: %v\n", err)
		return exitUsage
	}
	fmt.Printf("%s recorded (%s/%s) — action: defer\n", o.ID, o.Category, o.Severity)
	return 0
}

func runObserveList(args []string) int {
	fs := flag.NewFlagSet("observe list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir := fs.String("C", ".", "directory to run in")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	root, code := repoRoot(*dir)
	if code != 0 {
		return code
	}
	obs, err := observe.List(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vector: %v\n", err)
		return exitUsage
	}
	if len(obs) == 0 {
		fmt.Println("no observations recorded")
		return 0
	}
	for _, o := range obs {
		fmt.Printf("%-8s %-10s %-16s %s\n", o.ID, o.Severity, o.Category, firstLine(o.Note))
	}
	return 0
}

// isFlagValue reports whether v appears immediately after a flag, so a note
// is never mistaken for the argument of -task or -severity.
func isFlagValue(args []string, v string) bool {
	for i, a := range args {
		if a == v && i > 0 && strings.HasPrefix(args[i-1], "-") {
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func runScopeList(args []string) int {
	fs := flag.NewFlagSet("scope list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir := fs.String("C", ".", "directory to run in")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	root, code := repoRoot(*dir)
	if code != 0 {
		return code
	}
	ids, err := setup.ListScopes(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vector: %v\n", err)
		return exitUsage
	}
	if len(ids) == 0 {
		fmt.Println("no scopes declared")
		return 0
	}
	for _, id := range ids {
		fmt.Println(id)
	}
	return 0
}

func runAudit(args []string) int {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		task   = fs.String("task", "", "task id whose scope to enforce")
		base   = fs.String("base", "", "git ref to compare against")
		dir    = fs.String("C", ".", "directory to run in")
		asJSON = fs.Bool("json", false, "emit JSON")
	)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	rep, err := audit.Run(audit.Options{Dir: *dir, TaskID: *task, Base: *base})
	if err != nil {
		fmt.Fprintf(os.Stderr, "vector: %v\n", err)
		return exitUsage
	}

	var werr error
	if *asJSON {
		werr = rep.WriteJSON(os.Stdout)
	} else {
		werr = rep.WriteText(os.Stdout)
	}
	if werr != nil {
		fmt.Fprintf(os.Stderr, "vector: %v\n", werr)
		return exitUsage
	}
	return rep.ExitCode()
}

func runVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var only patterns
	fs.Var(&only, "only", "run just this check (repeatable)")
	task := fs.String("task", "", "task id whose scope to enforce")
	base := fs.String("base", "", "compare against this ref instead of HEAD")
	dir := fs.String("C", ".", "directory to run in")
	timeout := fs.Duration("timeout", 10*time.Minute, "per-command timeout")
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	rep, err := verify.Run(verify.Options{
		Dir: *dir, TaskID: *task, Base: *base, Only: only, Timeout: *timeout,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "vector: %v\n", err)
		return exitUsage
	}
	if *asJSON {
		err = rep.WriteJSON(os.Stdout)
	} else {
		err = rep.WriteText(os.Stdout)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "vector: %v\n", err)
		return exitUsage
	}
	return rep.ExitCode()
}

func runDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir := fs.String("C", ".", "directory to run in")
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	rep, err := doctor.Run(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vector: %v\n", err)
		return exitUsage
	}
	if *asJSON {
		err = rep.WriteJSON(os.Stdout)
	} else {
		err = rep.WriteText(os.Stdout)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "vector: %v\n", err)
		return exitUsage
	}
	return rep.ExitCode()
}

func line(label, val string) {
	fmt.Printf("%-22s %s\n", label, dash(val))
}

func dash(v string) string {
	if v == "" {
		return "—"
	}
	return v
}
