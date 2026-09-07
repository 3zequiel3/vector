package hook

import "strings"

// WriteTargets extracts the paths a shell command would write to.
//
// This exists because tool-level hooks fire on the tool, not on what the tool
// does. A hook watching Edit and Write sees nothing when the same content
// arrives through `cat > file << EOF` or `echo x >> file`, which is one of the
// documented ways enforcement leaks. Reading the command closes the common
// cases without pretending to be a shell.
//
// It is deliberately over-inclusive: a false positive costs one explained
// denial, a false negative costs the guarantee. It is also not a security
// boundary — a Python script writing files is invisible here, and only an OS
// sandbox catches that.
func WriteTargets(command string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" || strings.HasPrefix(p, "-") || strings.ContainsAny(p, "$`") {
			return
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}

	for _, simple := range splitCommands(command) {
		args := tokenize(simple)
		if len(args) == 0 {
			continue
		}
		// Redirections write regardless of which program runs.
		for i := 0; i < len(args); i++ {
			if target, ok := redirectTarget(args, i); ok {
				add(target)
			}
		}
		add(commandTarget(args))
	}
	return out
}

// redirectTarget resolves `> f`, `>> f`, `2> f`, `&> f` and their attached
// forms (`>f`). A heredoc (`<< EOF`) is not itself a write — the `>` beside it
// is, and that is already matched.
func redirectTarget(args []string, i int) (string, bool) {
	a := args[i]
	if !strings.ContainsRune(a, '>') {
		return "", false
	}
	// Strip an optional file-descriptor prefix: 2>, &>, 1>>.
	body := strings.TrimLeft(a, "0123456789&")
	if !strings.HasPrefix(body, ">") {
		return "", false
	}
	rest := strings.TrimLeft(body, ">")
	if rest != "" {
		return rest, true // attached form, e.g. >>out.txt
	}
	if i+1 < len(args) {
		return args[i+1], true
	}
	return "", false
}

// writers maps a command to how it names the file it writes.
type writerKind int

const (
	// lastArg: the final non-flag argument is the destination (mv, cp).
	lastArg writerKind = iota
	// allArgs: every non-flag argument is written (rm, touch, mkdir).
	allArgs
	// inPlaceLast: only writes when an in-place flag is present (sed, sd).
	inPlaceLast
)

var writers = map[string]writerKind{
	"tee": allArgs, "mv": lastArg, "cp": lastArg, "install": lastArg,
	"rm": allArgs, "rmdir": allArgs, "touch": allArgs, "mkdir": allArgs,
	"truncate": allArgs, "chmod": allArgs, "chown": allArgs, "ln": lastArg,
	"sed": inPlaceLast, "sd": inPlaceLast, "perl": inPlaceLast,
}

// commandTarget returns a single representative destination for the command, or
// "" when the command does not write by itself.
func commandTarget(args []string) string {
	name := args[0]
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	kind, known := writers[name]
	if !known {
		return ""
	}

	var operands []string
	inPlace := false
	for _, a := range args[1:] {
		if strings.HasPrefix(a, "-") {
			// -i, -i.bak, and the bundled forms like -ni all mean in place.
			if strings.HasPrefix(a, "-i") || strings.Contains(a, "i") && name == "sed" {
				inPlace = true
			}
			continue
		}
		if strings.ContainsRune(a, '>') || strings.ContainsRune(a, '<') {
			break
		}
		operands = append(operands, a)
	}
	if len(operands) == 0 {
		return ""
	}

	switch kind {
	case inPlaceLast:
		if !inPlace {
			return ""
		}
		return operands[len(operands)-1]
	case lastArg:
		return operands[len(operands)-1]
	default:
		return operands[0]
	}
}

// splitCommands breaks a line on the operators that separate simple commands,
// so `cd x && echo y > z` is examined as two commands rather than one.
func splitCommands(s string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	flush := func() {
		if strings.TrimSpace(cur.String()) != "" {
			out = append(out, cur.String())
		}
		cur.Reset()
	}
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
			cur.WriteRune(c)
		case c == '\'' || c == '"':
			quote = c
			cur.WriteRune(c)
		case c == ';' || c == '\n':
			flush()
		case c == '|' || c == '&':
			// A pipe separates commands; the redirection operators are handled
			// by the tokenizer, so only the control forms matter here.
			if i+1 < len(runes) && runes[i+1] == c {
				i++
			}
			flush()
		default:
			cur.WriteRune(c)
		}
	}
	flush()
	return out
}

// tokenize splits a simple command into words, honouring quotes and keeping
// redirection operators as their own tokens.
func tokenize(s string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				cur.WriteRune(c)
			}
		case c == '\\' && i+1 < len(runes):
			i++
			cur.WriteRune(runes[i])
		case c == '\'' || c == '"':
			quote = c
		case c == ' ' || c == '\t':
			flush()
		case c == '>' || c == '<':
			// Keep an attached destination with its operator (>>out.txt) but
			// split it away from the preceding word (echo x>out).
			flush()
			cur.WriteRune(c)
			for i+1 < len(runes) && runes[i+1] == c {
				i++
				cur.WriteRune(c)
			}
		default:
			cur.WriteRune(c)
		}
	}
	flush()
	return out
}
