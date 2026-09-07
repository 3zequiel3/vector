<div align="center">

# vector

**The scope boundary for AI coding agents.** A deterministic control layer that compiles a declared scope into a hard denial, audits what actually changed with a set operation that cannot fail, and gets out of the way.

[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.27-00ADD8.svg)](go.mod)
[![Tests](https://img.shields.io/badge/tests-128-green.svg)](#development)
[![Status](https://img.shields.io/badge/status-MVP-orange.svg)](#status)
[![Deterministic](https://img.shields.io/badge/model%20calls-zero-black.svg)](#what-vector-is-not)

**English** · [Español](README.es.md)

</div>

---

## What it is

An AI coding agent asked to add a dashboard filter will sometimes also refactor your auth middleware. vector makes that visible, and — once the hook lands — stops it before it reaches disk.

```
you                          your agent                      vector
├── "add a date filter"  ──►  Claude Code       ──────────►  ┌──────────────┐
│                             Codex · Cursor                 │    SCOPE     │  declared paths
│                             Gemini · OpenCode               │   EVIDENCE   │  git diff
└── vector audit         ◄──  (unmodified)      ◄──────────  │   VERDICT    │  exit 0 / 1
                                                             └──────────────┘
```

Three primitives, no more. **Scope** is a set of writable paths. **Evidence** is what git says actually happened. **Verdict** is the set difference between them.

vector never runs your agent, never holds context, never routes skills, and never remembers anything. It answers one question: *did the change stay inside the boundary declared for it?*

---

## The problem

This is measured, not anecdotal. [OverEager-Bench](https://arxiv.org/abs/2605.18583) ran 500 scenarios across ~7,500 executions:

| Setup | Out-of-scope action rate |
| --- | --- |
| Claude Code · Codex CLI · Gemini CLI | **5.4 % – 27.7 %** |
| OpenHands with ask-to-continue | 0.2 % – 4.5 % |

An order of magnitude, decided by whether anything asks. And prompting alone does not close it: with explicit instructions not to, models still selected unauthorized tools in [48–68 % of adversarial scenarios](https://arxiv.org/pdf/2605.18414), 96 % under role escalation. Allowlists written into the prompt reduce violations to 4 % — never to zero. A gate outside the prompt reaches 0 % by construction.

That gap is the entire reason vector exists.

---

## What vector is **not**

Aggressively, and on purpose:

- **Not an agent runtime.** No LLM loop, no tool-use loop, no context window.
- **Not a memory system.** Your repository is the source of truth.
- **Not a knowledge base, RAG index, or embeddings store.**
- **Not a spec framework.** OpenSpec and Spec Kit exist.
- **Not a multi-agent orchestrator.**
- **Not an AI reviewer.** Auditing generated code with more generation is the failure mode, not the fix.
- **Not a daemon.** Nothing runs in the background.

There are **zero model calls** anywhere in vector. Every decision is a set operation over normalized paths, and every answer names the rule that produced it.

---

## Install

```bash
go install github.com/3zequiel3/vector/cmd/vector@latest
```

Then, once per repository:

```bash
cd your-project
vector init
```

`init` detects the stack, writes `.vector/policy.toml`, and registers its hooks in `.claude/settings.json` — merging into whatever is already there, never replacing it. `vector uninstall` removes exactly those entries and leaves the rest alone.

**That is the whole setup.** Keep running `claude` the way you always have. You never declare a scope, never pass a task id, never remember to audit. Use `--no-hooks` if you would rather wire it yourself.

---

## Commands

```
vector init                       detect the stack, write config, register hooks
vector uninstall                  unregister the hooks, leaving others intact
vector doctor                     check whether vector is actually doing anything
vector audit                      compare the working tree against the boundary
vector verify                     run the project's checks and give a full verdict

# the agent runs these; you rarely will
vector scope new <id> -w <pat>    declare a task's boundary (and make it active)
vector scope expand <id> -w <pat> widen it, with a reason and evidence
vector scope list                 list declared scopes
vector observe "<note>"           record something noticed, without acting on it
vector observe list               list recorded observations
```

### A session

You type two words. Everything else is the hooks.

```console
$ claude
> add a date filter to the dashboard
```

`SessionStart` hands the agent the project's real commands and one instruction: declare your boundary once you know it.

```
vector is active in this repository.
Package manager: pnpm. Verification — test: pnpm run test; build: pnpm run build.
No scope is declared. Once you know which files this task needs — after
exploring, before your first edit — declare it:
  vector scope new <short-id> -o "<objective>" -w "<path pattern>"
```

**The agent declares the scope, not you.** That ordering is the point: after exploring it knows which files the task needs, and before exploring nobody does. Asking a person to predict paths up front is asking them to do the work they opened the agent for.

Then `PreToolUse` decides every write. In scope, it says nothing:

```console
$ # Write src/dashboard/Filter.tsx  →  (silence)
```

Out of scope, it reports and offers both honest ways out:

```
vector: src/middleware/auth.ts is outside the declared scope. Continuing
(advisory mode). If this belongs to the task, run `vector scope expand
date-filter -w "<pattern>" -reason blocking -evidence "<what proves it>"`;
if it does not, leave it and record it with `vector observe "<note>"`.
```

And a forbidden path is denied outright, including through the shell — the shape that slips past a hook watching only `Edit` and `Write`:

```console
$ # Bash: cat > .env << EOF
vector: .env is forbidden by .env
```

`Stop` runs the audit as the turn ends, so you see the result without asking.

---

## Version intelligence

`vector init` never assumes a stack, and it keeps four kinds of truth apart, because collapsing them into one "version" is how a tool ends up confidently wrong:

| | source | authority |
| --- | --- | --- |
| **declared** | manifest, `engines`, `packageManager`, `.nvmrc` | intent — not truth |
| **resolved** | the lockfile's identity picks the package manager | what would be installed |
| **installed** | `node_modules/`, `.venv`, `--version` | **what actually runs** |
| **upstream** | the registry | deliberately not collected |

```console
$ vector init
package manager        pnpm  (lockfile: pnpm-lock.yaml)
  installed            11.8.0
runtime declared       24.x
runtime installed      24.17.0

frameworks            declared         installed
  next                 16.2.12          16.2.12
  react                19.2.4           19.2.4
  tailwindcss          ^4               4.3.3

test                   pnpm run test
typecheck              pnpm run typecheck
```

Package-manager detection walks upward from the working directory to the repository root — never past it — applying the same ordered strategies at each level: lockfile, then `packageManager`, then `devEngines`, then install metadata. Verification commands are read from the project's own manifests. vector invokes them; it never invents them.

A range like `^4` against an installed `4.3.3` is **not** reported as a disagreement. vector only claims a mismatch it can prove without a semver solver: an exact pin that differs.

---

## Scope, and how it is allowed to grow

Finding a problem does not authorize fixing it. A boundary widens only for one of five reasons, and only with evidence:

`blocking` · `security` · `invariant` · `verification` · `authorized`

```console
$ vector scope expand date-filter -w "src/api/types.ts" -reason blocking
vector: policy requires evidence (expansion_requires_evidence = true);
        pass -evidence with what proves "blocking" applies
```

With evidence, the expansion is **appended, never merged** into the original declaration:

```toml
objective = "add a date filter to the dashboard"

write = ["src/dashboard/**"]              # the original declaration, intact

[[expansion]]
at = "2026-09-07T08:17:09Z"
reason = "blocking"  # prevents completing the requested task
evidence = "pnpm run typecheck fails: src/api/types.ts does not export DateFilter"
write = ["src/api/types.ts"]
```

The file becomes the record of how the boundary grew and on what grounds — reviewable, in git. Editing `write` in place would erase exactly that history.

### Observations

Everything else the agent noticed goes here, and stops here:

```console
$ vector observe "auth middleware mixes session and token in one layer" \
    -category architecture -severity medium
OBS-001 recorded (architecture/medium) — action: defer
```

`action: defer` is not a parameter. Recording is the whole of the permitted response.

---

## Enforcement tiers

vector is honest about which tier it actually reaches, and `vector doctor` reports it:

| tier | mechanism | guarantee |
| --- | --- | --- |
| **T3** confinement | OS sandbox — **what `vector init -sandbox` configures** | absolute — survives a bypassed hook, and covers writes vector cannot see |
| **T5** interception | native `PreToolUse` hook — **what `vector init` registers** | high, with [~5 % documented leaks](https://github.com/anthropics/claude-code/issues/45427) |
| **T2** observation | `git diff` against the boundary | **detection is total, prevention is none** |
| **T1** advice | `AGENTS.md`, `CLAUDE.md` | none |

Counter-intuitively, **T2 is more reliable than T5**: a hook has measured leaks — subagents, Bash heredocs, silent failures — while a set operation over git cannot fail. It does not prevent, but it never lies.

```console
$ vector doctor
REPOSITORY
  ok   self-protection    vector's own config is outside the writable scope
SCOPES
  warn date-filter        patterns matching no file in the repo: src/dashbaord/**
                          → usually a typo; check the path
ENFORCEMENT
  warn Claude Code        installed, no vector hook registered
       tier               T2 observation — detects 100 % after the fact,
                          but prevents nothing

0 failure(s), 2 warning(s) — enforcement at T2
```

Nothing in vector ever reports "safe". The strongest statement it makes is which tier was actually reached.

---

## The verdict

`audit` answers where the change went. `verify` adds whether it works, by running the commands the project already declares — and combines both:

```console
$ vector verify
PARTIALLY_VERIFIED — passed: lint, test, build — but no scope was declared,
                     so conformance was not checked

  ---- typecheck  not declared by the project
  ok   lint       go vet ./...     (101ms)
  ok   test       go test ./...    (2.307s)
  ok   build      go build ./...   (254ms)
```

Two rules decide the verdict:

**Nothing is `VERIFIED` unless something actually ran.** A repository that declares no test command has not been shown to work, however green the rest is. "Not checked" never becomes "fine".

**Scope outranks evidence.** A change that passes every test but touched files nobody declared is still `OUT_OF_SCOPE`. Passing tests do not retroactively authorize the work.

| verdict | meaning | exit |
| --- | --- | --- |
| `VERIFIED` | in scope, and every declared check passed | 0 |
| `PARTIALLY_VERIFIED` | what ran passed, but something checkable is missing | 0 |
| `FAILED` | a check returned non-zero | 1 |
| `OUT_OF_SCOPE` | the change left its boundary — reported even when checks pass | 1 |
| `UNVERIFIED` | nothing ran | 1 |

Checks run cheapest first — typecheck, lint, test, build — because a type error explains the test failures that follow. Each has a timeout, so a hung suite fails loudly instead of hanging.

When a task keeps failing, `verify` and the `Stop` hook say so:

```
vector: 4 failed verifies in a row on date-filter, with no passing run in
between. The diff grew from 99 to 812 changed lines across them. Nothing is
blocked — vector cannot tell a productive attempt from an unproductive one.
Consider stopping and asking a human what to change.
```

Three consecutive failures is the threshold: one is work, two is the ordinary
correction most fixes land on, and three is the first that the two-try pattern
does not explain. A passing run ends the streak. Nothing is ever blocked on this
— it is a signal that a loop may be happening, and a tool that halts legitimate
work on a heuristic gets uninstalled.

`verify` is never run by a hook. Running a test suite at the end of every turn would cost more than the waste it prevents; `Stop` runs the cheap scope audit and leaves the expensive question to you or to CI.

## Exit codes

| command | 0 | 1 | 2 |
| --- | --- | --- | --- |
| `audit` | in scope, no scope declared, or no changes | out of scope, or a forbidden path touched | usage or config error |
| `doctor` | no failed check | a check failed | not a repository |

Both accept `-json` and emit a versioned schema (`vector.audit/v1`, `vector.doctor/v1`). **Gate on the JSON field, never on the presence of output.**

---

## Status

Working and dogfooded: 11 commands, 128 tests, zero model calls, two dependencies.

Claude Code is the only agent whose hooks `init` writes today. Codex, Cursor and Gemini expose the same primitive under different event names, so the adapters are translation rather than new architecture — but they are not written yet, and `doctor` will honestly report T2 on those.

Still open: the Codex, Cursor and Gemini hook adapters, and verdict staleness — a passing verify should expire when the code it was about moves.

---

## Reference

[`docs/reference.md`](docs/reference.md) is the complete reference: what is required
versus merely recommended, every command with its flags and exit codes, every file
vector writes and whether it belongs in git, and which enforcement tier is actually
reached on which agent.

## Development

```bash
go test ./...        # 128 tests
go vet ./...
gofmt -l .
```

Two dependencies: [`BurntSushi/toml`](https://github.com/BurntSushi/toml) and [`bmatcuk/doublestar`](https://github.com/bmatcuk/doublestar) — the latter because the standard library's `filepath.Match` has no `**`.

---

<div align="center">

**English** · [Español](README.es.md)

</div>
