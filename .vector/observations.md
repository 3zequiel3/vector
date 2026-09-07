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
