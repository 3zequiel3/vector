# Observations

Append-only log. An observation is NOT a task.

Finding a problem does not authorize fixing it. This stays here until a person
decides otherwise.

## OBS-001
- date: 2026-09-07
- category: correctness
- severity: medium
- action: defer

gitx.ChangedFiles uses 'git diff --name-only' without -z, so with core.quotePath (git's default) a non-ASCII path comes back quoted and octal-escaped: "raro_\303\261_test.go". That string is then normalized and matched against scope patterns, which cannot match it — a file with a non-ASCII name may be silently absent from an audit. Reproduced with: git -c core.quotePath=true diff --numstat HEAD. Same fix as Numstat: pass -z. Not done here because ChangedFiles is outside this task's boundary.

## OBS-002
- date: 2026-09-07
- category: correctness
- severity: high
- action: defer

Corrects OBS-001, which stated the severity backwards, and records that it is now fixed. OBS-001 said a non-ASCII path 'may be silently absent from an audit' — a false negative. It is the opposite: the quoted string reaches Decide, matches no write pattern, and the file is reported OUT_OF_SCOPE while being inside the declared boundary. Reproduced: a scope of src/** with src/año/indice.ts reported 2 out_of_scope findings. Under strict enforcement that is not a report, it is a denied write — vector refusing legitimate work in any repository with accents, Cyrillic or CJK in its paths. Fixed by passing -z to ChangedFiles and AllFiles, the same fix Numstat already had.

## OBS-003
- date: 2026-09-07
- task: session-recovery
- category: correctness
- severity: low
- action: defer

sessionStart appends a sentence period directly after the joined verification commands, so a command ending in ./... renders as 'go vet ./....' — an agent copying that literally runs a command with a fourth dot. Pre-existing (hook.go, the Verification line), not introduced by session-recovery, and outside this task's boundary.

## OBS-004
- date: 2026-09-07
- task: risk-paths
- category: security
- severity: medium
- action: defer

Private key material (*.pem, *.p12, *.pfx) is high_risk but not always_forbidden, so it is reported after the fact and never denied — while .env and .env.* ARE forbidden and denied unconditionally in every mode. A private key is arguably the more dangerous of the two: .env compromises a process, a host key compromises a host. Moving them to always_forbidden would deny writes in every mode, which breaks the legitimate case of generating test fixtures — that case already has an escape hatch in 'scope expand -reason security'. Raised by a risk review of the high_risk feature; left as the user's product decision rather than changed unilaterally, because it changes what vector blocks.
