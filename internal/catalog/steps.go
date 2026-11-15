package catalog

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/orangehill/driftgate/internal/snapshot"
	"github.com/orangehill/driftgate/internal/upload"
)

// MaxRepeat bounds repeat: a catalog entry is a check, not a load test.
const MaxRepeat = 10000

// Multipart is a multipart/form-data body: form fields in order, then files.
type Multipart struct {
	Fields [][2]string `json:"fields,omitempty"`
	Files  []File      `json:"files,omitempty"`
}

// File is one file of a multipart body: a real file from the uploads folder
// outside the repository, named by its path relative to that folder and
// checked by Ref, or a synthetic file of a kind (upload.Kinds). The content
// never enters the catalog.
type File struct {
	Field     string `json:"field"`
	Path      string `json:"path,omitempty"`
	Ref       string `json:"ref,omitempty"` // keyed hash of the content (as a private body is named); a changed file is a configuration change
	Synthetic string `json:"synthetic,omitempty"`
	Name      string `json:"name,omitempty"` // file name sent; default the base name of path, or upload.<ext>
	Type      string `json:"type,omitempty"` // content type of the part; default from the kind or the extension
}

// IsCommand reports an entry that runs a command instead of sending a request.
func (r Request) IsCommand() bool { return len(r.Command) > 0 }

// ReadOnly reports an entry a read-only capture (--read-only) sends: a
// request that does not change data, and a command step only when it
// says mutates: false.
func (r Request) ReadOnly() bool {
	if r.IsCommand() {
		return r.Mutates != nil && !*r.Mutates
	}
	return !r.Mutating()
}

// ReadOnlySkips are the IDs a read-only capture leaves out: the entries
// that are not ReadOnly, and every request of a sequence that holds one
// (its later requests may take values from the earlier ones). nil when
// it leaves out none.
func (c *Catalog) ReadOnlySkips() map[string]bool {
	var skip map[string]bool
	add := func(id string) {
		if skip == nil {
			skip = map[string]bool{}
		}
		skip[id] = true
	}
	for _, r := range c.Requests {
		if !r.ReadOnly() {
			add(r.ID)
		}
	}
	for _, s := range c.Sequences {
		if slices.ContainsFunc(s.Requests, func(id string) bool { return skip[id] }) {
			for _, id := range s.Requests {
				add(id)
			}
		}
	}
	return skip
}

// Sequential reports an entry that runs one at a time after the read-only
// requests, like a request that changes data: a command, a request sent
// several times (its counters live in the state), or one that uploads a
// real file (its content may stay in the data, so every response after it
// until the next restore is stored as private, which only the sequential
// phase does).
func (r Request) Sequential() bool {
	return r.Mutating() || r.IsCommand() || r.Repeat > 1 || r.UsesFiles()
}

// UsesFiles reports an entry that sends a file from the uploads folder.
func (r Request) UsesFiles() bool {
	if r.Multipart == nil {
		return false
	}
	for _, f := range r.Multipart.Files {
		if f.Path != "" {
			return true
		}
	}
	return false
}

func validateStep(r Request) error {
	if r.Repeat < 0 || r.Repeat > MaxRepeat {
		return fmt.Errorf("repeat must be from 1 to %d", MaxRepeat)
	}
	if r.IsCommand() {
		switch {
		case r.Command[0] == "":
			return errors.New("command: the program is empty")
		case r.Method != "" || r.Path != "":
			return errors.New("a command step has no method or path")
		case len(r.Body) > 0 || r.Multipart != nil || len(r.Headers) > 0 || len(r.Params) > 0 || len(r.ParamsFrom) > 0 || r.Identity != "":
			return errors.New("a command step has no body, headers, parameters or identity")
		case r.Repeat > 1:
			return errors.New("a command step is not repeated")
		}
		if len(r.OutputIgnore) > 0 && !r.Output {
			return errors.New("output_ignore needs output: true")
		}
		for _, re := range r.OutputIgnore {
			if _, err := regexp.Compile(re); err != nil {
				return fmt.Errorf("output_ignore: %w", err)
			}
		}
	} else if r.Output || len(r.OutputIgnore) > 0 || r.Dir != "" || r.Stdin != "" {
		return errors.New("output, output_ignore, dir and stdin belong to command steps")
	}
	if r.Multipart == nil {
		return nil
	}
	if len(r.Body) > 0 {
		return errors.New("body and multipart cannot both be set")
	}
	for i, f := range r.Multipart.Files {
		where := fmt.Sprintf("multipart file %d", i+1)
		switch {
		case f.Field == "":
			return fmt.Errorf("%s: field is empty", where)
		case (f.Path == "") == (f.Synthetic == ""):
			return fmt.Errorf("%s: set either path (a file in the uploads folder) or synthetic (%s)", where, strings.Join(upload.Kinds(), ", "))
		case f.Synthetic != "" && !upload.Known(f.Synthetic):
			return fmt.Errorf("%s: unknown synthetic kind %q (%s)", where, f.Synthetic, strings.Join(upload.Kinds(), ", "))
		case f.Synthetic != "" && f.Ref != "":
			return fmt.Errorf("%s: a synthetic file has no ref", where)
		case f.Path != "" && (path.IsAbs(f.Path) || strings.Contains(f.Path, `\`) || path.Clean(f.Path) != f.Path || f.Path == "." || strings.HasPrefix(f.Path, "../") || f.Path == ".."):
			return fmt.Errorf("%s: path %q must be relative to the uploads folder, without . or .. in it", where, f.Path)
		}
	}
	// A real file holds real data, and so may the response that echoes it:
	// it is kept like personal data, encrypted on this machine only.
	if r.UsesFiles() && r.Storage != snapshot.Private {
		return errors.New("a request that sends a file from the uploads folder must have storage private (the response may echo its content)")
	}
	return nil
}
