package baseline

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/orangehill/driftgate/internal/snapshot"
)

// Expectations are drifts a change (a ticket) intends, recorded by
// driftgate expect next to a baseline without touching it: the baseline
// stays as it is (mode freeze allows no other change), and compare tells
// the expected drifts from the others.
//
// They are kept in the folder <baseline folder>.expectations, beside the
// baseline's folder (so its manifest and digest stay the same), one file
// per ref: <ref>.jsonl. Two pull requests with different tickets thus
// change different files and never conflict in git. Lines are only
// added: expectations of requests (expect, which replaces the ref's
// earlier expectation of a request), all of them removed (drop), or some
// closed once the baseline holds their drift (close). Like the acceptance
// log, every line names the SHA-256 of the line before, and the last line
// is a seal with the number of lines and the SHA-256 of the last of them.
// Every line names the baseline and the ref, so a file copied to another
// baseline or renamed to another ref is refused. As the manifest is, this
// is evidence of accidents and hand edits, not a signature: anyone can
// write a valid file, and review of the pull request (with baselines: git)
// is what protects it.

// ExpectSuffix ends the name of a baseline's expectations folder.
const ExpectSuffix = ".expectations"

// ExpectDir is the expectations folder of the baseline in dir.
func ExpectDir(dir string) string { return filepath.Clean(dir) + ExpectSuffix }

// Operations of an expectations line.
const (
	OpExpect = "expect"
	OpDrop   = "drop"
	OpClose  = "close"
	opSeal   = "seal"
)

// ExpectLine is one line of a ref's expectations file.
type ExpectLine struct {
	Version  int    `json:"version"`
	Op       string `json:"op"`
	Baseline string `json:"baseline"`
	Ref      string `json:"ref"`
	At       string `json:"at,omitempty"`
	By       string `json:"by,omitempty"`
	Reason   string `json:"reason,omitempty"`
	// Capture is the capture whose comparison the drifts come from
	// (expect), or the one a rebase took (close).
	Capture  string     `json:"capture,omitempty"`
	Commit   string     `json:"commit,omitempty"`
	Requests []Expected `json:"requests,omitempty"`
	Prev     string     `json:"prev,omitempty"`
	// The seal (the last line only): the number of lines before it and
	// the SHA-256 of the last of them.
	Lines int    `json:"lines,omitempty"`
	Last  string `json:"last,omitempty"`
}

// Expected is what an expectation names of one request: a digest of each
// drift it expects, with its values and what its description leaves out
// (sha256:… for a public response, ref:… keyed with the project key for a
// private one). Signatures are the drifts' signatures as accept logs
// them, to read.
type Expected struct {
	ID         string   `json:"id"`
	Drifts     []string `json:"drifts"`
	Signatures []string `json:"signatures,omitempty"`
}

// Expectation is an expectation in force: drifts of one request.
type Expectation struct {
	Ref    string
	Reason string
	By     string
	At     string
	Expected
}

// ExpectProblems are what is wrong with an expectations file that did
// not change through driftgate.
type ExpectProblems struct {
	File     string // its path under the baselines folder: local.expectations/T-1.jsonl
	Problems []string
}

func (e *ExpectProblems) Error() string {
	more := ""
	if n := len(e.Problems) - 1; n > 0 {
		more = fmt.Sprintf(" and %d more", n)
	}
	return fmt.Sprintf("%s changed outside driftgate expect (%s%s)", e.File, e.Problems[0], more)
}

// RefFile is the name of a ref's file: the ref itself when it is a plain
// file name of at most 60 characters (letters, digits, . _ -, not
// starting with a dot), otherwise the ref with other characters written
// as _, cut to 60 characters, and 8 bytes of its SHA-256 after a ~. hashed asks for the second form also
// for a plain ref, for a folder where its plain name is taken by another
// ref that differs only in letter case (macOS and Windows file names).
func RefFile(ref string, hashed bool) string {
	var b strings.Builder
	plain := ref != "" && ref[0] != '.' && len(ref) <= 60
	for _, r := range ref {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
			plain = false
		}
	}
	name := b.String()
	if strings.HasPrefix(name, ".") {
		name = "_" + name[1:] // no hidden file
	}
	if len(name) > 60 {
		name = name[:60]
	}
	if plain && !hashed && !reservedName(name) {
		return name + ".jsonl"
	}
	sum := sha256.Sum256([]byte(ref))
	return name + "~" + hex.EncodeToString(sum[:8]) + ".jsonl"
}

// reservedName reports a name Windows reserves for a device.
func reservedName(name string) bool {
	base, _, _ := strings.Cut(strings.ToLower(name), ".")
	switch base {
	case "con", "prn", "aux", "nul":
		return true
	}
	return len(base) == 4 && (strings.HasPrefix(base, "com") || strings.HasPrefix(base, "lpt")) && base[3] >= '1' && base[3] <= '9'
}

// refState is one ref's file as read: its lines without the seal.
type refState struct {
	path  string
	lines [][]byte
	ref   string
}

// readRefs reads and checks every file of a baseline's expectations
// folder; name is the baseline's name.
func readRefs(dir string) (map[string]*refState, error) {
	folder := ExpectDir(dir)
	entries, err := os.ReadDir(folder)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	name := filepath.Base(filepath.Clean(dir))
	refs := map[string]*refState{}
	for _, e := range entries {
		if ignored(e.Name()) {
			continue
		}
		rel := filepath.ToSlash(filepath.Join(filepath.Base(folder), e.Name()))
		if e.IsDir() || e.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(e.Name(), ".jsonl") {
			return nil, &ExpectProblems{File: rel, Problems: []string{"not an expectations file (a plain <ref>.jsonl)"}}
		}
		st, err := readRef(filepath.Join(folder, e.Name()), rel, name)
		if err != nil {
			return nil, err
		}
		if other, ok := refs[st.ref]; ok {
			return nil, &ExpectProblems{File: rel, Problems: []string{fmt.Sprintf("ref %s has a file already (%s)", st.ref, filepath.Base(other.path))}}
		}
		refs[st.ref] = st
	}
	return refs, nil
}

// readRef reads and checks one ref's file.
func readRef(path, rel, baselineName string) (*refState, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	broken := func(problems ...string) error { return &ExpectProblems{File: rel, Problems: problems} }
	if len(raw) == 0 {
		return nil, broken("the file is empty, without its seal")
	}
	if raw[len(raw)-1] != '\n' {
		return nil, broken("the last line has no newline")
	}
	rawLines := bytes.Split(raw[:len(raw)-1], []byte("\n"))
	var problems []string
	st := &refState{path: path}
	prev := ""
	for i, line := range rawLines {
		var l ExpectLine
		if err := json.Unmarshal(line, &l); err != nil || len(bytes.TrimSpace(line)) == 0 {
			problems = append(problems, fmt.Sprintf("line %d is not valid JSON", i+1))
			prev = snapshot.Sum(line)
			continue
		}
		if i == 0 {
			st.ref = l.Ref
		}
		last := i == len(rawLines)-1
		switch {
		case l.Version != Version:
			problems = append(problems, fmt.Sprintf("line %d: unsupported version %d", i+1, l.Version))
		case l.Baseline != baselineName:
			problems = append(problems, fmt.Sprintf("line %d is of baseline %q, not %s (copied from another baseline?)", i+1, l.Baseline, baselineName))
		case l.Ref != st.ref || l.Ref == "":
			problems = append(problems, fmt.Sprintf("line %d is of ref %q, not %q", i+1, l.Ref, st.ref))
		case last && l.Op != opSeal:
			problems = append(problems, fmt.Sprintf("line %d (the last) is not the seal (lines were added or removed at the end)", i+1))
		case last:
			if l.Lines != i || l.Last != prev {
				problems = append(problems, fmt.Sprintf("the seal does not match line %d (a line was edited, removed or added)", i))
			}
		case l.Op == opSeal:
			problems = append(problems, fmt.Sprintf("line %d is a seal before the last line", i+1))
		case l.Prev != prev:
			if i == 0 {
				problems = append(problems, "line 1 refers to a line before it (lines were removed)")
			} else {
				problems = append(problems, fmt.Sprintf("line %d does not follow line %d (a line was edited, removed or inserted)", i+1, i))
			}
		default:
			if p := l.check(); p != "" {
				problems = append(problems, fmt.Sprintf("line %d: %s", i+1, p))
			}
		}
		if !last {
			st.lines = append(st.lines, line)
		}
		prev = snapshot.Sum(line)
	}
	if len(problems) == 0 && len(st.lines) == 0 {
		// driftgate never writes a seal alone: every file holds a line.
		problems = append(problems, "the file has only its seal, no line before it")
	}
	if len(problems) == 0 && st.ref != "" {
		if base := filepath.Base(path); base != RefFile(st.ref, false) && base != RefFile(st.ref, true) {
			problems = append(problems, fmt.Sprintf("the file of ref %s is named %s (renamed?)", st.ref, RefFile(st.ref, false)))
		}
	}
	if len(problems) > 0 {
		return nil, broken(problems...)
	}
	return st, nil
}

// check says what a line lacks; empty when it is whole.
func (l ExpectLine) check() string {
	switch l.Op {
	case OpExpect, OpClose:
		if len(l.Requests) == 0 {
			return l.Op + " without requests"
		}
		for _, e := range l.Requests {
			if e.ID == "" || len(e.Drifts) == 0 {
				return l.Op + " of a request without its ID or drifts"
			}
			for _, d := range e.Drifts {
				if d == "" {
					return l.Op + " of a request with an empty drift"
				}
			}
		}
	case OpDrop:
	default:
		return fmt.Sprintf("unknown op %q", l.Op)
	}
	return ""
}

// inForce applies the lines of one ref: the expectations of its requests
// in force, by request.
func (st *refState) inForce() (map[string]Expectation, error) {
	out := map[string]Expectation{}
	for _, line := range st.lines {
		var l ExpectLine
		if err := json.Unmarshal(line, &l); err != nil {
			return nil, err
		}
		switch l.Op {
		case OpExpect:
			for _, e := range l.Requests {
				out[e.ID] = Expectation{Ref: l.Ref, Reason: l.Reason, By: l.By, At: l.At, Expected: e}
			}
		case OpDrop:
			clear(out)
		case OpClose:
			for _, e := range l.Requests {
				if x, ok := out[e.ID]; ok && sameSet(x.Drifts, e.Drifts) {
					delete(out, e.ID)
				}
			}
		}
	}
	return out, nil
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	in := make(map[string]bool, len(a))
	for _, x := range a {
		in[x] = true
	}
	for _, x := range b {
		if !in[x] {
			return false
		}
	}
	return true
}

// Expectations reads the expectations in force of the baseline in dir,
// ordered by ref and request, and the refs that have a file. A missing
// folder has none. A file that did not change through driftgate is
// refused with *ExpectProblems.
func Expectations(dir string) ([]Expectation, []string, error) {
	refs, err := readRefs(dir)
	if err != nil {
		return nil, nil, err
	}
	names := make([]string, 0, len(refs))
	for ref := range refs {
		names = append(names, ref)
	}
	sort.Strings(names)
	var out []Expectation
	for _, ref := range names {
		m, err := refs[ref].inForce()
		if err != nil {
			return nil, nil, err
		}
		ids := make([]string, 0, len(m))
		for id := range m {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			out = append(out, m[id])
		}
	}
	return out, names, nil
}

// AppendExpectation adds a line to the ref's file of the baseline in dir
// (l.Ref; l.Baseline is set), chained to the line before, and seals the
// file again. The folder's files are checked first, as Expectations
// checks them.
func AppendExpectation(dir string, l ExpectLine) error {
	refs, err := readRefs(dir)
	if err != nil {
		return err
	}
	l.Baseline = filepath.Base(filepath.Clean(dir))
	if l.Ref == "" {
		return errors.New("a line without a ref")
	}
	if p := l.check(); p != "" {
		return errors.New(p)
	}
	var kept [][]byte
	path := ""
	if st, ok := refs[l.Ref]; ok {
		kept, path = st.lines, st.path
	} else {
		path = filepath.Join(ExpectDir(dir), RefFile(l.Ref, false))
		if _, err := os.Lstat(path); err == nil {
			// Taken by a ref that differs only in letter case.
			path = filepath.Join(ExpectDir(dir), RefFile(l.Ref, true))
		}
		if _, err := os.Lstat(path); err == nil {
			// The ref has no file, so this one is another ref's: never
			// written over.
			return fmt.Errorf("%s is the file of another ref; ref %s cannot be written next to it", filepath.Base(path), l.Ref)
		}
	}
	l.Version, l.Prev, l.Lines, l.Last = Version, "", 0, ""
	if len(kept) > 0 {
		l.Prev = snapshot.Sum(kept[len(kept)-1])
	}
	line, err := encodeLine(l)
	if err != nil {
		return err
	}
	kept = append(kept, line)
	return writeRef(path, kept, l.Baseline, l.Ref)
}

// writeRef writes a ref's lines and their seal.
func writeRef(path string, lines [][]byte, baselineName, ref string) error {
	seal, err := encodeLine(ExpectLine{Version: Version, Op: opSeal, Baseline: baselineName, Ref: ref, Lines: len(lines), Last: snapshot.Sum(lines[len(lines)-1])})
	if err != nil {
		return err
	}
	var all bytes.Buffer
	for _, k := range append(lines, seal) {
		all.Write(k)
		all.WriteByte('\n')
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return snapshot.WriteFile(path, all.Bytes(), 0o644)
}

// ImportExpectations takes the file of a ref from elsewhere (another
// worktree's baseline of the same name) into the baseline in dir, after
// checking it as Expectations does. A ref the baseline has already is
// replaced only by a file that continues it (its lines and more); the
// ref is returned.
func ImportExpectations(dir, file string) (string, bool, error) {
	name := filepath.Base(filepath.Clean(dir))
	st, err := readRef(file, filepath.Base(file), name)
	if err != nil {
		return "", false, err
	}
	refs, err := readRefs(dir)
	if err != nil {
		return "", false, err
	}
	path := filepath.Join(ExpectDir(dir), filepath.Base(file))
	if have, ok := refs[st.ref]; ok {
		if len(have.lines) > len(st.lines) {
			return "", false, fmt.Errorf("baseline %s has more of ref %s than %s; nothing was imported", name, st.ref, file)
		}
		for i, l := range have.lines {
			if !bytes.Equal(l, st.lines[i]) {
				return "", false, fmt.Errorf("baseline %s has other expectations of ref %s than %s (line %d differs); to take that file, delete this baseline's file of the ref (%s) and import again; a --drop adds a line, so the two files would still differ", name, st.ref, file, i+1, have.path)
			}
		}
		if len(have.lines) == len(st.lines) {
			return st.ref, false, nil
		}
		path = have.path
	} else if _, err := os.Lstat(path); err == nil {
		return "", false, fmt.Errorf("%s is the file of another ref in baseline %s", filepath.Base(file), name)
	}
	return st.ref, true, writeRef(path, st.lines, name, st.ref)
}

// encodeLine writes a line as one line of JSON, without its newline.
func encodeLine(l ExpectLine) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(l); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}
