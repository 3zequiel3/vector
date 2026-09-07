package scope

import "testing"

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
