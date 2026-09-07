package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/3zequiel3/vector/internal/observe"
)

// repo builds a real git repository with a declared, active task.
//
// The command layer resolves paths, reads the active task and writes files;
// stubbing any of that would test the stub. Everything here is what a user's
// repository actually looks like when the command runs.
func repo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	cmd := exec.Command("git", "init", "-q", "-b", "main")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	mk(t, root, ".vector/scope/live.toml", "objective = \"o\"\nwrite = [\"src/**\"]\n")
	mk(t, root, ".vector/current", "live\n")
	return root
}

func mk(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func only(t *testing.T, root string) observe.Observation {
	t.Helper()
	obs, err := observe.List(root)
	if err != nil {
		t.Fatalf("observe.List: %v", err)
	}
	if len(obs) != 1 {
		t.Fatalf("recorded %d observations, want 1", len(obs))
	}
	return obs[0]
}

func TestObserveFilesAgainstTheActiveTask(t *testing.T) {
	// Nobody passes -task by hand. The agent recording a note is mid-work and
	// the active task is already on disk, so without the fallback every
	// observation is filed against nothing and no later session can ask what
	// this task noticed and deliberately left alone.
	root := repo(t)

	if code := runObserve([]string{"auth mixes session and token", "-C", root}); code != 0 {
		t.Fatalf("runObserve exited %d", code)
	}
	if got := only(t, root).Task; got != "live" {
		t.Errorf("task = %q, want the active task", got)
	}
}

func TestAnExplicitTaskBeatsTheActiveOne(t *testing.T) {
	// The fallback fills a gap; it must never overrule someone who said which
	// task they meant.
	root := repo(t)

	if code := runObserve([]string{"note", "-task", "something-else", "-C", root}); code != 0 {
		t.Fatalf("runObserve exited %d", code)
	}
	if got := only(t, root).Task; got != "something-else" {
		t.Errorf("task = %q, want the one that was passed", got)
	}
}

func TestObserveWithNoActiveTaskStillRecords(t *testing.T) {
	// A note taken before any scope is declared is still worth keeping. The
	// fallback has nothing to supply, and that must not turn into a refusal.
	root := t.TempDir()
	cmd := exec.Command("git", "init", "-q", "-b", "main")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	if code := runObserve([]string{"noticed before any scope", "-C", root}); code != 0 {
		t.Fatalf("runObserve exited %d", code)
	}
	if got := only(t, root).Task; got != "" {
		t.Errorf("task = %q, want it left empty", got)
	}
}

func TestObserveKeepsRejectingAnInventedCategory(t *testing.T) {
	// The fallback runs before Record validates. A note that should have been
	// refused must still be refused, and must not reach the log.
	root := repo(t)

	if code := runObserve([]string{"note", "-category", "vibes", "-C", root}); code == 0 {
		t.Error("an out-of-vocabulary category was accepted")
	}
	if obs, err := observe.List(root); err == nil && len(obs) != 0 {
		t.Errorf("a rejected observation was still recorded: %+v", obs)
	}
}
