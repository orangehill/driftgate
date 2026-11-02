// Package baseline guards the integrity of a baseline folder: manifest.json
// holds the SHA-256 of every file and the hash of the last line of the
// acceptance log, whose lines are chained by the hash of the line before.
// A baseline changes only through driftgate (first run, import, accept);
// verify finds any other change.
//
// The manifest is not a signature: anyone can recompute it. It makes a hand
// edit visible (verify fails, compare refuses), and a legitimate change then
// shows in the pull request as a new acceptance line next to the files.
package baseline

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/orangehill/driftgate/internal/snapshot"
)

const Version = 1

// File names inside a baseline folder.
const (
	ManifestFile = "manifest.json"
	LogFile      = "acceptances.jsonl"
)

// ErrNoManifest means the baseline has no manifest.json (made before
// manifests existed, or the file was removed).
var ErrNoManifest = errors.New("no " + ManifestFile)

// Manifest is manifest.json.
type Manifest struct {
	Version int               `json:"version"`
	Files   map[string]string `json:"files"` // slash path → SHA-256; without the manifest and the log
	Log     LogState          `json:"acceptances"`
}

// LogState pins the acceptance log: its number of lines and the SHA-256 of
// the last one (empty when there are none).
type LogState struct {
	Lines int    `json:"lines"`
	Last  string `json:"last"`
}

// Acceptance is one line of acceptances.jsonl.
type Acceptance struct {
	Version    int      `json:"version"`
	At         string   `json:"at"`
	By         string   `json:"by"`
	Reason     string   `json:"reason"`
	Ref        string   `json:"ref,omitempty"`
	Requests   []string `json:"requests"`
	Signatures []string `json:"signatures"`
	Capture    string   `json:"capture"`
	Commit     string   `json:"commit,omitempty"`
	// Prev is the SHA-256 of the line before (empty on the first line), so
	// an edited, removed or inserted line breaks the chain.
	Prev string `json:"prev,omitempty"`
}

// ignored are files no tool of ours writes and that say nothing about the
// baseline (Finder metadata).
func ignored(name string) bool { return name == ".DS_Store" }

// Sums returns the SHA-256 of every file under dir by slash path, without
// the manifest and the log.
func Sums(dir string) (map[string]string, error) {
	// A baseline is plain files in a folder of the repository: a symbolic
	// link could point anywhere and change without the repository changing.
	if fi, err := os.Lstat(dir); err != nil {
		return nil, err
	} else if fi.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symbolic link; a baseline must be a folder in the repository", dir)
	}
	var rels []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symbolic link; a baseline holds only plain files", path)
		}
		if d.IsDir() || ignored(d.Name()) {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel = filepath.ToSlash(rel); !listed(rel) {
			return nil
		}
		rels = append(rels, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return sumFiles(dir, rels)
}

// listed reports whether the manifest lists a file: all but itself and the log.
func listed(rel string) bool { return rel != ManifestFile && rel != LogFile }

// sumFiles hashes the files (slash paths under dir) in parallel, each read
// as a stream.
func sumFiles(dir string, rels []string) (map[string]string, error) {
	out := make([]string, len(rels))
	err := snapshot.Each(len(rels), func(i int) error {
		f, err := os.Open(filepath.Join(dir, filepath.FromSlash(rels[i])))
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return err
		}
		out[i] = hex.EncodeToString(h.Sum(nil))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sums := make(map[string]string, len(rels))
	for i, rel := range rels {
		sums[rel] = out[i]
	}
	return sums, nil
}

// Write computes and writes the manifest of the baseline in dir.
func Write(dir string) error {
	sums, err := Sums(dir)
	if err != nil {
		return err
	}
	return writeManifest(dir, sums)
}

// WriteFrom writes the manifest of the baseline in dir from the SHA-256 of
// its files taken while they were written (slash path → SHA-256 of every
// file), as Write would compute them; files the manifest leaves out are
// skipped.
func WriteFrom(dir string, written map[string]string) error {
	sums := map[string]string{}
	for rel, sum := range written {
		if listed(rel) && !ignored(path.Base(rel)) {
			sums[rel] = sum
		}
	}
	return writeManifest(dir, sums)
}

func writeManifest(dir string, sums map[string]string) error {
	lines, err := readLines(dir)
	if err != nil {
		return err
	}
	m := Manifest{Version: Version, Files: sums, Log: LogState{Lines: len(lines)}}
	if len(lines) > 0 {
		m.Log.Last = snapshot.Sum(lines[len(lines)-1])
	}
	return snapshot.WriteJSON(filepath.Join(dir, ManifestFile), m)
}

// Read reads the manifest; ErrNoManifest when there is none.
func Read(dir string) (*Manifest, error) {
	path := filepath.Join(dir, ManifestFile)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoManifest
	}
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if m.Version != Version {
		return nil, fmt.Errorf("%s: unsupported version %d", path, m.Version)
	}
	return &m, nil
}

// Digest identifies the current content of a baseline: the SHA-256 of its
// manifest. It is meaningful only for a baseline that verifies.
func Digest(dir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNoManifest
	}
	if err != nil {
		return "", err
	}
	return snapshot.Sum(raw), nil
}

// SetDigest identifies the content of a capture: its records and meta.json.
// Bodies are named by their hash (or ref) in the records, so the records
// cover them, and are not read.
func SetDigest(dir string) (string, error) {
	if fi, err := os.Lstat(dir); err != nil {
		return "", err
	} else if fi.Mode()&fs.ModeSymlink != 0 {
		return "", fmt.Errorf("%s is a symbolic link; a baseline must be a folder in the repository", dir)
	}
	var rels []string
	meta := filepath.Join(dir, "meta.json")
	switch fi, err := os.Lstat(meta); {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return "", err
	case fi.Mode()&fs.ModeSymlink != 0:
		return "", fmt.Errorf("%s is a symbolic link; a baseline holds only plain files", meta)
	case fi.Mode().IsRegular():
		rels = append(rels, "meta.json")
	}
	responses := filepath.Join(dir, "responses")
	if _, err := os.Lstat(responses); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	} else if err == nil {
		err := filepath.WalkDir(responses, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type()&fs.ModeSymlink != 0 {
				return fmt.Errorf("%s is a symbolic link; a baseline holds only plain files", path)
			}
			if d.IsDir() || ignored(d.Name()) {
				return nil
			}
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			rels = append(rels, filepath.ToSlash(rel))
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	sums, err := sumFiles(dir, rels)
	if err != nil {
		return "", err
	}
	sort.Strings(rels)
	var b strings.Builder
	for _, k := range rels {
		fmt.Fprintf(&b, "%s %s\n", sums[k], k)
	}
	return snapshot.Sum([]byte(b.String())), nil
}

// Stamp identifies the state of the files under dir without reading them:
// the path, size, modification time and mode of each. A baseline whose
// stamp is the one it had when it verified has not been written since.
func Stamp(dir string) (string, error) {
	var b []byte
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		b = append(append(b, path...), 0) // no path holds a NUL
		for _, n := range []int64{fi.Size(), fi.ModTime().UnixNano(), int64(fi.Mode())} {
			b = binary.LittleEndian.AppendUint64(b, uint64(n))
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return snapshot.Sum(b), nil
}

// Result is what Verify found.
type Result struct {
	Files       int      // files listed in the manifest
	Acceptances int      // lines in the acceptance log
	Problems    []string // empty when the baseline is intact
	// LineEndings: some changed files differ only in their line endings
	// (CRLF where the manifest has LF): git converted them on checkout
	// (core.autocrlf, on by default with Git for Windows).
	LineEndings bool
}

// OK reports whether the baseline is intact.
func (r *Result) OK() bool { return len(r.Problems) == 0 }

// Verify checks the files against the manifest (changed, missing, extra)
// and the acceptance log against its chain and the manifest. A missing
// manifest returns ErrNoManifest.
func Verify(dir string) (*Result, error) {
	m, err := Read(dir)
	if err != nil {
		return nil, err
	}
	sums, err := Sums(dir)
	if err != nil {
		return nil, err
	}
	res := &Result{Files: len(m.Files)}
	var changed, crlf, missing, extra []string
	for path, want := range m.Files {
		got, ok := sums[path]
		switch {
		case !ok:
			missing = append(missing, path)
		case got != want && crlfOnly(filepath.Join(dir, filepath.FromSlash(path)), want):
			crlf = append(crlf, path)
			res.LineEndings = true
		case got != want:
			changed = append(changed, path)
		}
	}
	for path := range sums {
		if _, ok := m.Files[path]; !ok {
			extra = append(extra, path)
		}
	}
	for _, l := range []struct {
		what  string
		paths []string
	}{{"changed", changed}, {"changed (only its line endings: CRLF instead of LF)", crlf}, {"missing", missing}, {"not in the manifest", extra}} {
		sort.Strings(l.paths)
		for _, p := range l.paths {
			res.Problems = append(res.Problems, fmt.Sprintf("%s: %s", p, l.what))
		}
	}

	lines, err := readLines(dir)
	if err != nil {
		res.Problems = append(res.Problems, err.Error())
		return res, nil
	}
	res.Acceptances = len(lines)
	res.Problems = append(res.Problems, checkChain(lines)...)
	switch {
	case len(lines) != m.Log.Lines:
		res.Problems = append(res.Problems, fmt.Sprintf("%s: %d lines, the manifest records %d", LogFile, len(lines), m.Log.Lines))
	case len(lines) > 0 && snapshot.Sum(lines[len(lines)-1]) != m.Log.Last:
		res.Problems = append(res.Problems, fmt.Sprintf("%s: line %d (the last) differs from the manifest", LogFile, len(lines)))
	}
	return res, nil
}

// crlfOnly reports whether the file at path, with CRLF written as LF, has
// the SHA-256 want. Only a changed file is read again.
func crlfOnly(path, want string) bool {
	raw, err := os.ReadFile(path)
	return err == nil && bytes.Contains(raw, []byte("\r\n")) && snapshot.Sum(bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))) == want
}

// checkChain checks that every line is an acceptance and names the hash of
// the line before.
func checkChain(lines [][]byte) []string {
	var problems []string
	prev := ""
	for i, line := range lines {
		var a Acceptance
		if err := json.Unmarshal(line, &a); err != nil {
			problems = append(problems, fmt.Sprintf("%s: line %d is not valid JSON", LogFile, i+1))
		} else if a.Prev != prev {
			if i == 0 {
				problems = append(problems, fmt.Sprintf("%s: line 1 refers to a line before it (lines were removed)", LogFile))
			} else {
				problems = append(problems, fmt.Sprintf("%s: line %d does not follow line %d (a line was edited, removed or inserted)", LogFile, i+1, i))
			}
		}
		prev = snapshot.Sum(line)
	}
	return problems
}

// readLines reads the acceptance log; a missing log has no lines. Every
// line must end with a newline and none may be empty.
func readLines(dir string) ([][]byte, error) {
	raw, err := os.ReadFile(filepath.Join(dir, LogFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		return nil, fmt.Errorf("%s: the last line has no newline (edited by hand?)", LogFile)
	}
	var lines [][]byte
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(nil, 16<<20)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			return nil, fmt.Errorf("%s: line %d is empty", LogFile, len(lines)+1)
		}
		lines = append(lines, bytes.Clone(sc.Bytes()))
	}
	return lines, sc.Err()
}

// Log reads the acceptances recorded so far.
func Log(dir string) ([]Acceptance, error) {
	lines, err := readLines(dir)
	if err != nil {
		return nil, err
	}
	out := make([]Acceptance, len(lines))
	for i, line := range lines {
		if err := json.Unmarshal(line, &out[i]); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", LogFile, i+1, err)
		}
	}
	return out, nil
}

// Append adds an acceptance as the last line of the log, chained to the
// line before. The manifest is not rewritten; call Write after.
func Append(dir string, a Acceptance) error {
	lines, err := readLines(dir)
	if err != nil {
		return err
	}
	a.Version, a.Prev = Version, ""
	if len(lines) > 0 {
		a.Prev = snapshot.Sum(lines[len(lines)-1])
	}
	if a.Requests == nil {
		a.Requests = []string{}
	}
	if a.Signatures == nil {
		a.Signatures = []string{}
	}
	var line bytes.Buffer
	enc := json.NewEncoder(&line) // one line, with a trailing newline
	enc.SetEscapeHTML(false)
	if err := enc.Encode(a); err != nil {
		return err
	}
	var all bytes.Buffer
	for _, l := range lines {
		all.Write(l)
		all.WriteByte('\n')
	}
	all.Write(line.Bytes())
	return snapshot.WriteFile(filepath.Join(dir, LogFile), all.Bytes(), 0o644)
}
