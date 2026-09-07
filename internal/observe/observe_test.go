package observe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecordAssignsSequentialIDs(t *testing.T) {
	root := t.TempDir()
	for i, want := range []string{"OBS-001", "OBS-002", "OBS-003"} {
		o, err := Record(root, Observation{Note: "note", Category: "other"})
		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		if o.ID != want {
			t.Errorf("id = %q, want %q", o.ID, want)
		}
	}
}

func TestIDsSurviveAHandEditedFile(t *testing.T) {
	// Ids are derived from the file rather than a counter, so a merge, a
	// revert, or someone editing by hand cannot cause a collision.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".vector"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(root),
		[]byte("# Observations\n\n## OBS-007\n- category: other\n\nsomething\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	o, err := Record(root, Observation{Note: "next"})
	if err != nil {
		t.Fatal(err)
	}
	if o.ID != "OBS-008" {
		t.Errorf("id = %q, want OBS-008 derived from the highest id present", o.ID)
	}
}

func TestRecordIsAppendOnly(t *testing.T) {
	root := t.TempDir()
	if _, err := Record(root, Observation{Note: "first"}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(Path(root))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Record(root, Observation{Note: "second"}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(Path(root))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(after), string(before)) {
		t.Error("the file is not append-only: earlier content changed")
	}
}

func TestEveryObservationIsDeferred(t *testing.T) {
	// The action is not a parameter. Recording something is the whole of the
	// permitted response to noticing it.
	root := t.TempDir()
	if _, err := Record(root, Observation{Note: "the auth architecture is weak"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(Path(root))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "- action: defer") {
		t.Error("an observation was recorded without action: defer")
	}
}

func TestRejectsEmptyAndInvalidInput(t *testing.T) {
	root := t.TempDir()
	if _, err := Record(root, Observation{Note: "   "}); err == nil {
		t.Error("an empty note was accepted")
	}
	if _, err := Record(root, Observation{Note: "x", Severity: "catastrophic"}); err == nil {
		t.Error("an invented severity was accepted")
	}
	if _, err := Record(root, Observation{Note: "x", Category: "vibes"}); err == nil {
		t.Error("an invented category was accepted")
	}
}

func TestListRoundTrips(t *testing.T) {
	root := t.TempDir()
	in := Observation{
		Note: "The middleware mixes session and token.", Task: "add-filter",
		Category: "architecture", Severity: "medium",
	}
	if _, err := Record(root, in); err != nil {
		t.Fatal(err)
	}
	if _, err := Record(root, Observation{Note: "another", Severity: "low"}); err != nil {
		t.Fatal(err)
	}

	got, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	first := got[0]
	if first.ID != "OBS-001" || first.Task != "add-filter" ||
		first.Category != "architecture" || first.Severity != "medium" {
		t.Errorf("got %+v, want the recorded fields preserved", first)
	}
	if first.Note != in.Note {
		t.Errorf("note = %q, want %q", first.Note, in.Note)
	}
}

func TestListOnAMissingFile(t *testing.T) {
	got, err := List(t.TempDir())
	if err != nil {
		t.Fatalf("an empty list is not an error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}
