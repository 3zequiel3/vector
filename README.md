<div align="center">

# vector

**The scope boundary for AI coding agents.** A deterministic control layer that compiles a declared scope into a hard denial, audits what actually changed with a set operation that cannot fail, and gets out of the way.

[![Go](https://img.shields.io/badge/go-1.27-00ADD8.svg)](go.mod)
[![Tests](https://img.shields.io/badge/tests-42-green.svg)](#development)
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

That is the whole setup. Keep using `claude`, `codex`, `cursor` or whatever you already use — vector does not sit in your workflow.

---

## Commands

```
vector init                       detect the stack, write .vector/policy.toml
vector scope new <id> -w <pat>    declare a task's boundary
vector scope expand <id> -w <pat> widen it, with a reason and evidence
vector scope list                 list declared scopes
vector observe "<note>"           record something noticed, without acting on it
vector observe list               list recorded observations
vector audit [-task <id>]         compare the working tree against the boundary
vector doctor                     check whether vector is actually doing anything
```

### A session

```console
$ vector scope new date-filter -o "add a date filter to the dashboard" \
    -w "src/dashboard/**" -w "src/dashboard/__tests__/**"
.vector/scope/date-filter.toml written

$ claude                                    # your normal workflow, untouched
> add a date filter to the dashboard

$ vector audit -task date-filter
OUT OF SCOPE — 2 of 9 file(s) were not declared
  objective: add a date filter to the dashboard
  out_of_scope   src/middleware/auth.ts
  out_of_scope   src/lib/session.ts
```

Two files nobody asked for. That is the number this tool exists to produce.

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
| **T3** confinement | OS sandbox (Seatbelt, bubblewrap) | absolute — survives a bypassed hook |
| **T5** interception | native `PreToolUse` hook | high, with [~5 % documented leaks](https://github.com/anthropics/claude-code/issues/45427) |
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

## Exit codes

| command | 0 | 1 | 2 |
| --- | --- | --- | --- |
| `audit` | in scope, no scope declared, or no changes | out of scope, or a forbidden path touched | usage or config error |
| `doctor` | no failed check | a check failed | not a repository |

Both accept `-json` and emit a versioned schema (`vector.audit/v1`, `vector.doctor/v1`). **Gate on the JSON field, never on the presence of output.**

---

## Status

The deterministic core is complete and dogfooded: 8 commands, 42 tests, zero model calls.

The `PreToolUse` hook — the piece that moves enforcement from T2 to T5 — is **not built yet**, deliberately. It is three weeks of work justified by a number nobody has measured: the out-of-scope baseline on real repositories. `vector audit` is the instrument for that measurement, which is why it came first.

Until then, vector detects. It does not prevent, and it says so.

---

## Development

```bash
go test ./...        # 42 tests
go vet ./...
gofmt -l .
```

Two dependencies: [`BurntSushi/toml`](https://github.com/BurntSushi/toml) and [`bmatcuk/doublestar`](https://github.com/bmatcuk/doublestar) — the latter because the standard library's `filepath.Match` has no `**`.

---

<div align="center">

**English** · [Español](README.es.md)

</div>
