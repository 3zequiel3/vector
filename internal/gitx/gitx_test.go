package gitx

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func repo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return root
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commit(t *testing.T, root string) {
	t.Helper()
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", "x"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}

func TestNumstatCountsBothDirections(t *testing.T) {
	root := repo(t)
	write(t, root, "a.txt", "1\n2\n3\n4\n")
	commit(t, root)
	write(t, root, "a.txt", "1\n9\n")

	stats, err := Numstat(root, "")
	if err != nil {
		t.Fatalf("Numstat: %v", err)
	}
	got, ok := stats["a.txt"]
	if !ok {
		t.Fatalf("a.txt missing from %v", stats)
	}
	// One line added, three removed: the direction is the whole point, so
	// both columns have to survive the parse.
	if got != [2]int{1, 3} {
		t.Errorf("a.txt = %v, want [1 3]", got)
	}
}

func TestNumstatReportsADeletedFileAsAllRemovals(t *testing.T) {
	// A suite deleted outright is the loudest version of the failure this
	// feeds, and it must not fall out of the map.
	root := repo(t)
	write(t, root, "gone.txt", "a\nb\nc\n")
	commit(t, root)
	if err := os.Remove(filepath.Join(root, "gone.txt")); err != nil {
		t.Fatal(err)
	}

	stats, err := Numstat(root, "")
	if err != nil {
		t.Fatalf("Numstat: %v", err)
	}
	if got := stats["gone.txt"]; got != [2]int{0, 3} {
		t.Errorf("gone.txt = %v, want [0 3]", got)
	}
}

func TestNumstatRecordsBinaryCountsAsZero(t *testing.T) {
	// git reports "-" for binary content. Inventing a line count to fill the
	// column would put a fabricated number into a verdict, and one unparseable
	// file must not cost the text files beside it.
	root := repo(t)
	write(t, root, "blob.bin", "\x00\x01\x02")
	write(t, root, "a.txt", "1\n2\n")
	commit(t, root)
	write(t, root, "blob.bin", "\x00\x09\x09\x09")
	write(t, root, "a.txt", "1\n")

	stats, err := Numstat(root, "")
	if err != nil {
		t.Fatalf("Numstat: %v", err)
	}
	if got := stats["blob.bin"]; got != [2]int{0, 0} {
		t.Errorf("blob.bin = %v, want [0 0]", got)
	}
	if got := stats["a.txt"]; got != [2]int{0, 1} {
		t.Errorf("a.txt = %v, want [0 1] — a binary neighbour cost it its count", got)
	}
}

func TestNumstatOnARepositoryWithNoCommits(t *testing.T) {
	// There is no HEAD to diff against. Nothing can have been removed yet, so
	// the honest answer is an empty result rather than an error that would
	// stop a verdict.
	root := repo(t)
	write(t, root, "a.txt", "1\n")

	stats, err := Numstat(root, "")
	if err != nil {
		t.Fatalf("Numstat: %v", err)
	}
	if len(stats) != 0 {
		t.Errorf("stats = %v, want empty before the first commit", stats)
	}
}

func TestNumstatIgnoresUntrackedFiles(t *testing.T) {
	// git has nothing to diff a new file against, and a file that did not
	// exist cannot have had anything removed from it.
	root := repo(t)
	write(t, root, "a.txt", "1\n")
	commit(t, root)
	write(t, root, "brand_new.txt", "1\n2\n3\n")

	stats, err := Numstat(root, "")
	if err != nil {
		t.Fatalf("Numstat: %v", err)
	}
	if _, ok := stats["brand_new.txt"]; ok {
		t.Errorf("an untracked file appeared in %v", stats)
	}
}

func TestNumstatTreatsARenameAsNoChange(t *testing.T) {
	// A renamed test file moved byte for byte has had nothing removed from
	// it. With rename detection off git reports the old path as a full
	// deletion, and a caller asking "what did this change remove" would be
	// told a lie about a `git mv`.
	root := repo(t)
	write(t, root, "a_test.go", "1\n2\n3\n4\n5\n")
	commit(t, root)

	cmd := exec.Command("git", "mv", "a_test.go", "b_test.go")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git mv: %v: %s", err, out)
	}

	stats, err := Numstat(root, "")
	if err != nil {
		t.Fatalf("Numstat: %v", err)
	}
	if _, ok := stats["a_test.go"]; ok {
		t.Errorf("the old path was reported as a deletion: %v", stats)
	}
	if got := stats["b_test.go"]; got != [2]int{0, 0} {
		t.Errorf("b_test.go = %v, want [0 0] for a pure rename", got)
	}
}

func TestNumstatCountsAnEditedRenameAgainstItsNewPath(t *testing.T) {
	// A rename that also edits is a real change, and it belongs to the path
	// that exists now — that is the name every later question will use.
	//
	// The edit is kept small on purpose. Rename detection is a similarity
	// test, and git stops calling it a rename below roughly half: a file that
	// moved and then lost most of its content is reported as a deletion plus
	// an addition, and being told that content went missing is the correct
	// answer for a change that did remove it.
	root := repo(t)
	write(t, root, "a_test.go", "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n")
	commit(t, root)
	cmd := exec.Command("git", "mv", "a_test.go", "b_test.go")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git mv: %v: %s", err, out)
	}
	write(t, root, "b_test.go", "1\n2\n3\n4\n5\n6\n7\n8\n9\n")

	stats, err := Numstat(root, "")
	if err != nil {
		t.Fatalf("Numstat: %v", err)
	}
	if _, ok := stats["a_test.go"]; ok {
		t.Errorf("the old path is still in %v", stats)
	}
	if got := stats["b_test.go"]; got != [2]int{0, 1} {
		t.Errorf("b_test.go = %v, want [0 1] against its new path", got)
	}
}

func TestNumstatReturnsUnquotedNonASCIIPaths(t *testing.T) {
	// Under git's default core.quotePath a non-ASCII path comes back
	// C-quoted and octal-escaped — "t\303\253st_test.go" — and a caller
	// matching on the end of that string is matching on a trailing quote.
	// The path must arrive as the bytes it actually is.
	root := repo(t)
	cmd := exec.Command("git", "config", "core.quotePath", "true")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git config: %v: %s", err, out)
	}
	write(t, root, "ñandú_test.go", "1\n2\n3\n")
	commit(t, root)
	write(t, root, "ñandú_test.go", "1\n")

	stats, err := Numstat(root, "")
	if err != nil {
		t.Fatalf("Numstat: %v", err)
	}
	if got := stats["ñandú_test.go"]; got != [2]int{0, 2} {
		t.Errorf("ñandú_test.go = %v (map: %v), want [0 2] under its real name", got, stats)
	}
}

func TestNumstatHandlesAPathWithASpace(t *testing.T) {
	// -z makes the separator NUL, so a path may contain anything but NUL.
	root := repo(t)
	write(t, root, "a name_test.go", "1\n2\n")
	commit(t, root)
	write(t, root, "a name_test.go", "1\n")

	stats, err := Numstat(root, "")
	if err != nil {
		t.Fatalf("Numstat: %v", err)
	}
	if got := stats["a name_test.go"]; got != [2]int{0, 1} {
		t.Errorf("path with a space = %v, want [0 1]", got)
	}
}

func TestNumstatAgainstAnExplicitBase(t *testing.T) {
	// The CI case: the working tree is clean and identical to HEAD, so a diff
	// against HEAD is empty however much the branch removed. Only a base ref
	// can see it.
	root := repo(t)
	write(t, root, "a_test.go", "1\n2\n3\n4\n")
	commit(t, root)

	cmd := exec.Command("git", "branch", "base")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git branch: %v: %s", err, out)
	}
	write(t, root, "a_test.go", "1\n")
	commit(t, root)

	if stats, err := Numstat(root, ""); err != nil || len(stats) != 0 {
		t.Errorf("Numstat(HEAD) = %v (err %v), want empty on a clean tree", stats, err)
	}
	stats, err := Numstat(root, "base")
	if err != nil {
		t.Fatalf("Numstat(base): %v", err)
	}
	if got := stats["a_test.go"]; got != [2]int{0, 3} {
		t.Errorf("a_test.go against base = %v, want [0 3]", got)
	}
}

// quotePath turns on the setting that produced the bug these tests pin.
//
// It is git's default, but a test that relies on a default proves nothing the
// day someone's environment changes it: the test would pass while checking
// nothing. Setting it explicitly is what makes the assertion mean something.
func quotePath(t *testing.T, root string) {
	t.Helper()
	cmd := exec.Command("git", "config", "core.quotePath", "true")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git config: %v: %s", err, out)
	}
}

func has(files []string, want string) bool {
	for _, f := range files {
		if f == want {
			return true
		}
	}
	return false
}

func TestChangedFilesReturnsNonASCIIPathsUnquoted(t *testing.T) {
	// A quoted path is a different string, and a different string matches
	// different patterns. Left quoted, a file inside the declared boundary is
	// reported as out of scope — and under strict enforcement, denied. That is
	// the tool refusing legitimate work, which is worse than not checking.
	root := repo(t)
	quotePath(t, root)
	write(t, root, "src/año/índice.ts", "1\n")
	commit(t, root)
	write(t, root, "src/año/índice.ts", "1\n2\n")

	files, err := ChangedFiles(root, "")
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	if !has(files, "src/año/índice.ts") {
		t.Errorf("files = %v, want the path under its real name", files)
	}
}

func TestChangedFilesReturnsUntrackedNonASCIIPathsUnquoted(t *testing.T) {
	// The untracked half runs a different git command, and had the same bug.
	// A brand-new file outside the boundary is exactly the drift vector exists
	// to catch, so this half must not be the one that misses it.
	root := repo(t)
	quotePath(t, root)
	write(t, root, "keep.txt", "x\n")
	commit(t, root)
	write(t, root, "src/año/nuevo.ts", "1\n")

	files, err := ChangedFiles(root, "")
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	if !has(files, "src/año/nuevo.ts") {
		t.Errorf("files = %v, want the untracked path under its real name", files)
	}
}

func TestAllFilesReturnsNonASCIIPathsUnquoted(t *testing.T) {
	// doctor uses AllFiles to warn about scope patterns that match nothing.
	// Quoted paths would make it report a working pattern as a probable typo.
	root := repo(t)
	quotePath(t, root)
	write(t, root, "src/año/índice.ts", "1\n")
	commit(t, root)

	files, err := AllFiles(root)
	if err != nil {
		t.Fatalf("AllFiles: %v", err)
	}
	if !has(files, "src/año/índice.ts") {
		t.Errorf("files = %v, want the path under its real name", files)
	}
}

func TestChangedFilesKeepsAPathWithSurroundingSpace(t *testing.T) {
	// Under -z each record is already exactly the path. Trimming it — which is
	// what the newline-separated version had to do — would rename a legal file
	// into one that matches no pattern.
	root := repo(t)
	write(t, root, " leading.txt", "1\n")
	commit(t, root)
	write(t, root, " leading.txt", "1\n2\n")

	files, err := ChangedFiles(root, "")
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	if !has(files, " leading.txt") {
		t.Errorf("files = %v, want the leading space preserved", files)
	}
}

func TestChangedFilesStillSeesOrdinaryPaths(t *testing.T) {
	// The mirror case: changing the separator must not lose the common one.
	root := repo(t)
	write(t, root, "a.txt", "1\n")
	write(t, root, "dir/b.txt", "1\n")
	commit(t, root)
	write(t, root, "a.txt", "2\n")
	write(t, root, "untracked.txt", "1\n")

	files, err := ChangedFiles(root, "")
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	for _, want := range []string{"a.txt", "untracked.txt"} {
		if !has(files, want) {
			t.Errorf("files = %v, missing %q", files, want)
		}
	}
	if has(files, "dir/b.txt") {
		t.Errorf("files = %v, contains an unchanged file", files)
	}
}
