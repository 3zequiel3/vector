<div align="center">

# vector

**Keeps an AI coding agent inside the task you asked for.**

You ask for a date filter. The agent also refactors your auth middleware.
vector notices, tells you, and — where it can — stops it first.

[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.27-00ADD8.svg)](go.mod)
[![Tests](https://img.shields.io/badge/tests-247-green.svg)](#development)
[![Status](https://img.shields.io/badge/status-MVP-orange.svg)](#status)
[![Deterministic](https://img.shields.io/badge/model%20calls-zero-black.svg)](#what-vector-is-not)

**English** · [Español](README.es.md)

</div>

---

## What it does

You ask your agent for one thing. Somewhere along the way it also touches three
files nobody mentioned. Sometimes that is necessary and sometimes it is drift,
and today nothing tells you which happened until you read the diff yourself.

vector watches the boundary of a task and answers two questions:

- **Did this change stay where it was supposed to?**
- **Does it actually work?**

It never runs your agent, never holds context, never talks to a model. Every
answer is a comparison it can show you.

---

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/3zequiel3/vector/main/install.sh | sh
```

No Go toolchain needed — this downloads a checksummed binary and refuses to
install one it cannot verify. If the install directory is not on your `PATH`,
the script prints the exact line to add and which file to add it to.

> **Not yet.** No release has been tagged, so this command has nothing to
> download. Until the first tag lands, use `go install` below.

<details>
<summary>Other ways to install</summary>

```bash
go install github.com/3zequiel3/vector/cmd/vector@latest   # needs Go 1.27+
```

Building from source: `git clone`, then `go build ./cmd/vector`.

`go install` puts the binary in `~/go/bin`, which is often not on `PATH`. The
curl installer exists partly to avoid that.

</details>

Then, once per repository:

```bash
cd your-project
vector init
```

That is the whole setup. **Keep running `claude` exactly as you always have.**

<details>
<summary>What <code>vector init</code> actually does to your repo</summary>

- Detects your stack — package manager, runtime, frameworks, and the project's
  own test/build/lint commands — and writes them to `.vector/policy.toml`
- Registers three hooks in `.claude/settings.json`, **merging** into whatever is
  already there rather than replacing it
- Nothing else. `vector uninstall` removes exactly those entries.

Add `-sandbox` to also let the OS deny writes to forbidden paths, and `-no-hooks`
to skip the hook registration entirely.

</details>

---

## What happens then

Nothing you have to do. Here is the whole loop.

**1. You work normally.**

```console
$ claude
> add a date filter to the dashboard
```

**2. The agent declares what it is about to touch** — not you. It does this
after exploring and before its first edit, which is the only moment anyone
knows the answer. vector stops that first write and asks:

```
vector: no scope is declared, and this writes src/dashboard/Filter.tsx.
Declare the boundary before writing, covering every file this task needs:
  vector scope new <short-id> -o "<objective>" -w "<pattern>"
```

**3. Writes inside the boundary are silent.** Writes outside it are reported:

```
vector: src/middleware/auth.ts is outside the declared scope. Continuing
(advisory mode). If this belongs to the task, run `vector scope expand ...`;
if it does not, leave it and record it with `vector observe "<note>"`.
```

**4. When the turn ends, you get the summary** without asking for it.

That is it. You typed `vector init` once and then a sentence to your agent.

<details>
<summary>Running it by hand</summary>

```bash
vector audit      # did the change stay in scope?
vector verify     # ...and does it work? runs your project's own test/build/lint
vector doctor     # is vector actually doing anything on this machine?
```

`vector doctor` is the one to run when something feels off. It reports what it
verified, never that things are "fine".

</details>

---

### Who types what

This is the part worth being precise about, because vector's whole claim is that
you are not the one running it.

| | who | when |
| --- | --- | --- |
| `vector init` | **you** | once per repository |
| `vector scope new` | the agent | its first write, because the hook stops it and asks |
| `vector scope expand` | the agent | when the hook reports a write outside the boundary |
| `vector observe` | the agent | when the hook tells it to record rather than act |
| the scope audit | the `Stop` hook | every turn, automatically |
| staleness and retry signals | the `Stop` hook | every turn, automatically |
| **`vector verify`** | **you** | **never automatic — see below** |

Everything the agent runs is triggered by a hook denying or reporting something,
so it happens whether or not the agent was in the mood.

`verify` is the one exception, and it is deliberate: running a project's test
suite at the end of every turn would cost more than the drift it prevents. So
when a change is in scope and nothing has verified it, the `Stop` hook says
exactly that rather than letting silence read as an answer:

```
vector: date-filter is in scope, and nothing has checked whether it works.
Run `vector verify` for a verdict; until then the change is UNVERIFIED.
```

---

## Why it exists

This is not a hunch. It is measured. [OverEager-Bench](https://arxiv.org/abs/2605.18583) ran 500 scenarios across ~7,500 executions:

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

Four rules decide the verdict, and each one closes a different way of looking finished:

**Nothing is `VERIFIED` unless something actually ran.** A repository that declares no test command has not been shown to work, however green the rest is. "Not checked" never becomes "fine".

**Scope outranks evidence.** A change that passes every test but touched files nobody declared is still `OUT_OF_SCOPE`. Passing tests do not retroactively authorize the work.

**Nothing is `VERIFIED` if the change edited its own judge.** A suite with its assertions deleted exits zero. A suite deleted outright exits zero, loudly and in green — and METR saw explicit reward hacking in 39 of 128 unprompted runs of o3 on RE-Bench, with exactly those mechanisms. So when a change is net-subtractive across the project's test files, the verdict is capped and says so:

```console
PARTIALLY_VERIFIED — passed: lint, test, build — but this change removed
6 line(s) from 1 test file and added 0 (total_test.go), so the suite that
passed is not the suite that was there
```

There is no parser here and no assertion counter — those are per-language and they rot. It reads `git diff --numstat`, which git had already computed. It never fails a build: removing lines from a test is ordinary, and vector cannot tell a consolidation from a gutting.

**A boundary that is not about migrations does not authorize one.** Some paths are written deliberately or not at all: migrations, CI workflows, terraform, key material. When a change touches one and no declared pattern was *about* it, the verdict is capped and the path is named. `migrations/**` declares a migration; `src/**` does not, however many migrations live under `src`.

| verdict | meaning | exit |
| --- | --- | --- |
| `VERIFIED` | in scope, and every declared check passed | 0 |
| `PARTIALLY_VERIFIED` | what ran passed, but something checkable is missing | 0 |
| `FAILED` | a check returned non-zero | 1 |
| `OUT_OF_SCOPE` | the change left its boundary — reported even when checks pass | 1 |
| `UNVERIFIED` | nothing ran | 1 |

Checks run cheapest first — typecheck, lint, test, build — because a type error explains the test failures that follow. Each has a timeout, so a hung suite fails loudly instead of hanging.

### When a verdict stops being true

`VERIFIED` is a claim about a tree. Keep editing and it stops being one, so the
`Stop` hook says so:

```
vector: the VERIFIED verdict for date-filter is stale — src/dashboard/Filter.tsx
changed since it was reached. What is on disk now is UNVERIFIED, not failed:
nothing is blocked and no exit code changed.
```

Stale is not failed. The vocabulary already had a word for unknown and this is
it. Only passing verdicts are reported stale — a stale `FAILED` misleads nobody,
and the editing that made it stale is the response it was asking for.

The identifier is git's own blob hash, so nothing new is invented and no other
tool is required.

### When a task keeps failing

`verify` and the `Stop` hook say so:

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

**In CI, pass `-base`.** A fresh checkout has a working tree identical to `HEAD`, so every diff is empty and a branch that gutted its tests three commits ago looks like no change at all:

```bash
vector verify -base origin/main -json
```

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

## Exit codes

| command | 0 | 1 | 2 |
| --- | --- | --- | --- |
| `audit` | in scope, no scope declared, or no changes | out of scope, or a forbidden path touched | usage or config error |
| `doctor` | no failed check | a check failed | not a repository |

Both accept `-json` and emit a versioned schema (`vector.audit/v1`, `vector.doctor/v1`). **Gate on the JSON field, never on the presence of output.**

---

## Status

Working and dogfooded: 11 commands, 247 tests, zero model calls, two dependencies.

Claude Code is the only agent whose hooks `init` writes today. Codex, Cursor and Gemini expose the same primitive under different event names, so the adapters are translation rather than new architecture — but they are not written yet, and `doctor` will honestly report T2 on those.

Still open: the Codex, Cursor and Gemini hook adapters. They expose the same primitive under different event names, so that is translation rather than new architecture — but until it is written, `doctor` reports T2 on those machines and means it.

The number that would justify all of this still does not exist: nobody has measured how often an agent actually goes out of scope on these repositories. `vector audit` is the instrument, and running it for a week costs nothing.

---

## Reference

[`docs/reference.md`](docs/reference.md) is the complete reference: what is required
versus merely recommended, every command with its flags and exit codes, every file
vector writes and whether it belongs in git, and which enforcement tier is actually
reached on which agent.

## Development

```bash
go test ./...        # 247 tests
go vet ./...
gofmt -l .
```

Those three run on every push, if you enable the hook once per clone:

```bash
git config core.hooksPath .githooks
```

It rejects the push and says which of the three failed, printing only the part
that failed. `git push --no-verify` skips it — a check you cannot bypass when you
know better is a check people stop using.

There is no CI workflow. The checks a pipeline would run happen before the push
instead, which is faster to act on and does not depend on an account being in
good standing. The release workflow stays, because building binaries is not
something a laptop should be trusted to do reproducibly.

Two dependencies: [`BurntSushi/toml`](https://github.com/BurntSushi/toml) and [`bmatcuk/doublestar`](https://github.com/bmatcuk/doublestar) — the latter because the standard library's `filepath.Match` has no `**`.

---

<div align="center">

**English** · [Español](README.es.md)

</div>
