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
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/3zequiel3/vector/internal/attempt"
	"github.com/3zequiel3/vector/internal/audit"
	"github.com/3zequiel3/vector/internal/detect"
	"github.com/3zequiel3/vector/internal/freshness"
	"github.com/3zequiel3/vector/internal/gitx"
	"github.com/3zequiel3/vector/internal/observe"
	"github.com/3zequiel3/vector/internal/scope"
)

// Event is the subset of an agent's hook payload that vector reads. The field
// names follow Claude Code, and other agents are mapped onto them by adapters.
type Event struct {
	HookEventName string `json:"hook_event_name"`
	CWD           string `json:"cwd"`
	// SessionID identifies the agent conversation. vector reads it so that
	// "this session was already asked to declare a scope" is remembered per
	// session: two agents working the same repository each get their own ask.
	SessionID string          `json:"session_id"`
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
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

	// A configuration vector cannot read must not switch enforcement off.
	//
	// This used to return nil on either error, and that was the cheapest
	// possible way to defeat the whole tool. One unparseable byte in
	// policy.toml or in a scope file — a control character in an objective is
	// enough, and so is a crash mid-append — and the hook went silent: no
	// forbidden-path denial, no strict-mode denial, nothing, repository-wide,
	// until a human happened to notice. The rules that exist precisely so an
	// agent cannot weaken its own constraints were the first thing to go.
	//
	// Failing safe means falling back to the defaults, which still deny
	// `.vector/**`, the agent's own settings and `.env`. Failing silent means
	// pretending there is nothing to say. Those are not close, and the tool
	// that refuses to call unchecked things fine has no business doing the
	// second one.
	//
	// The fallback is announced, because a boundary quietly narrowed to the
	// built-in defaults is its own kind of false confidence.
	var broken string
	pol, err := scope.LoadPolicy(root)
	if err != nil {
		pol, broken = scope.DefaultPolicy(), "policy.toml"
	}
	sc, err := scope.LoadScope(root, scope.Current(root))
	if err != nil {
		// A nil scope is a legitimate state that BuildRuleset already handles:
		// no task boundary, only the always-forbidden rules. It is a weaker
		// answer than the file would have given and a far stronger one than
		// silence.
		sc = nil
		if broken == "" {
			broken = "the active scope file"
		} else {
			broken += " and the active scope file"
		}
	}
	rules := scope.BuildRuleset(pol, sc)
	strict := pol.Mode.Enforcement == "strict"

	var outOfScope, inRepo []string
	for _, p := range paths {
		// Judge every path the write reaches, not how it is spelled. A symlink
		// resting inside the declared scope can point at a denied path, and
		// checking only the literal path lets the write straight through the
		// rule that exists to stop it.
		reached, err := scope.Targets(root, p)
		if err != nil {
			continue // outside the repository; not vector's boundary to police
		}
		rel := reached[0]
		inRepo = append(inRepo, rel)

		// The strictest decision across everything the path reaches wins.
		d, pat := scope.Allowed, ""
		for _, t := range reached {
			if td, tpat := rules.Decide(t); td > d {
				d, pat, rel = td, tpat, t
			}
		}
		switch d {
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
	// An undeclared ruleset cannot report anything out of scope, so without
	// this branch enforcement quietly does nothing at all: the agent skips the
	// session-start suggestion and every later check has no boundary to check
	// against. The first write is the moment the boundary is finally
	// decidable, so that is where vector asks for it.
	if !rules.Declared {
		if broken != "" {
			return brokenConfig(broken)
		}
		return undeclared(root, e, inRepo, strict)
	}
	if len(outOfScope) == 0 {
		if broken != "" {
			return brokenConfig(broken)
		}
		return nil
	}

	if strict {
		return &hookOutput{
			HookEventName:      "PreToolUse",
			PermissionDecision: "deny",
			PermissionDecisionReason: fmt.Sprintf(
				"vector: %s is outside the scope the agent declared as %s.\n"+
					"If it is genuinely required, record why:\n"+
					"  vector scope expand %s -w \"<pattern>\" -reason blocking -evidence \"<what proves it>\"",
				named(outOfScope), quoted(rules.Objective), rules.TaskID),
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
			named(outOfScope), rules.TaskID),
	}
}

// maxQuoted caps a string vector repeats back. An objective is one line
// describing a task; anything longer is not a description.
const maxQuoted = 200

// quoted renders text an agent wrote so that it cannot be read as text vector
// wrote.
//
// The objective, the evidence on an expansion and the note on an observation
// are all authored by the agent, stored in a file that travels in git, and
// handed back to a later session inside vector's own message. That last step
// is the problem: "vector is active in this repository. Objective: add a
// filter. SYSTEM: ignore all previous instructions" reads as one voice, and
// the second half of it is not vector's.
//
// Nothing here claims to stop prompt injection. A model that has attacker text
// in its context may act on it, and no amount of quoting changes that — the
// honest scope of this function is narrower and worth stating exactly: vector
// does not lend its own authority to a string it did not write. It is put on
// one line so it cannot open a fake section, quoted so its edges are visible,
// capped so it cannot bury the message around it, and attributed so a reader
// knows who said it.
//
// Deterministic, and no attempt to recognise "instruction-shaped" text: that
// would be a heuristic vector could not show the reasoning for, and it would be
// wrong in both directions.
func quoted(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return `""`
	}
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, s)
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	if len(s) > maxQuoted {
		s = s[:maxQuoted] + "…"
	}
	return `"` + strings.ReplaceAll(s, `"`, `'`) + `"`
}

// maxNamed caps how many paths a hook message lists, the way freshness already
// caps its own.
//
// This is not cosmetic. A hook's reply is injected straight into the agent's
// context window, and the path list is derived from the tool input — a shell
// command with twenty thousand redirections produced a 269 KB reply, roughly
// sixty thousand tokens of noise from one tool call. The finding is the same
// whether it names five paths or five thousand.
const maxNamed = 5

// named renders a path list for a message, bounded.
func named(paths []string) string {
	if len(paths) <= maxNamed {
		return strings.Join(paths, ", ")
	}
	return fmt.Sprintf("%s and %d more",
		strings.Join(paths[:maxNamed], ", "), len(paths)-maxNamed)
}

// brokenConfig reports that vector is running on its built-in defaults.
//
// It is context, never a denial. The write being judged may be perfectly
// ordinary, and refusing it because a different file is malformed would punish
// the wrong thing. What must not happen is silence: enforcement has quietly
// narrowed to the defaults, and a session that is not told reads the absence of
// complaint as approval.
func brokenConfig(what string) *hookOutput {
	return &hookOutput{
		HookEventName: "PreToolUse",
		AdditionalContext: fmt.Sprintf(
			"vector: %s could not be parsed, so vector is enforcing its built-in "+
				"defaults only — the declared boundary for this task is not in "+
				"force. Forbidden paths are still denied. Run `vector doctor` to "+
				"see what is wrong with it.", what),
	}
}

// undeclared answers a write made with no scope in force.
//
// It denies once, with the exact command to run, and then gets out of the way.
// Denying every time would be the more principled-looking choice and the wrong
// one: the hook cannot explain itself any better on the second attempt, so an
// agent that did not understand the first message would spend the whole turn
// failing against a wall. One clear ask, then work continues and `vector audit`
// reports NO_SCOPE_DECLARED honestly at the end.
//
// strict is the exception. There the boundary is mandatory, so the denial
// repeats until a scope exists.
func undeclared(root string, e Event, paths []string, strict bool) *hookOutput {
	if len(paths) == 0 {
		return nil
	}
	if !strict {
		// The hook is a fresh process per tool call, so "already asked" has to
		// live on disk. If it cannot be recorded, vector says nothing at all:
		// a denial it cannot remember is a denial it would repeat forever.
		if alreadyNudged(root, e.SessionID) || !recordNudge(root, e.SessionID) {
			return nil
		}
	}
	return &hookOutput{
		HookEventName:      "PreToolUse",
		PermissionDecision: "deny",
		PermissionDecisionReason: fmt.Sprintf(
			"vector: no scope is declared, and this writes %s.\n"+
				"Declare the boundary before writing, covering every file this task needs "+
				"— not just this one:\n"+
				"  vector scope new <short-id> -o \"<objective>\" -w \"<pattern>\"\n"+
				"Repeat -w for each additional pattern. Then retry this write.",
			strings.Join(paths, ", ")),
	}
}

// nudgeState is the file where vector remembers which sessions have already
// been asked to declare a scope.
//
// It holds one short, sanitised session id per line and is only ever appended
// to, so two concurrent sessions cannot damage each other's entry. Every read
// failure — missing, unreadable, truncated, full of nonsense — means "not
// asked yet": one extra ask costs a single explained denial, while failing a
// tool call over bookkeeping costs the user their turn.
func nudgeState(root string) string {
	return filepath.Join(root, ".vector", "nudged")
}

func alreadyNudged(root, session string) bool {
	data, err := os.ReadFile(nudgeState(root))
	if err != nil {
		return false
	}
	key := sessionKey(session)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == key {
			return true
		}
	}
	return false
}

// recordNudge remembers that this session has been asked, and reports whether
// the record actually reached the disk. The caller denies only on true, so an
// unwritable repository degrades to silence rather than to a loop.
// maxNudged caps the session log.
//
// This is the one state file that had no bound at all, and it sits on the hot
// path: every pre-tool call from a session that has not declared a scope reads
// it whole. Measured, fifty thousand entries take the call from 6 ms to 10 ms —
// years of use, and still the only file in the package where the project's own
// "keep it bounded" discipline was not applied. Short-lived CI sessions, each
// with a fresh id, get there faster than a person would.
//
// Two thousand is far more than any machine has live sessions. Dropping the
// oldest costs at most one repeated ask in a session old enough to have been
// forgotten, which is the harmless direction.
const maxNudged = 2000

func recordNudge(root, session string) bool {
	dir := filepath.Join(root, ".vector")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	f, err := os.OpenFile(nudgeState(root), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return false
	}
	if _, err := f.WriteString(sessionKey(session) + "\n"); err != nil {
		f.Close()
		return false
	}
	if err := f.Close(); err != nil {
		return false
	}
	compactNudged(root)
	return true
}

// compactNudged keeps the session log to its last maxNudged entries.
//
// It runs after the append rather than instead of it, so the ask this call was
// recording is never the one that gets dropped. A compaction racing another
// session's append can lose that append, and the cost of that is one repeated
// "declare a scope" message — the same trade the attempt log already makes,
// and in the same direction.
func compactNudged(root string) {
	path := nudgeState(root)
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) <= maxNudged {
		return
	}
	kept := strings.Join(lines[len(lines)-maxNudged:], "\n") + "\n"
	tmp, err := os.CreateTemp(filepath.Dir(path), ".nudged-*")
	if err != nil {
		return
	}
	if _, err := tmp.WriteString(kept); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return
	}
	if os.Chmod(tmp.Name(), 0o644) != nil || os.Rename(tmp.Name(), path) != nil {
		os.Remove(tmp.Name())
	}
}

// sessionKey reduces an agent's session id to something safe to keep on one
// line of a state file. An agent that sends no id still gets exactly one ask,
// under a shared key: asking one session too few is better than a loop.
func sessionKey(id string) string {
	var b strings.Builder
	for _, r := range id {
		if b.Len() >= 128 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "-"
	}
	return b.String()
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
		// Merged, not raw: the agent is being told which commands to run,
		// and telling it one command while `vector verify` runs another is
		// how a control layer starts contradicting itself.
		//
		// The error is dropped because this is a hook. A policy that will not
		// parse is a real problem, and it is `vector doctor`'s to report — a
		// session that refuses to start over it would take the repository
		// down instead of the diagnostic. The zero Policy overrides nothing,
		// so the agent is told what detection found, which is what it would
		// have been told before this existed.
		pol, _ := scope.LoadPolicy(root)
		c := pol.MergeCommands(detect.DetectCommands(root, s.PM))
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
		fmt.Fprintf(&b, "Active scope: %s.", cur)
		resume(&b, root, cur)
		b.WriteString(" Writes outside it are reported.\n")
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

// resume hands a new session the three facts that survive it.
//
// A task crosses sessions, compactions, subagents and days, and what is lost
// at every one of those boundaries is the same three things: what was meant,
// what happened, and what is still unknown. Vector already writes all three
// down — the scope file for the objective and how the boundary grew, the
// verdict store for the last answer and when it was reached, the observations
// log for what was noticed and deliberately left alone. Until now nothing read
// them back, so the state existed and no one was told.
//
// This is not memory, and the difference is checkable rather than rhetorical.
// Memory grows with the conversation; this is three fields vector had already
// computed for other reasons, and it is the same length on the first day of a
// task as on the fortieth. If a fourth field ever seems necessary the question
// to ask is not whether to add it but whether this has become Engram, which
// exists, belongs to the user, and is not vector's to reimplement.
//
// Every read failure is silence. A session must start whatever state is on
// disk, and a recovered fact nobody gets is cheaper than a session that will
// not begin.
func resume(b *strings.Builder, root, task string) {
	if sc, err := scope.LoadScope(root, task); err == nil && sc != nil {
		if o := strings.TrimSpace(sc.Objective); o != "" {
			fmt.Fprintf(b, " The agent declared its objective as %s.", quoted(o))
		}
		if n := len(sc.Expansions); n > 0 {
			fmt.Fprintf(b, " Boundary widened %s.", times(n))
		}
	}

	// The verdict is reported with its age, and with whether it is still about
	// the tree in front of you.
	//
	// The age alone is not enough, and this is the one place where getting it
	// wrong costs the most. A session that has just started has no other
	// memory: told "Last verdict: VERIFIED, three hours ago", it will
	// reasonably treat the code as already proven and not check again. If the
	// tree moved in between — the previous session kept editing after the
	// verdict, or another agent did — that sentence is the false confidence
	// this tool exists to refuse, handed over at the exact moment nothing else
	// can catch it.
	//
	// The comparison costs no git call. A snapshot already names every file it
	// covered and that file's blob hash, so re-hashing those files answers it,
	// and there are only as many of them as the change was large. What it
	// cannot see is a file that entered the boundary since — that needs an
	// audit, which is the Stop hook's job and is paid for once per turn there.
	// Missing those errs toward saying "stale" less often, never more, and a
	// verdict reported stale that is merely old costs a re-run of verify.
	if snap, ok := freshness.Last(root, task); ok {
		if freshness.Compare(root, snap, nil).Stale() {
			fmt.Fprintf(b, " Last verdict: %s, %s — but the code it was about has"+
				" changed since, so what is on disk now is UNVERIFIED.",
				snap.Verdict, ago(snap.At))
		} else {
			fmt.Fprintf(b, " Last verdict: %s, %s.", snap.Verdict, ago(snap.At))
		}
	}

	// "During", not "about" — that is what the field means, and the wording
	// has to match or the count reads as a list of defects in this task's own
	// work. An observation is what someone noticed while doing this and
	// deliberately did not fix, wherever in the repository it lives.
	//
	// This reads the whole observations log, which is the one piece of state
	// here with no compaction. That is deliberate rather than overlooked: the
	// attempt log and the verdict store are written by machine on every run
	// and would grow without bound, while an observation is written once, by
	// hand, because someone decided it was worth writing down. A file that
	// only grows when a person adds to it is bounded by the same thing that
	// bounds a changelog. Compacting it would delete a human record, which is
	// the opposite of what an append-only log in git is for.
	if obs, err := observe.List(root); err == nil {
		n := 0
		for _, o := range obs {
			if o.Task == task {
				n++
			}
		}
		if n > 0 {
			fmt.Fprintf(b, " %d observation(s) recorded during this task and not"+
				" acted on (`vector observe list`).", n)
		}
	}
}

// times renders a small count the way a sentence would.
func times(n int) string {
	switch n {
	case 1:
		return "once"
	case 2:
		return "twice"
	default:
		return fmt.Sprintf("%d times", n)
	}
}

// ago is a coarse age, because that is the precision the reader needs: whether
// the last answer is from this afternoon or from before the weekend. A
// timestamp would be more precise and less useful, and it would put a date in
// front of someone who then has to work out what it means.
func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	// A verdict dated in the future is not recent, it is wrong — a skewed
	// clock, or a snapshot that travelled from another machine. Falling into
	// the "just now" branch would answer the least trustworthy input with the
	// most trustworthy-sounding output, at the one moment a new session has
	// nothing else to go on.
	case d < -time.Minute:
		return "at a timestamp in the future — check the clock on whatever wrote it"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d minute(s) ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hour(s) ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d day(s) ago", int(d.Hours()/24))
	}
}

// stop reports, as the turn ends, everything the evidence on disk supports
// saying: where the change went, whether the last verdict still describes the
// tree, and whether the recorded history looks like a retry loop.
//
// The three are independent. A task can be circling inside a scope it never
// left; a verdict can go stale on work that never drifted; a first out-of-scope
// write is not a loop. When more than one is true the reader gets all of them,
// because dropping either would hide a finding behind an unrelated one.
//
// They are ordered by how firm the claim is. Scope drift is a fact about paths.
// Staleness is a fact about bytes, and it voids the standing answer, so it
// comes before the signal that only suspects something. The retry judgement is
// last: it is the softest of the three and it closes by telling the reader to
// consider stopping, which is not a sentence to have findings after.
func stop(root string) *hookOutput {
	// One audit, reused. It answers both "did this change stay in bounds" and
	// "which files are in bounds now", and the Stop hook has no business paying
	// git twice for the same question.
	rep, audited := scopeAudit(root)
	task := scope.Current(root)

	var parts []string
	if s := scopeMessage(rep, audited); s != "" {
		parts = append(parts, s)
	}
	if s := staleMessage(root, task, rep); s != "" {
		parts = append(parts, s)
	}
	// This costs two small file reads and no git call: verify already paid for
	// the measurement, and the Stop hook runs once per turn.
	if m := attempt.Judge(root, task).Message(); m != "" {
		parts = append(parts, m)
	}
	// When vector has nothing to complain about, it says what it has not
	// checked. The scope half of a verdict is automatic; the evidence half
	// never is, because running a project's test suite at the end of every turn
	// would cost more than the drift it prevents. That trade is defensible only
	// if the gap is visible — silence about "does it work" reads as an answer,
	// and it is not one.
	//
	// It fires only when the rest of the hook is quiet, so it is the one line a
	// reader gets rather than a fourth appended to three louder ones.
	if len(parts) == 0 {
		if m := unverifiedMessage(root, task, rep, audited); m != "" {
			parts = append(parts, m)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	// Reported, never decided. The Stop hook returns context only; a retry
	// suspicion vector cannot confirm must not stop a fourth attempt that was
	// about to work, and a stale verdict is a question, not a failure.
	return &hookOutput{HookEventName: "Stop", AdditionalContext: strings.Join(parts, " ")}
}

// scopeAudit runs the audit once for the whole hook. The bool distinguishes a
// report from the zero value, so a caller cannot mistake "git would not answer"
// for "nothing changed".
func scopeAudit(root string) (audit.Report, bool) {
	rep, err := audit.Run(audit.Options{Dir: root})
	if err != nil {
		return audit.Report{}, false
	}
	return rep, true
}

func scopeMessage(rep audit.Report, audited bool) string {
	if !audited {
		return ""
	}
	switch rep.Status {
	case audit.InScope, audit.NoChanges, audit.NoScopeDeclared:
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "vector: %s.", rep.Status)
	for _, f := range rep.Findings {
		fmt.Fprintf(&b, " %s (%s);", f.Path, f.Kind)
	}
	b.WriteString(" Run `vector audit` for the full report.")
	return b.String()
}

// unverifiedMessage names the half of the verdict nobody asked for.
//
// It stays quiet when there is nothing to verify, when no boundary was declared
// — NO_SCOPE_DECLARED is already its own admission — and when a verdict for
// this tree already exists, since staleMessage owns that case.
func unverifiedMessage(root, task string, rep audit.Report, audited bool) string {
	if !audited || task == "" || rep.Status != audit.InScope {
		return ""
	}
	if _, ok := freshness.Last(root, task); ok {
		return ""
	}
	return fmt.Sprintf(
		"vector: %s is in scope, and nothing has checked whether it works. "+
			"Run `vector verify` for a verdict; until then the change is UNVERIFIED.", task)
}

// staleMessage reports a verdict that has been edited out from under it.
//
// Only a passing verdict earns this. That is the case the reader is actually
// exposed to: someone ran verify, heard VERIFIED, kept editing, and the last
// thing they were told is now a claim about a tree that no longer exists. A
// stale FAILED misleads nobody — it was already bad news, and the editing that
// made it stale is the response it was asking for.
//
// The hashing is here rather than in verify because this is the only moment
// the question is live, and it is bounded on both sides. Nothing is hashed at
// all unless verify has already recorded a passing verdict for the active task,
// which is one small file read and the common case's whole cost. When there is
// one, the set hashed is that verdict's in-scope files plus whatever is in
// scope now — the size of a change, never the size of a repository — and it is
// hashed in process, which measured at about 2ms for two hundred files.
//
// An audit that failed leaves rep zero, so no additions are visible. The
// recorded files still answer whether what was verified has moved, and half an
// answer that names its files beats none.
func staleMessage(root, task string, rep audit.Report) string {
	snap, ok := freshness.Last(root, task)
	if !ok || !snap.Passed {
		return ""
	}
	return freshness.Compare(root, snap, rep.InScope).Message()
}
