// Package suite answers one question about a change: did it subtract from the
// tests that verify it?
//
// `vector verify` judges a change by running the project's own test command and
// reading its exit code. That is a sound way to be told something works, and it
// has one blind spot large enough to drive the failure through: the change
// being judged can edit its own judge. A suite with its assertions removed exits
// zero. A suite with its cases skipped exits zero. A suite deleted outright
// exits zero, loudly and in green.
//
// This is not a hypothetical. METR ran o3 inside RE-Bench 128 times and observed
// explicit reward hacking in 39 of them — 30.4%, with nobody asking for it — and
// the catalogued mechanisms are exactly these: overwriting tests, deleting
// assertions, terminating early. The measurement is external and the citation
// belongs in the docs; what belongs here is the consequence. An exit code is
// evidence about the code the suite was pointed at, and it stops being evidence
// about anything when the same change moved the suite.
//
// # What this does not do
//
// It does not read code. There is no parser here, no assertion counter, no
// notion of what a test means — those are per-language, they rot, and the first
// one to disagree with a project's idiom would make vector wrong in a way it
// could not explain. What this reads is `git diff --numstat`: how many lines a
// change added and removed in the files that are the project's tests. That is a
// fact git already computed.
//
// It also does not accuse. Removing lines from a test is ordinary — suites get
// consolidated, cases get replaced, a task whose whole purpose is to fix a test
// will subtract from it. Nothing here blocks, denies, or changes an exit code
// on its own. It lowers a claim: a passing suite that this change shrank does
// not support VERIFIED, and the vocabulary already has the word for a result
// that is partly established. The numbers are reported so the reader can decide
// in one glance what vector deliberately declines to decide for them.
package suite

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// Delta is what a change did to the files that test it.
type Delta struct {
	// Files are the test files the change subtracted from, sorted. A file
	// that grew is not listed: it is not the finding.
	Files []string
	// Added and Deleted are line counts across those files only. Both are
	// reported because either one alone invites the wrong reading — "removed
	// 40 lines" sounds like vandalism until you see the 38 that replaced them.
	Added, Deleted int
}

// Weakened reports whether a change is net-subtractive across the project's
// test files, and by how much.
//
// The rule is the simplest one that can be stated without a magic number: more
// lines left the tests than entered them. A threshold would be a heuristic
// vector could not justify — there is no line count at which removing test code
// becomes suspicious, only the fact that this change did it while claiming the
// tests passed.
//
// It is measured per file and summed over the files that shrank. A change that
// deletes forty lines from one test and adds fifty to another has not restored
// what it removed from the first; treating the totals as one number would let
// unrelated growth pay for a specific deletion.
//
// Within one file that offsetting is exactly what happens, and it is the way
// this check is evaded: delete three assertions, add fifty lines of anything,
// and the file grew. Saying so plainly is better than implying a guarantee
// that is not here. Closing it would mean counting assertions, which means
// parsing every language — the cost this package exists to refuse. What is
// left is still worth having: it catches the change that only subtracts, which
// is what the catalogued mechanisms actually look like, and it costs one git
// command that has already run.
func Weakened(numstat map[string][2]int) (Delta, bool) {
	var d Delta
	for p, n := range numstat {
		if !IsTest(p) {
			continue
		}
		added, deleted := n[0], n[1]
		if deleted <= added {
			continue
		}
		d.Files = append(d.Files, p)
		d.Added += added
		d.Deleted += deleted
	}
	if len(d.Files) == 0 {
		return Delta{}, false
	}
	sort.Strings(d.Files)
	return d, true
}

// Reason is the sentence appended to a verdict.
//
// It states the measurement and stops. Vector cannot tell a suite being
// consolidated from a suite being gutted, and a tool that guesses at intent
// here would be wrong on the ordinary case — which is most cases, and the ones
// where being wrong costs it the reader.
func (d Delta) Reason() string {
	var b strings.Builder
	fmt.Fprintf(&b, "but this change removed %d line(s) from %s and added %d",
		d.Deleted, plural(len(d.Files), "test file", "test files"), d.Added)
	const maxNamed = 3
	names := d.Files
	if len(names) > maxNamed {
		names = names[:maxNamed]
	}
	fmt.Fprintf(&b, " (%s", strings.Join(names, ", "))
	if rest := len(d.Files) - len(names); rest > 0 {
		fmt.Fprintf(&b, " and %d more", rest)
	}
	b.WriteString("), so the suite that passed is not the suite that was there")
	return b.String()
}

// testDirs are path segments whose contents are tests by convention.
var testDirs = map[string]bool{
	"test": true, "tests": true, "__tests__": true, "spec": true, "specs": true,
}

// testSuffixes are filename endings that name a test file across the
// ecosystems vector already detects.
var testSuffixes = []string{
	"_test.go",                          // Go
	"_test.py", "_test.rb", "_test.php", // suffix-style
	"_spec.rb", "_spec.js", "_spec.ts", // rspec and friends
	"Test.java", "Tests.java", "Test.kt", // JVM
	"Test.php", "Tests.cs", "Test.cs", // PHP, .NET
	".test.js", ".test.ts", ".test.jsx", // JS/TS
	".test.tsx", ".test.mjs", ".test.cjs", //
	".spec.js", ".spec.ts", ".spec.jsx", //
	".spec.tsx", ".spec.mjs", ".spec.cjs", //
}

// testPrefixes are filename beginnings that name a test file.
var testPrefixes = []string{
	"test_", // pytest, and unittest by convention
}

// IsTest reports whether a repository-relative path is one of the project's
// test files.
//
// This is a path judgement, deliberately. Reading the file to decide would mean
// parsing every language vector supports, and being wrong about one of them in
// a way no rule could name.
//
// It is knowingly incomplete, and the incompleteness only ever costs a finding,
// never invents one. Rust's `#[cfg(test)] mod tests` lives inside the file it
// tests and cannot be seen from the path at all; a project with its own naming
// convention will not be recognised. In both cases vector says nothing, which
// is what it should say about something it did not check — VERIFIED never
// claimed the tests were intact, and it still does not.
func IsTest(p string) bool {
	p = strings.TrimSpace(p)
	if p == "" {
		return false
	}
	p = strings.TrimPrefix(path.Clean(strings.ReplaceAll(p, "\\", "/")), "./")

	for _, seg := range strings.Split(path.Dir(p), "/") {
		if testDirs[strings.ToLower(seg)] {
			return true
		}
	}

	base := path.Base(p)
	for _, s := range testSuffixes {
		if strings.HasSuffix(base, s) {
			return true
		}
	}
	lower := strings.ToLower(base)
	for _, pre := range testPrefixes {
		if strings.HasPrefix(lower, pre) {
			return true
		}
	}
	// conftest.py is not a test, but every test in its directory depends on the
	// fixtures it defines. Gutting it disables them without touching a case.
	return lower == "conftest.py"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
