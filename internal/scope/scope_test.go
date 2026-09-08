package scope

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/3zequiel3/vector/internal/detect"
)

func ruleset(write, forbidden []string, declared bool) Ruleset {
	return Ruleset{Write: write, Forbidden: forbidden, Declared: declared}
}

func TestDecide(t *testing.T) {
	tests := []struct {
		name      string
		write     []string
		forbidden []string
		declared  bool
		path      string
		want      Decision
	}{
		{"globstar matches nested", []string{"src/dashboard/**"}, nil, true, "src/dashboard/a/b/c.ts", Allowed},
		{"globstar does not match sibling", []string{"src/dashboard/**"}, nil, true, "src/api/users.ts", OutOfScope},
		{"bare directory covers its subtree", []string{"src/dashboard"}, nil, true, "src/dashboard/x.ts", Allowed},
		{"bare directory matches itself", []string{"src/dashboard"}, nil, true, "src/dashboard", Allowed},
		{"bare directory is not a prefix match", []string{"src/dash"}, nil, true, "src/dashboard/x.ts", OutOfScope},
		{"single star stays in one segment", []string{"src/*.ts"}, nil, true, "src/a/b.ts", OutOfScope},
		{"single star matches one segment", []string{"src/*.ts"}, nil, true, "src/a.ts", Allowed},

		// Deny beats allow, unconditionally. This is the rule that closes the
		// self-weakening hole: an agent granted a broad write scope must still
		// not be able to edit the config that constrains it.
		{"deny beats a matching allow", []string{"**"}, []string{".claude/settings.json"}, true, ".claude/settings.json", Forbidden},
		{"deny applies without any scope", nil, []string{".vector/**"}, false, ".vector/policy.toml", Forbidden},
		{"deny glob matches nested", []string{"**"}, []string{".claude/hooks/**"}, true, ".claude/hooks/guard.sh", Forbidden},
		{"dotfile glob matches", nil, []string{".env.*"}, false, ".env.local", Forbidden},

		// An undeclared boundary cannot be crossed; only denies apply.
		{"undeclared allows anything not denied", nil, []string{".vector/**"}, false, "src/anything.ts", Allowed},

		// Pattern hygiene: authored patterns should not have to be perfect.
		{"leading ./ in pattern is tolerated", []string{"./src/**"}, nil, true, "src/a.ts", Allowed},
		{"trailing slash in pattern is tolerated", []string{"src/"}, nil, true, "src/a.ts", Allowed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := ruleset(tt.write, tt.forbidden, tt.declared).Decide(tt.path)
			if got != tt.want {
				t.Errorf("Decide(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestDecideReportsTheRule(t *testing.T) {
	r := ruleset([]string{"**"}, []string{".claude/hooks/**"}, true)
	d, pat := r.Decide(".claude/hooks/guard.sh")
	if d != Forbidden {
		t.Fatalf("decision = %v, want Forbidden", d)
	}
	if pat != ".claude/hooks/**" {
		t.Errorf("pattern = %q, want the original authored pattern", pat)
	}
}

func TestNormalize(t *testing.T) {
	const root = "/repo"
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"src/a.ts", "src/a.ts", false},
		{"./src/a.ts", "src/a.ts", false},
		{"src/../src/a.ts", "src/a.ts", false},
		{"/repo/src/a.ts", "src/a.ts", false},

		// Traversal must be an invariant of the matcher, not a burden on
		// whoever authored the patterns.
		{"../outside.ts", "", true},
		{"src/../../outside.ts", "", true},
		{"/etc/passwd", "", true},
		{"", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := Normalize(root, tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Normalize(%q) = %q, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Normalize(%q) returned %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("Normalize(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestTraversalCannotEscapeADeny(t *testing.T) {
	// The failure this guards against: a path that matches no deny pattern in
	// its raw form, but resolves into a denied location once cleaned.
	r := ruleset([]string{"src/**"}, []string{".claude/**"}, true)

	rel, err := Normalize("/repo", "src/../.claude/settings.json")
	if err != nil {
		t.Fatalf("Normalize returned %v", err)
	}
	if d, _ := r.Decide(rel); d != Forbidden {
		t.Errorf("Decide(%q) = %v, want Forbidden", rel, d)
	}
}

func TestDefaultPolicyProtectsItsOwnEnforcement(t *testing.T) {
	r := BuildRuleset(DefaultPolicy(), &Scope{Write: []string{"**"}})
	for _, p := range []string{
		".vector/policy.toml",
		".claude/settings.json",
		".claude/hooks/guard.sh",
		".env",
	} {
		if d, _ := r.Decide(p); d != Forbidden {
			t.Errorf("Decide(%q) = %v, want Forbidden even under a wide-open scope", p, d)
		}
	}
}

func TestSymlinkCannotLaunderAForbiddenPath(t *testing.T) {
	// The attack, and the reason Targets exists: create a link inside the
	// declared scope that points at a denied path, then write to the link. The
	// link's own path passes every rule; the target is the one that matters.
	root := t.TempDir()
	mkdirs(t, root, "src", ".claude")
	writeFile(t, root, ".claude/settings.json", "{}")
	if err := os.Symlink(
		filepath.Join(root, ".claude", "settings.json"),
		filepath.Join(root, "src", "notes.json"),
	); err != nil {
		t.Skip("symlinks unavailable:", err)
	}

	r := BuildRuleset(DefaultPolicy(), &Scope{Write: []string{"src/**"}})
	reached, err := Targets(root, "src/notes.json")
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}

	worst := Allowed
	for _, p := range reached {
		if d, _ := r.Decide(p); d > worst {
			worst = d
		}
	}
	if worst != Forbidden {
		t.Errorf("decision = %v over %v, want Forbidden — the link laundered the target", worst, reached)
	}
}

func TestSymlinkedDirectoryIsResolvedForAFileNotYetCreated(t *testing.T) {
	// The other half of the same attack: link the directory, then write a file
	// that does not exist yet. EvalSymlinks fails on a missing path, so
	// resolution has to walk up to the nearest ancestor that does exist.
	root := t.TempDir()
	mkdirs(t, root, "src", ".claude/hooks")
	if err := os.Symlink(
		filepath.Join(root, ".claude", "hooks"),
		filepath.Join(root, "src", "vendor"),
	); err != nil {
		t.Skip("symlinks unavailable:", err)
	}

	r := BuildRuleset(DefaultPolicy(), &Scope{Write: []string{"src/**"}})
	reached, err := Targets(root, "src/vendor/guard.sh")
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	worst := Allowed
	for _, p := range reached {
		if d, _ := r.Decide(p); d > worst {
			worst = d
		}
	}
	if worst != Forbidden {
		t.Errorf("decision = %v over %v, want Forbidden for a new file under a linked directory", worst, reached)
	}
}

func TestSymlinkOutOfTheRepositoryIsAnEscape(t *testing.T) {
	// A link leaving the repository cannot be judged by repo-relative patterns,
	// so it is refused rather than silently allowed.
	root := t.TempDir()
	outside := t.TempDir()
	mkdirs(t, root, "src")
	writeFile(t, outside, "secret", "x")
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "src", "link")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if _, err := Targets(root, "src/link"); err == nil {
		t.Error("a link out of the repository was accepted as an ordinary path")
	}
}

func TestTargetsLeavesOrdinaryPathsAlone(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "src")
	writeFile(t, root, "src/a.ts", "x")
	got, err := Targets(root, "src/a.ts")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "src/a.ts" {
		t.Errorf("Targets = %v, want just the path itself", got)
	}
}

func mkdirs(t *testing.T, root string, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMergeCommandsOverridesFieldByField(t *testing.T) {
	detected := detect.Commands{
		Test:      "go test ./...",
		Typecheck: "",
		Build:     "go build ./...",
		Lint:      "go vet ./...",
	}

	tests := []struct {
		name     string
		override detect.Commands
		want     detect.Commands
	}{
		{
			// The common case. Most repositories never touch [commands], and
			// an empty override must leave detection exactly as it found it.
			name:     "empty policy changes nothing",
			override: detect.Commands{},
			want:     detected,
		},
		{
			// The reason this exists: a project whose tests hide behind a
			// wrapper detection cannot see. Correcting one command must not
			// cost the three that were already right.
			name:     "one override leaves the others detected",
			override: detect.Commands{Test: "make test"},
			want: detect.Commands{
				Test:      "make test",
				Typecheck: "",
				Build:     "go build ./...",
				Lint:      "go vet ./...",
			},
		},
		{
			// Detection found nothing for typecheck. Supplying one is the
			// difference between PARTIALLY_VERIFIED and a real check.
			name:     "an override can fill what detection missed",
			override: detect.Commands{Typecheck: "make typecheck"},
			want: detect.Commands{
				Test:      "go test ./...",
				Typecheck: "make typecheck",
				Build:     "go build ./...",
				Lint:      "go vet ./...",
			},
		},
		{
			// An empty string is absence, not an instruction to skip. A
			// policy cannot silently disable a check it declines to mention,
			// because a check that vanishes is exactly the missing evidence
			// this tool refuses to call verified.
			name:     "an empty override does not erase a detected command",
			override: detect.Commands{Test: "", Lint: "   "},
			want:     detected,
		},
		{
			name: "every field can be overridden at once",
			override: detect.Commands{
				Test: "a", Typecheck: "b", Build: "c", Lint: "d",
			},
			want: detect.Commands{Test: "a", Typecheck: "b", Build: "c", Lint: "d"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p Policy
			p.Commands = tt.override
			if got := p.MergeCommands(detected); got != tt.want {
				t.Errorf("MergeCommands() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestMergeCommandsDoesNotMutateTheDetectedValue(t *testing.T) {
	// Callers detect once and may report the raw result alongside the merged
	// one. Merging must not reach back and edit what detection actually found.
	detected := detect.Commands{Test: "go test ./..."}
	var p Policy
	p.Commands = detect.Commands{Test: "make test"}

	_ = p.MergeCommands(detected)

	if detected.Test != "go test ./..." {
		t.Errorf("detected was mutated: Test = %q", detected.Test)
	}
}

func TestPolicyCommandsRoundTripThroughTOML(t *testing.T) {
	// The override is only useful if a human can write it into policy.toml and
	// have it survive the load, which is the one path nothing else covers.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".vector"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "[commands]\ntest = \"make test\"\n\n[mode]\nenforcement = \"strict\"\n"
	if err := os.WriteFile(filepath.Join(dir, ".vector", "policy.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	p, err := LoadPolicy(dir)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	if p.Commands.Test != "make test" {
		t.Errorf("Commands.Test = %q, want %q", p.Commands.Test, "make test")
	}
	// A policy that declares commands must not thereby lose the defaults it
	// never mentioned — the forbidden set is what keeps vector's own config
	// out of reach.
	if len(p.Scope.AlwaysForbidden) == 0 {
		t.Error("always_forbidden was emptied by a policy that did not mention it")
	}
}

func TestUndeclaredRisk(t *testing.T) {
	// The rule: the pattern that authorizes a dangerous path must itself be a
	// dangerous path. The only way to evade it is to write "migrations/**",
	// which is the declaration that was wanted.
	tests := []struct {
		name  string
		write []string
		path  string
		want  bool // true = allowed but never declared
	}{
		// The shapes that used to slip through, and the reason the first
		// version of this check was worth almost nothing.
		{"the bare catch-all declares nothing", []string{"**"}, "migrations/0003.sql", true},
		{"nor does its doubled spelling", []string{"**/**"}, "migrations/0003.sql", true},
		{"nor a single star", []string{"*"}, "Dockerfile", true},
		{"nor the top directory most repos use", []string{"src/**"}, "src/migrations/0003.sql", true},
		{"nor enumerating every top-level directory", []string{"src/**", "docs/**", "infra/**"}, "infra/main.tf", true},

		// Naming it is the whole requirement, and it is a low bar on purpose.
		{"naming the directory declares it", []string{"migrations/**"}, "migrations/0003.sql", false},
		{"naming it under a prefix declares it", []string{"src/migrations/**"}, "src/migrations/0003.sql", false},
		{"naming the extension declares it", []string{"**/*.tf"}, "infra/main.tf", false},
		{"naming the file declares it", []string{"Dockerfile"}, "Dockerfile", false},
		// One naming pattern is enough; it does not matter what else is beside it.
		{"a naming pattern among broad ones is enough", []string{"src/**", "migrations/**"}, "migrations/0003.sql", false},

		// Ordinary source is never the subject of this rule.
		{"ordinary source is not risky at all", []string{"**"}, "src/components/Button.tsx", false},
	}

	p := DefaultPolicy()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := BuildRuleset(p, &Scope{Write: tt.write})
			_, got := r.Undeclared(tt.path)
			if got != tt.want {
				t.Errorf("Undeclared(%q) under %v = %v, want %v", tt.path, tt.write, got, tt.want)
			}
		})
	}
}

func TestUndeclaredNeedsADeclaredBoundary(t *testing.T) {
	// With no scope at all there is nothing that failed to declare anything.
	// NO_SCOPE_DECLARED is already the honest verdict and says more.
	r := BuildRuleset(DefaultPolicy(), nil)
	if _, got := r.Undeclared("migrations/0003.sql"); got {
		t.Error("reported an undeclared risk against a boundary that was never drawn")
	}
}

func TestRiskyIsIndependentOfBeingAllowed(t *testing.T) {
	// A path can be perfectly in scope and still be a migration. Collapsing
	// the two would force a report that wants to say "allowed, and worth
	// looking at" to pick one of them.
	r := BuildRuleset(DefaultPolicy(), &Scope{Write: []string{"**"}})

	for _, p := range []string{
		"migrations/0003_drop.sql",
		".github/workflows/deploy.yml",
		"infra/main.tf",
		"Dockerfile",
		"certs/server.pem",
	} {
		if _, risky := r.Risky(p); !risky {
			t.Errorf("Risky(%q) = false, want it named", p)
		}
		if d, _ := r.Decide(p); d != Allowed {
			t.Errorf("Decide(%q) = %v, want Allowed — risk is not a denial", p, d)
		}
	}
	for _, p := range []string{"src/components/Button.tsx", "README.md", "internal/scope/scope.go"} {
		if _, risky := r.Risky(p); risky {
			t.Errorf("Risky(%q) = true, want ordinary source left alone", p)
		}
	}

	// Deliberately absent from the defaults, and pinned so that adding them
	// back is a decision rather than a slip. Each fires on ordinary work: an
	// auth folder is usually a front-end component directory, "migrate" is a
	// common package name and every vendored copy of golang-migrate, and an
	// extension is not content — a .key is as likely to be a Keynote deck.
	for _, p := range []string{
		"src/auth/LoginButton.tsx",
		"vendor/github.com/golang-migrate/migrate/migrate.go",
		"docs/deck.key",
	} {
		if _, risky := r.Risky(p); risky {
			t.Errorf("Risky(%q) = true — this shape fires on ordinary work", p)
		}
	}
}

func TestHighRiskSurvivesAPolicyThatDoesNotMentionIt(t *testing.T) {
	// Existing repositories have a policy.toml written before this list
	// existed. They must get the defaults rather than an empty list, or the
	// feature would be silently off everywhere it was not re-initialised.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".vector"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "[scope]\nalways_forbidden = [\".vector/**\"]\n\n[mode]\nenforcement = \"advisory\"\n"
	if err := os.WriteFile(filepath.Join(dir, ".vector", "policy.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	p, err := LoadPolicy(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Scope.HighRisk) == 0 {
		t.Error("high_risk was emptied by a policy written before it existed")
	}
}

func TestAnExplicitlyEmptyListTurnsTheRuleOff(t *testing.T) {
	// Omitting a key keeps the default; writing "= []" means the project
	// decided it wants none. Those must not be the same thing — a repository
	// whose layout makes the defaults noisy needs a way out that is not
	// copying and then maintaining the whole list by hand.
	//
	// The same mechanics apply to always_forbidden, where the consequence is
	// far larger: emptying it removes vector's protection over its own config.
	// `vector doctor` fails on exactly that, which is why it is a diagnostic
	// rather than something LoadPolicy refuses.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".vector"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "[scope]\nhigh_risk = []\n"
	if err := os.WriteFile(filepath.Join(dir, ".vector", "policy.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	p, err := LoadPolicy(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Scope.HighRisk) != 0 {
		t.Errorf("high_risk = %v, want the explicit opt-out honoured", p.Scope.HighRisk)
	}
	r := BuildRuleset(p, &Scope{Write: []string{"**"}})
	if _, risky := r.Risky("migrations/0003.sql"); risky {
		t.Error("a path was still called risky with the list explicitly emptied")
	}
	// And the rest of the defaults survive: emptying one list is not a request
	// to empty the others.
	if len(p.Scope.AlwaysForbidden) == 0 {
		t.Error("always_forbidden was emptied by a policy that only mentioned high_risk")
	}
}
