package suite

import (
	"strings"
	"testing"
)

func TestIsTest(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		// The ecosystems vector already detects, by their own convention.
		{"internal/scope/scope_test.go", true},
		{"tests/test_import.py", true},
		{"src/import_test.py", true},
		{"src/Button.test.tsx", true},
		{"src/Button.spec.ts", true},
		{"spec/models/user_spec.rb", true},
		{"src/test/java/com/x/UserTest.java", true},
		{"app/Http/UserTest.php", true},

		// A directory whose contents are tests by convention, whatever the
		// file inside it is called.
		{"tests/helpers.go", true},
		{"test/fixtures.js", true},
		{"src/__tests__/render.jsx", true},
		{"spec/spec_helper.rb", true},

		// conftest.py defines the fixtures every test around it depends on.
		// Gutting it disables them without touching a single case.
		{"tests/conftest.py", true},
		{"conftest.py", true},

		// Ordinary source must never be mistaken for a test: the finding only
		// means anything if it is about the files that do the judging.
		{"internal/scope/scope.go", false},
		{"src/components/Button.tsx", false},
		{"src/api/client.ts", false},
		{"README.md", false},
		{"", false},

		// Near misses. "latest/" is not "test/", and a file merely mentioning
		// the word is not a suite.
		{"src/latest/index.ts", false},
		{"src/contest/rules.go", false},
		{"docs/testing.md", false},

		// Path shape must not decide the answer.
		{"./internal/scope/scope_test.go", true},
		{"internal\\scope\\scope_test.go", true},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := IsTest(tt.path); got != tt.want {
				t.Errorf("IsTest(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestWeakened(t *testing.T) {
	tests := []struct {
		name     string
		numstat  map[string][2]int
		want     bool
		wantAdd  int
		wantDel  int
		wantFile int
	}{
		{
			// The case this exists for: assertions removed from a suite that
			// then exits zero.
			name:     "a test that shrank is the finding",
			numstat:  map[string][2]int{"pkg/thing_test.go": {0, 14}},
			want:     true,
			wantDel:  14,
			wantFile: 1,
		},
		{
			// Growth is the ordinary, good case, and must stay silent — a tool
			// that complains about tests being added trains people to ignore it.
			name:    "a test that grew is not",
			numstat: map[string][2]int{"pkg/thing_test.go": {40, 3}},
			want:    false,
		},
		{
			// Equal counts are a rewrite, not a subtraction. The rule is "more
			// left than entered", and this is neither.
			name:    "an even exchange is not",
			numstat: map[string][2]int{"pkg/thing_test.go": {9, 9}},
			want:    false,
		},
		{
			// Source code is not evidence about itself. Only the files that do
			// the judging count.
			name:    "shrinking ordinary source is not the finding",
			numstat: map[string][2]int{"pkg/thing.go": {0, 200}},
			want:    false,
		},
		{
			// Unrelated growth must not pay for a specific deletion: forty
			// lines gone from one suite are not restored by fifty added to
			// another, so the totals are taken over the files that shrank.
			name: "growth elsewhere does not offset a deletion",
			numstat: map[string][2]int{
				"pkg/a_test.go": {0, 40},
				"pkg/b_test.go": {50, 0},
			},
			want:     true,
			wantDel:  40,
			wantFile: 1,
		},
		{
			name: "several shrinking files are summed",
			numstat: map[string][2]int{
				"pkg/a_test.go": {2, 10},
				"pkg/b_test.go": {1, 5},
				"pkg/c.go":      {0, 99},
			},
			want:     true,
			wantAdd:  3,
			wantDel:  15,
			wantFile: 2,
		},
		{
			name:    "no changes at all",
			numstat: map[string][2]int{},
			want:    false,
		},
		{
			// A git failure hands back nil. The zero Delta must claim nothing
			// rather than read as "the tests are intact".
			name:    "a nil numstat is not a finding",
			numstat: nil,
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, got := Weakened(tt.numstat)
			if got != tt.want {
				t.Fatalf("Weakened() = %v, want %v", got, tt.want)
			}
			if !got {
				if len(d.Files) != 0 || d.Added != 0 || d.Deleted != 0 {
					t.Errorf("a non-finding carried data: %+v", d)
				}
				return
			}
			if len(d.Files) != tt.wantFile {
				t.Errorf("Files = %v, want %d of them", d.Files, tt.wantFile)
			}
			if tt.wantAdd != 0 && d.Added != tt.wantAdd {
				t.Errorf("Added = %d, want %d", d.Added, tt.wantAdd)
			}
			if d.Deleted != tt.wantDel {
				t.Errorf("Deleted = %d, want %d", d.Deleted, tt.wantDel)
			}
		})
	}
}

func TestWeakenedNamesTheFilesInAStableOrder(t *testing.T) {
	// The reason string ends up in a verdict, and a verdict that reorders
	// itself between identical runs cannot be diffed or trusted.
	numstat := map[string][2]int{
		"z_test.go": {0, 1}, "a_test.go": {0, 1}, "m_test.go": {0, 1},
	}
	for i := 0; i < 20; i++ {
		d, _ := Weakened(numstat)
		if got := strings.Join(d.Files, ","); got != "a_test.go,m_test.go,z_test.go" {
			t.Fatalf("Files = %q, want sorted", got)
		}
	}
}

func TestReasonReportsBothNumbers(t *testing.T) {
	// Either number alone invites the wrong reading. "Removed 40 lines" sounds
	// like vandalism until the 38 that replaced them are on the same line.
	d, ok := Weakened(map[string][2]int{"pkg/thing_test.go": {38, 40}})
	if !ok {
		t.Fatal("expected a finding")
	}
	r := d.Reason()
	for _, want := range []string{"40", "38", "pkg/thing_test.go"} {
		if !strings.Contains(r, want) {
			t.Errorf("Reason() = %q, missing %q", r, want)
		}
	}
}

func TestReasonCapsHowManyFilesItNames(t *testing.T) {
	// A verdict is read in a terminal. Naming forty files would push the
	// finding itself off the screen.
	numstat := map[string][2]int{}
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		numstat[n+"_test.go"] = [2]int{0, 1}
	}
	d, _ := Weakened(numstat)
	r := d.Reason()
	if !strings.Contains(r, "and 2 more") {
		t.Errorf("Reason() = %q, want the remainder counted", r)
	}
	if strings.Contains(r, "e_test.go") {
		t.Errorf("Reason() = %q, named more files than it should", r)
	}
}
