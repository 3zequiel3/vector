# Reference

Everything vector requires, installs, writes and returns. The README explains
why the tool exists; this page is the part you look things up in.

---

## Requirements

Only the first row is required. Everything below it changes what vector *could*
do, and — with one exception noted per row — vector does not read any of them
today. `vector doctor` reports the same table for your machine, under
`INTEGRATIONS`.

| Piece | Level | Unlocks | Without it |
| --- | --- | --- | --- |
| **git** | required | the entire tool: the repository root, the changed-file set, the evidence a verdict rests on | nothing runs. Every command exits 2 with "not a git repository" |
| **an agent with hooks** | recommended | T5 interception — the boundary is enforced before a write reaches disk. Only Claude Code is wired today | vector drops to T2: `vector audit` still detects everything, after the fact |
| **Go toolchain (1.27+)** | optional | building from source, and `go install` | use the install script or a release binary; the built binary needs no Go |
| **gentle-ai** | optional | seeding a task scope from `gentle-ai sdd-status <change> --cwd <repo> --json`, whose `actionContext.allowedEditRoots` is already a declared boundary | nothing. **vector does not consume this yet** — the integration is possible, not present |
| **openspec** | optional | structural validation of change artifacts via `openspec validate --changes --json` | nothing. **Not consumed yet** |
| **chronicle** | optional | doc/code drift as an evidence source, read from the fingerprints in `.ledger/fingerprints.json` | nothing. **Not consumed yet** |
| **atlas** | optional | seeding a task scope from a change's "Leer antes" pointers in `CHANGES.md` | nothing. **Not consumed yet** |
| **engram** | optional | nothing vector can reach | nothing. `~/.engram/engram.db` is detected and reported **present but unreadable**: it exposes no CLI and no documented external read path, so vector neither reads it nor should |

The distinction the last five rows keep making is deliberate. A detected
integration and a working integration are different facts, and a diagnostic
that lets the first read as the second is manufacturing the false confidence
this tool exists to refuse. Today the honest answer for all of them is
*detected, not consumed*.

### How each is detected

| Piece | Evidence |
| --- | --- |
| gentle-ai | binary on `PATH`; its version comes from `gentle-ai --version`, falling back to `gentle-ai version`. An unrecognised flag is not an error — the binary is reported without a version rather than with a scraped usage line |
| openspec | binary on `PATH`, or an `openspec/` directory in the repository, or both |
| chronicle | `.ledger/fingerprints.json` in the repository. The file, not the directory: an empty `.ledger/` proves only that something once intended to track drift |
| atlas | `CHANGES.md` at the repository root |
| engram | `~/.engram/engram.db`. Per-developer, so finding it says nothing about the project |

---

## Installation

### Install script

```bash
curl -fsSL https://raw.githubusercontent.com/3zequiel3/vector/main/install.sh | sh
```

It resolves the latest release, downloads the build for your OS and
architecture, and **refuses to install anything it cannot verify against
`checksums.txt`**. It never uses sudo and never writes outside the install
directory.

| Variable | Effect |
| --- | --- |
| `VECTOR_VERSION` | install this tag instead of the latest (`v1.2.3` or `1.2.3`) |
| `VECTOR_INSTALL_DIR` | install here instead of `~/.local/bin` |

Linux and macOS, amd64 and arm64. On Windows, download
`vector_<version>_windows_amd64.zip` from the releases page and put `vector.exe`
on your PATH yourself.

### go install

```bash
go install github.com/3zequiel3/vector/cmd/vector@latest
```

Lands in `$(go env GOPATH)/bin`. Note that `version` is stamped through
`-ldflags` at release time, so a `go install` build reports `dev`.

### From source

```bash
git clone https://github.com/3zequiel3/vector
cd vector
go build ./cmd/vector
```

### Where the binary lands, and the PATH caveat

| Method | Location |
| --- | --- |
| install script | `~/.local/bin/vector`, or `$VECTOR_INSTALL_DIR/vector` |
| `go install` | `$(go env GOPATH)/bin/vector` |
| `go build` | the current directory |

**The most common failure after a successful install is a binary on disk that
the shell cannot see.** The install script checks `PATH` and prints the exact
line to add to your shell's rc file when the directory is missing from it. It
matters more than usual here: the hooks `vector init` registers invoke `vector`
*by name*, so a binary that your interactive shell can find but your agent's
environment cannot will register hooks that silently do nothing.
`vector doctor` reports this under `INSTALLATION`.

If you cannot get the directory onto the agent's PATH, put the absolute path
into `.claude/settings.json` instead of the bare `vector`.

### Uninstall

```bash
vector uninstall     # in the repository — removes vector's hook entries only
rm -rf .vector       # in the repository — removes the config, scopes, observations
rm ~/.local/bin/vector
```

`vector uninstall` rewrites `.claude/settings.json` removing exactly the entries
it wrote, leaving every other hook, permission and setting in place. It
deliberately does **not** delete `.vector/`: the scopes and observations there
are a record of decisions, and a tool that deletes those on its way out is one
you cannot safely try. Remove the directory yourself when you want it gone.

---

## Commands

Every command accepts `-C <dir>` to run as if started in another directory.

```
vector -h | --help | help          print usage, exit 0
vector -v | --version | version    print the version, exit 0
```

### `vector init`

Detects the stack, writes `.vector/policy.toml`, and registers hooks in
`.claude/settings.json`. Re-running is safe: `[stack]` and `[commands]` are
regenerated from local evidence, `[scope]` and `[mode]` are preserved verbatim,
and hook entries that already exist are not duplicated.

| Flag | Meaning |
| --- | --- |
| `-no-hooks` | write the policy but register nothing in the agent |

Exit: 0 on success, 2 on a usage or configuration error (including a
`.claude/settings.json` that is not valid JSON — it refuses to rewrite a file it
could not parse). No JSON output.

### `vector uninstall`

Unregisters vector's hook entries, leaving other hooks intact. Exit 0, or 2 if
the settings file cannot be read or parsed. No JSON output.

### `vector scope new <id> -w <pattern>`

Declares a task boundary at `.vector/scope/<id>.toml`, and makes it the active
task. Refuses to overwrite an existing scope.

| Flag | Meaning |
| --- | --- |
| `-w <pattern>` | writable path, repeatable. At least one is required — an empty scope declares nothing |
| `-o <text>` | the task's objective |

The id may not contain `/`, `\` or `.`. Flags must come after the id: Go's flag
parser stops at the first positional argument, so `-w` written before the id
would be swallowed. Exit 0, or 2 on any of the above.

### `vector scope expand <id> -w <pattern>`

Widens a boundary by **appending** an `[[expansion]]` block — the original
`write` list is never edited, so the file stays a record of how the boundary
grew and on what grounds.

| Flag | Meaning |
| --- | --- |
| `-w <pattern>` | path to add, repeatable, at least one |
| `-reason <r>` | one of `blocking`, `security`, `invariant`, `verification`, `authorized` |
| `-evidence <t>` | what proves the reason applies |

When `expansion_requires_evidence = true` (the default), an expansion without
`-evidence` is refused with exit 2. That refusal is the mechanism: a boundary
that widens on assertion alone is not a boundary.

### `vector scope list`

Prints the declared task ids, one per line, or `no scopes declared`. Exit 0.

### `vector observe "<note>"`

Records something the agent noticed without acting on it. The note is
positional and may sit before or after the flags.

| Flag | Values | Default |
| --- | --- | --- |
| `-task <id>` | task during which it was noticed | empty |
| `-category <c>` | `architecture`, `security`, `performance`, `correctness`, `maintainability`, `other` | `other` |
| `-severity <s>` | `low`, `medium`, `high` | `medium` |

An unknown category or severity is an error, not a silent coercion: exit 2.
Appends to `.vector/observations.md` and prints `OBS-NNN recorded (…) — action:
defer`. `action: defer` is not a parameter; recording is the whole of the
permitted response.

### `vector observe list`

One line per observation: id, severity, category, first line of the note. Exit 0.

### `vector audit`

Compares what changed against the declared boundary. This is the set operation
the whole tool reduces to.

| Flag | Meaning |
| --- | --- |
| `-task <id>` | scope to enforce; defaults to the active task in `.vector/current` |
| `-base <ref>` | compare against a git ref instead of the working tree |
| `-json` | emit `vector.audit/v1` |

| Status | Exit |
| --- | --- |
| `IN_SCOPE` | 0 |
| `NO_SCOPE_DECLARED` | 0 |
| `NO_CHANGES` | 0 |
| `OUT_OF_SCOPE` | 1 |
| `FORBIDDEN` | 1 |

Exit 2 for a usage or configuration error. Untracked files count as changes: a
brand-new file outside the boundary is exactly the drift this exists to catch.

### `vector verify`

Runs the project's own declared checks and combines the result with the scope
audit.

| Flag | Meaning | Default |
| --- | --- | --- |
| `-only <name>` | run just this check, repeatable: `typecheck`, `lint`, `test`, `build` | all declared |
| `-task <id>` | scope to enforce for the conformance half | the active task |
| `-timeout <d>` | per-command timeout | `10m` |
| `-json` | emit `vector.verify/v1` | |

| Verdict | Meaning | Exit |
| --- | --- | --- |
| `VERIFIED` | in scope, and every declared check passed | 0 |
| `PARTIALLY_VERIFIED` | what ran passed, but something checkable is missing | 0 |
| `FAILED` | a check returned non-zero | 1 |
| `OUT_OF_SCOPE` | the change left its boundary — reported even when every check passed | 1 |
| `UNVERIFIED` | nothing ran | 1 |

Two rules decide the verdict. Nothing is `VERIFIED` unless something actually
ran — a repository that declares no test command has not been shown to work,
however green the rest is. And scope outranks evidence: passing tests do not
retroactively authorize files nobody declared.

Checks run cheapest first — typecheck, lint, test, build — because a type error
explains the test failures that follow. `verify` is never invoked by a hook:
running a suite at the end of every turn would cost more than the waste it
prevents.

### `vector doctor`

Answers whether vector is doing anything at all. It reports what it verified,
never that anything is safe.

| Flag | Meaning |
| --- | --- |
| `-json` | emit `vector.doctor/v1` |

| Exit | When |
| --- | --- |
| 0 | no check failed — warnings included |
| 1 | at least one check returned `fail` |
| 2 | not a git repository |

A warning never fails the exit code. Degraded enforcement is still enforcement,
and treating it as broken would train people to ignore the output.

Groups, in output order: `repository`, `stack`, `verification`, `scopes`,
`enforcement`, `installation`, `integrations`.

### `vector hook <event>`

Not for you to run. The agent invokes it once per event; `vector init` registers
it. Events: `session-start`, `pre-tool`, `stop`. Reads the agent's JSON payload
on stdin, writes its response on stdout, and **exits 0 even when it fails** — a
hook that errors loudly on every tool call is a hook people disable. (Calling it
with no event at all is a usage error: exit 2.)

Note the consequence: a broken vector cannot break your agent, and it also
cannot tell you it is broken. That is what `vector doctor` is for.

### JSON schemas

| Schema | Emitted by | Top-level fields |
| --- | --- | --- |
| `vector.audit/v1` | `vector audit -json` | `schema`, `status`, `task_id`, `objective`, `enforcement`, `base`, `expansions`, `changed_files`, `in_scope`, `findings` |
| `vector.verify/v1` | `vector verify -json` | `schema`, `verdict`, `reason`, `scope` (a full `vector.audit/v1` report), `checks` |
| `vector.doctor/v1` | `vector doctor -json` | `schema`, `root`, `enforcement_tier`, `checks` |

Gate on the JSON field, never on the presence of output.

A `vector.doctor/v1` check is `{group, name, level, detail}`, with `hint`,
`present`, `unlocks` and `use` omitted when empty. `level` is `info`, `ok`,
`warn` or `fail`. The last three fields are set only by the `integrations`
group:

| Field | Meaning |
| --- | --- |
| `present` | the piece was found on this machine |
| `unlocks` | what vector could do with it |
| `use` | `consumed` (vector reads it today), `not-yet` (present and ignored), `unreadable` (no external read path exists) |

The schema name stays at `v1`: the three fields are `omitempty`, so a consumer
written against the original shape reads the same bytes it read before. It sees
one additional group of `info`-level checks, which it was already required to
tolerate — `level` was never a closed set of two.

---

## Files vector writes

| Path | What it is | In git? | Owner |
| --- | --- | --- | --- |
| `.vector/policy.toml` | the repository contract: detected stack, detected verification commands, always-forbidden paths, enforcement mode | **yes** | split. `[stack]` and `[commands]` are regenerated by `init`; `[scope]` and `[mode]` are yours and survive verbatim |
| `.vector/scope/<id>.toml` | one task's declared boundary, plus the appended record of every expansion and its evidence | **yes** — this is the reviewable artifact | the agent writes it via `scope new` / `scope expand`; you read it |
| `.vector/observations.md` | append-only log of what the agent noticed and did not act on | **yes** | the agent appends; nothing rewrites it |
| `.vector/current` | the active task id, so `audit` and the hooks work without `-task` | **no** — gitignored | per-developer working state, like `.git/HEAD`. Committing it would make every teammate's checkout fight over whose task is current |
| `.vector/.gitignore` | contains exactly `current` | **yes** | written once by `init`, never overwritten |
| `.claude/settings.json` | your agent's settings. `init` merges three hook entries into `hooks`; `uninstall` removes exactly those | **yes**, usually | **yours.** vector merges, never replaces: other hooks, permissions and settings survive both operations, and a file it cannot parse is refused rather than rewritten |

The always-forbidden defaults cover `.vector/**`, `.claude/settings*.json`,
`.claude/hooks/**`, `.codex/hooks.json`, `.cursor/hooks.json`, `.env` and
`.env.*`. The first two matter most: an agent that can edit the configuration
constraining it is not constrained, and `vector doctor` reports a policy that
does not cover `.vector/` as a **failure**, not a warning.

---

## Enforcement tiers

| Tier | Mechanism | Guarantee | Reached today? |
| --- | --- | --- | --- |
| **T5** interception | native `PreToolUse` hook | high, with [~5% documented leaks](https://github.com/anthropics/claude-code/issues/45427) — subagents, Bash heredocs, silent hook failures | **yes, on Claude Code only** |
| **T3** confinement | OS sandbox (Seatbelt, bubblewrap) | absolute — survives a bypassed hook | **no.** vector does not configure, launch or verify a sandbox |
| **T2** observation | `git diff` against the boundary | detection is total; prevention is none | **yes, always** |
| **T1** advice | `AGENTS.md`, `CLAUDE.md` | none | out of scope — vector writes neither |

Counter-intuitively, **T2 is more reliable than T5**. A hook has measured leaks;
a set operation over git cannot fail. T2 does not prevent, but it never lies.
That is why `audit` remains the tool's centre of gravity even where a hook is
installed.

### What is actually wired

`vector init` writes hooks for **Claude Code and nothing else**, into
`.claude/settings.json`:

| Event | Matcher | Command |
| --- | --- | --- |
| `SessionStart` | — | `vector hook session-start` |
| `PreToolUse` | `Edit\|Write\|MultiEdit\|NotebookEdit\|Bash` | `vector hook pre-tool` |
| `Stop` | — | `vector hook stop` |

`Bash` is in the matcher because a hook watching only `Edit` and `Write` misses
the shape that actually slips past: a redirect in a shell command.

`vector doctor` detects Codex CLI, Cursor, Gemini CLI, OpenCode and Amp on
`PATH`, and warns when one is installed with no vector hook registered. Those
agents expose the same primitive under different event names, so the adapters
are translation rather than new architecture — but they are **not written**, and
`doctor` reports T2 on a machine running them. It reports T5 only when it finds
an actual invocation of the vector binary in an agent's configuration, not the
bare word `vector`.

`vector doctor` never reports "safe". The strongest statement it makes is which
tier was actually reached.
