// Package observe records what an agent noticed without letting it act.
//
// The failure this exists to break is a single implicit step: "I found a
// problem" becoming "therefore I will fix it". An observation is a record, not
// a task. Writing one down is the whole of the permitted response.
//
// The file is append-only markdown in git, so observations survive the session,
// stay reviewable by a person, and never become a database of complaints that
// nobody reads.
package observe

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Observation is one thing noticed and deliberately not acted on.
type Observation struct {
	ID       string
	Date     string
	Task     string
	Category string
	Severity string
	Note     string
}

// Severities and categories are constrained so the file stays scannable. They
// are labels for a human triaging later, not an ontology.
var (
	Severities = []string{"low", "medium", "high"}
	Categories = []string{"architecture", "security", "performance", "correctness", "maintainability", "other"}
)

const header = `# Observations

Append-only log. An observation is NOT a task.

Finding a problem does not authorize fixing it. This stays here until a person
decides otherwise.
`

// Path returns the observations file for a repository.
func Path(root string) string {
	return filepath.Join(root, ".vector", "observations.md")
}

var idPattern = regexp.MustCompile(`^## (OBS-(\d+))\s*$`)

// nextID scans the existing file for the highest recorded id. Reading the
// file rather than keeping a counter means the ids stay correct after a merge,
// a revert, or someone editing the file by hand.
func nextID(path string) (string, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return "OBS-001", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()

	highest := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m := idPattern.FindStringSubmatch(sc.Text()); m != nil {
			if n, err := strconv.Atoi(m[2]); err == nil && n > highest {
				highest = n
			}
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return fmt.Sprintf("OBS-%03d", highest+1), nil
}

// Record appends an observation and returns it. It never modifies an existing
// entry: the file only ever grows.
func Record(root string, o Observation) (Observation, error) {
	if strings.TrimSpace(o.Note) == "" {
		return Observation{}, fmt.Errorf("an empty observation records nothing")
	}
	if o.Severity == "" {
		o.Severity = "medium"
	}
	if o.Category == "" {
		o.Category = "other"
	}
	if !contains(Severities, o.Severity) {
		return Observation{}, fmt.Errorf("severity %q; use one of: %s",
			o.Severity, strings.Join(Severities, ", "))
	}
	if !contains(Categories, o.Category) {
		return Observation{}, fmt.Errorf("category %q; use one of: %s",
			o.Category, strings.Join(Categories, ", "))
	}

	path := Path(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Observation{}, err
	}

	id, err := nextID(path)
	if err != nil {
		return Observation{}, err
	}
	o.ID = id
	o.Date = time.Now().Format("2006-01-02")

	var b strings.Builder
	if _, err := os.Stat(path); os.IsNotExist(err) {
		b.WriteString(header)
	}
	fmt.Fprintf(&b, "\n## %s\n", o.ID)
	fmt.Fprintf(&b, "- date: %s\n", o.Date)
	if o.Task != "" {
		fmt.Fprintf(&b, "- task: %s\n", o.Task)
	}
	fmt.Fprintf(&b, "- category: %s\n", o.Category)
	fmt.Fprintf(&b, "- severity: %s\n", o.Severity)
	fmt.Fprintf(&b, "- action: defer\n\n")
	fmt.Fprintf(&b, "%s\n", strings.TrimSpace(o.Note))

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return Observation{}, err
	}
	defer f.Close()
	if _, err := f.WriteString(b.String()); err != nil {
		return Observation{}, err
	}
	return o, nil
}

// List reads back every recorded observation.
func List(root string) ([]Observation, error) {
	data, err := os.ReadFile(Path(root))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var out []Observation
	var cur *Observation
	for _, line := range strings.Split(string(data), "\n") {
		if m := idPattern.FindStringSubmatch(line); m != nil {
			if cur != nil {
				cur.Note = strings.TrimSpace(cur.Note)
				out = append(out, *cur)
			}
			cur = &Observation{ID: m[1]}
			continue
		}
		if cur == nil {
			continue
		}
		if k, v, ok := field(line); ok {
			switch k {
			case "date":
				cur.Date = v
			case "task":
				cur.Task = v
			case "category":
				cur.Category = v
			case "severity":
				cur.Severity = v
			}
			continue
		}
		if strings.TrimSpace(line) != "" {
			cur.Note += line + "\n"
		}
	}
	if cur != nil {
		cur.Note = strings.TrimSpace(cur.Note)
		out = append(out, *cur)
	}
	return out, nil
}

func field(line string) (key, val string, ok bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(line), "- ")
	if !ok {
		return "", "", false
	}
	k, v, ok := strings.Cut(rest, ": ")
	if !ok {
		return "", "", false
	}
	return strings.TrimSpace(k), strings.TrimSpace(v), true
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
