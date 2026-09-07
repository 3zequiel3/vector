// Package freshness answers whether the last verdict is still about the tree
// in front of you.
//
// `vector verify` can answer VERIFIED, and five edits later that answer is
// still the last thing anyone was told — but it has quietly become a claim
// about a tree that no longer exists. A verdict with no expiry is exactly the
// false confidence this tool exists to refuse.
//
// The answer is a comparison of git blob hashes: the content of every in-scope
// file at the moment a verdict was reached, against the same files now. That
// makes every answer nameable — "Filter.tsx changed since it was verified" —
// rather than a feeling that something moved. There is no fingerprint scheme
// of its own here; the identifier is the one git already uses for file content,
// so vector keeps working with git and nothing else.
//
// Nothing in this package blocks, denies, or changes an exit code. Stale is
// not failed. It is unknown, and the vocabulary already has a word for that:
// what sits on disk after an unverified edit is UNVERIFIED.
package freshness

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// maxBytes caps the state file. The Stop hook reads it whole, once per
	// turn, so the cap is set where that read stays trivial. A snapshot is
	// bounded by the size of a change rather than the size of a repository —
	// in-scope means "changed and inside the boundary" — so this is reached
	// only by genuinely enormous work.
	maxBytes = 256 << 10

	// maxTasks is how many tasks keep a verdict. One verdict per task is the
	// whole ambition here; counting how a task got where it is belongs to
	// internal/attempt, which already does it properly.
	maxTasks = 8

	// maxNamed is how many paths a message spells out before summarising the
	// rest. Naming forty files is the same as naming none: nobody reads it,
	// and the one that matters is buried.
	maxNamed = 5
)

// Snapshot is what a verdict was about: the task, the answer, and the content
// of every file that was in scope when that answer was reached.
type Snapshot struct {
	Task    string    `json:"task"`
	Verdict string    `json:"verdict"`
	Passed  bool      `json:"passed"`
	At      time.Time `json:"at"`
	// Files maps a repo-relative path to its git blob hash. Untracked files
	// belong here too: a file created during the work is part of what was
	// verified, and leaving it out would make both its later edits and its
	// later deletion invisible.
	Files map[string]string `json:"files"`
}

// state is the whole file. Snapshots are oldest first.
type state struct {
	Schema    string     `json:"schema"`
	Snapshots []Snapshot `json:"snapshots"`
}

const schema = "vector.freshness/v1"

// Path returns the verdict state file for a repository.
//
// It sits beside `current`, `nudged` and `attempts` as per-developer working
// state. Two people on the same branch verify different trees at different
// moments, and committing one person's snapshot would report their staleness
// as the other's.
func Path(root string) string {
	return filepath.Join(root, ".vector", "verdicts")
}

// Hash returns the git blob hash of each path that could be read, keyed by the
// repo-relative path it was given.
//
// A path that is missing, unreadable, or a directory is simply absent from the
// result. "Gone" is a real answer to this question, and Compare reads it as
// one rather than as an error.
//
// The hash is computed in process rather than by shelling out to
// `git hash-object`. It is the same value — SHA-1 over "blob <size>\0" and the
// file's bytes, which TestTheHashIsTheOneGitWouldGive pins against git itself
// — and the measurement was not close. Over 200 files: one batched
// `git hash-object` invocation 4.8ms, 200 separate invocations 421ms, in
// process 2.1ms. Batching is the only shape of the subprocess worth having,
// and it is also the one with an argv limit that a large change would
// eventually find. SHA-1 is an identity here, not a security boundary: it
// answers "are these the same bytes git would call the same blob".
func Hash(root string, paths []string) map[string]string {
	out := make(map[string]string, len(paths))
	for _, p := range paths {
		if h, ok := blob(filepath.Join(root, filepath.FromSlash(p))); ok {
			out[p] = h
		}
	}
	return out
}

// blob computes one git blob hash, streaming so that an enormous in-scope file
// cannot make vector fall over on it.
//
// The size is read once, up front, the way git does. A file being rewritten
// underneath this read yields a short copy and is reported as unreadable
// rather than as a hash of half a file: a wrong hash would be a claim, and the
// absence of one is only a question.
func blob(abs string) (string, bool) {
	f, err := os.Open(abs)
	if err != nil {
		return "", false
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.IsDir() {
		return "", false
	}
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", info.Size())
	n, err := io.Copy(h, f)
	if err != nil || n != info.Size() {
		return "", false
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

// Record stores the tree one verdict was about, replacing whatever was known
// about that task before.
//
// A verdict supersedes the last one for the same task, so there is exactly one
// record per task and no history to prune. That is deliberate: a verdict two
// edits ago is not evidence about anything, and the shape of a task over time
// is already answered, honestly and separately, by internal/attempt.
//
// Two sessions recording at once resolve to last-writer-wins. That costs a
// forgotten snapshot, whose only consequence is a reminder nobody receives —
// the direction in which this feature is allowed to fail.
func Record(root string, s Snapshot) error {
	s.Task = strings.TrimSpace(s.Task)
	if s.Task == "" {
		return fmt.Errorf("a snapshot with no task belongs to nothing")
	}
	if s.At.IsZero() {
		s.At = time.Now()
	}
	if s.Files == nil {
		s.Files = map[string]string{}
	}
	if err := os.MkdirAll(filepath.Join(root, ".vector"), 0o755); err != nil {
		return err
	}

	kept := make([]Snapshot, 0, maxTasks)
	for _, old := range load(root).Snapshots {
		if old.Task != s.Task {
			kept = append(kept, old)
		}
	}
	data, err := json.MarshalIndent(state{Schema: schema, Snapshots: trim(append(kept, s))}, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(Path(root), append(data, '\n'))
}

// trim keeps the file bounded.
//
// The snapshot just written is never the one dropped — it is the one the next
// turn will ask about — and the oldest goes first, because a verdict nobody
// has revisited in eight tasks is not the one anyone is about to be misled by.
func trim(all []Snapshot) []Snapshot {
	if len(all) > maxTasks {
		all = all[len(all)-maxTasks:]
	}
	for len(all) > 1 && encodedSize(all) > maxBytes {
		all = all[1:]
	}
	return all
}

func encodedSize(all []Snapshot) int {
	data, err := json.Marshal(state{Schema: schema, Snapshots: all})
	if err != nil {
		return 0
	}
	return len(data)
}

// Last returns the verdict recorded for a task, or false when there is none.
//
// Every way of failing to read — missing, unreadable, truncated, holding
// something that is not a snapshot at all — is "no prior verdict". None of
// them is a condition a caller can act on, and an error escaping into verify
// or the Stop hook would cost the user their turn over bookkeeping.
func Last(root, task string) (Snapshot, bool) {
	task = strings.TrimSpace(task)
	if task == "" {
		return Snapshot{}, false
	}
	for _, s := range load(root).Snapshots {
		if s.Task == task {
			return s, true
		}
	}
	return Snapshot{}, false
}

func load(root string) state {
	data, err := os.ReadFile(Path(root))
	if err != nil {
		return state{}
	}
	var st state
	if err := json.Unmarshal(data, &st); err != nil {
		return state{}
	}
	// A record with no task cannot be looked up and cannot be superseded, so
	// it is dropped rather than carried forward forever.
	kept := st.Snapshots[:0]
	for _, s := range st.Snapshots {
		if strings.TrimSpace(s.Task) != "" {
			kept = append(kept, s)
		}
	}
	st.Snapshots = kept
	return st
}

// Staleness is the difference between the tree a verdict was about and the
// tree now, in the only terms that can be checked without guessing: which
// files moved, and how.
type Staleness struct {
	Snapshot
	Changed []string
	Added   []string
	Removed []string
}

// Stale reports whether anything moved at all.
func (s Staleness) Stale() bool {
	return len(s.Changed)+len(s.Added)+len(s.Removed) > 0
}

// Compare answers "is this verdict still about the current tree", naming every
// file that moved.
//
// inScope is what is in scope right now, taken from an audit the caller has
// already run. It is consulted for one thing only: files that did not exist
// when the verdict was reached. A new file inside the boundary is part of the
// work and was never verified, so it moves the tree just as surely as an edit
// to an old one. Paths outside the boundary are deliberately not consulted —
// drift is the audit's finding, and reporting it here as well would give the
// same file two names in one message.
func Compare(root string, s Snapshot, inScope []string) Staleness {
	st := Staleness{Snapshot: s}

	// One hashing pass over the union: what the verdict covered, plus whatever
	// has appeared inside the boundary since.
	paths := make([]string, 0, len(s.Files)+len(inScope))
	for p := range s.Files {
		paths = append(paths, p)
	}
	fresh := make([]string, 0, len(inScope))
	seen := map[string]bool{}
	for _, p := range inScope {
		if _, verified := s.Files[p]; verified || seen[p] {
			continue
		}
		seen[p] = true
		fresh = append(fresh, p)
		paths = append(paths, p)
	}
	now := Hash(root, paths)

	for p, was := range s.Files {
		is, exists := now[p]
		switch {
		case !exists:
			st.Removed = append(st.Removed, p)
		case is != was:
			st.Changed = append(st.Changed, p)
		}
	}
	for _, p := range fresh {
		// A path that cannot be read is not evidence of an addition; it is a
		// path vector could not look at, and saying nothing about it is the
		// only claim it can support.
		if _, exists := now[p]; exists {
			st.Added = append(st.Added, p)
		}
	}

	// Sorted so the same movement always reads the same way, whatever order
	// git or a map iteration happened to produce.
	sort.Strings(st.Changed)
	sort.Strings(st.Added)
	sort.Strings(st.Removed)
	return st
}

// Message is what a person or an agent reads, or "" when nothing moved.
//
// It names the files. "Something changed" is precisely the unfalsifiable
// answer this tool refuses to give, and a reader who has to go find out for
// themselves has been told nothing. It also says outright that nothing is
// blocked, because a message that reads like a refusal gets treated as one.
func (s Staleness) Message() string {
	if !s.Stale() {
		return ""
	}
	verdict := s.Verdict
	if verdict == "" {
		verdict = "last"
	}

	var moved []string
	if len(s.Changed) > 0 {
		moved = append(moved, names(s.Changed)+" changed")
	}
	if len(s.Added) > 0 {
		moved = append(moved, names(s.Added)+" "+was(len(s.Added))+" added")
	}
	if len(s.Removed) > 0 {
		moved = append(moved, names(s.Removed)+" "+was(len(s.Removed))+" deleted")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "vector: the %s verdict for %s is stale — %s since it was reached",
		verdict, s.Task, list(moved))
	if !s.At.IsZero() {
		fmt.Fprintf(&b, " (%s)", s.At.UTC().Format(time.RFC3339))
	}
	b.WriteString(". What is on disk now is UNVERIFIED, not failed: nothing is blocked " +
		"and no exit code changed. Run `vector verify` for an answer about the tree in front of you.")
	return b.String()
}

// names spells out the paths, up to the point where a longer list stops being
// read at all.
func names(paths []string) string {
	if len(paths) <= maxNamed {
		return strings.Join(paths, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(paths[:maxNamed], ", "), len(paths)-maxNamed)
}

func was(n int) string {
	if n == 1 {
		return "was"
	}
	return "were"
}

func list(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	default:
		return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
	}
}

// writeAtomic writes via a temporary file and a rename, so a crash cannot
// leave half a snapshot behind for the next turn to misread.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".vector-verdicts-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
