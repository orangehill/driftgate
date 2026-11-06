package capture

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/httpx"
	"github.com/orangehill/driftgate/internal/procx"
	"github.com/orangehill/driftgate/internal/secret"
	"github.com/orangehill/driftgate/internal/snapshot"
	"github.com/orangehill/driftgate/internal/upload"
)

// CommandTimeout bounds one command step.
const CommandTimeout = 10 * time.Minute

// maxUpload bounds one file from the uploads folder.
const maxUpload = 256 << 20

// multipartBody is a request with files. Its body is encoded only when it
// is sent and dropped after, so no file is held for the whole capture.
type multipartBody struct {
	fields [][2]string
	parts  []upload.Part
}

// apply turns the request into one with the encoded body; a content type
// from the catalog would lack the boundary and is replaced.
func (m multipartBody) apply(req catalog.Request) (catalog.Request, error) {
	body, ct, err := upload.Encode(m.fields, m.parts)
	if err != nil {
		return req, err
	}
	var headers [][2]string
	for _, h := range req.Headers {
		if !strings.EqualFold(h[0], "content-type") {
			headers = append(headers, h)
		}
	}
	req.Headers = append(headers, [2]string{"content-type", ct})
	req.Body = json.RawMessage(body) // sent as it is; never written back to the catalog
	req.Multipart = nil
	return req, nil
}

// prepareUploads checks the files of every request with a multipart body
// before anything is sent: a missing file stops the capture, and a file
// whose content changed since the catalog named it is a change of
// configuration, not drift. A file is read here only to check its ref; it
// is read again (and checked again) when its request is sent.
func prepareUploads(p *config.Project, reqs []catalog.Request) (map[string]multipartBody, error) {
	out := map[string]multipartBody{}
	dir := ""
	var key secret.Key
	for _, req := range reqs {
		if req.Multipart == nil {
			continue
		}
		var parts []upload.Part
		for _, f := range req.Multipart.Files {
			part := upload.Part{Field: f.Field, Name: f.Name, Type: f.Type}
			if f.Synthetic != "" {
				data, err := upload.Synthetic(f.Synthetic)
				if err != nil {
					return nil, configErr("%s: %v", req.ID, err)
				}
				part.Data = data
				if part.Name == "" {
					part.Name = upload.FileName(f.Synthetic)
				}
				if part.Type == "" {
					part.Type = upload.MediaType(f.Synthetic)
				}
				parts = append(parts, part)
				continue
			}
			if dir == "" {
				d, err := p.UploadsDir()
				if err != nil {
					return nil, configErr("%s: %v", req.ID, err)
				}
				if st, err := os.Stat(d); err != nil || !st.IsDir() {
					return nil, envErr("the uploads folder %s is not on this machine (%s: %s)", d, req.ID, f.Path)
				}
				dir = d
			}
			if key == nil {
				var err error
				if key, err = p.LoadKey(); err != nil {
					return nil, envErr("%s: the key names uploaded files: %v", req.ID, err)
				}
			}
			ref, size, err := uploadRef(dir, f.Path, key)
			if err != nil {
				return nil, envErr("%s: %v", req.ID, err)
			}
			switch {
			case f.Ref == "":
				return nil, configErr("%s: %s has no ref in the catalog; after checking it is the file to upload, set \"ref\": %q", req.ID, f.Path, ref)
			case f.Ref != ref:
				return nil, configErr("%s: %s is not the file the catalog names (ref %s, the file now %s): the input changed, so responses cannot be compared; restore the file, or set \"ref\": %q and make a new baseline", req.ID, f.Path, f.Ref, ref, ref)
			}
			rel := f.Path
			part.Open = func() (io.ReadCloser, error) { return openChecked(dir, rel, key, ref, size) }
			part.Size = size
			if part.Name == "" {
				part.Name = filepath.Base(f.Path)
			}
			if part.Type == "" {
				part.Type = upload.MediaType(upload.KindOf(f.Path, ""))
			}
			parts = append(parts, part)
		}
		out[req.ID] = multipartBody{fields: req.Multipart.Fields, parts: parts}
	}
	return out, nil
}

// openUpload opens a regular file under dir. The file is opened through
// the folder as an os.Root, which follows a relative symbolic link only
// while it stays inside the folder, at the moment of the open (an absolute
// link is refused); the opened file itself is then checked and read, so
// nothing can be swapped between a check and the read.
func openUpload(dir, rel string) (*os.File, int64, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, 0, err
	}
	defer root.Close()
	f, err := root.OpenFile(filepath.FromSlash(rel), os.O_RDONLY|openNonblock, 0)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, 0, fmt.Errorf("upload file %s is not on this machine: %w", rel, err)
	case err != nil && strings.Contains(err.Error(), "escapes"):
		return nil, 0, fmt.Errorf("upload file %s leads out of the uploads folder", rel)
	case err != nil:
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, 0, fmt.Errorf("upload file %s is not a regular file", rel)
	}
	if st.Size() > maxUpload {
		f.Close()
		return nil, 0, fmt.Errorf("upload file %s is larger than %d bytes", rel, maxUpload)
	}
	return f, st.Size(), nil
}

// uploadRef reads a file of the uploads folder once and returns its ref
// and size.
func uploadRef(dir, rel string, key secret.Key) (string, int64, error) {
	f, _, err := openUpload(dir, rel)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	mac := key.RefHash()
	n, err := io.Copy(mac, io.LimitReader(f, maxUpload+1))
	if err != nil {
		return "", 0, err
	}
	if n > maxUpload {
		return "", 0, fmt.Errorf("upload file %s is larger than %d bytes", rel, maxUpload)
	}
	return hex.EncodeToString(mac.Sum(nil)), n, nil
}

// openChecked opens a file of the uploads folder for a send: read to its
// end, it must still be the file the catalog names.
func openChecked(dir, rel string, key secret.Key, ref string, size int64) (io.ReadCloser, error) {
	f, _, err := openUpload(dir, rel)
	if err != nil {
		return nil, err
	}
	return &checkedFile{f: f, r: io.LimitReader(f, size+1), mac: key.RefHash(), rel: rel, ref: ref}, nil
}

// checkedFile reads a file and, at its end, checks its ref.
type checkedFile struct {
	f        *os.File
	r        io.Reader
	mac      hash.Hash
	rel, ref string
}

func (c *checkedFile) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.mac.Write(p[:n])
	if err == io.EOF && hex.EncodeToString(c.mac.Sum(nil)) != c.ref {
		return n, fmt.Errorf("upload file %s changed during the capture", c.rel)
	}
	return n, err
}

func (c *checkedFile) Close() error { return c.f.Close() }

// ownState reports whether unit i starts from a restored state although
// nothing else asks for it: a request sent several times counts in the
// state (a rate limit in the cache), so it needs a known start, and the
// unit after it must not inherit its exhausted counters.
func ownState(units []unit, i int) bool {
	repeats := func(u unit) bool {
		for _, r := range u.reqs {
			if r.Repeat > 1 {
				return true
			}
		}
		return false
	}
	return repeats(units[i]) || (i > 0 && repeats(units[i-1]))
}

// do runs one entry of a unit: a command, a request sent several times, or
// a request. Once a real file has been uploaded, every response until the
// next restore is kept private: the data now holds the file's content, and
// any response may show it (the rows of an import, an export).
func (r *runner) do(ctx context.Context, req catalog.Request) httpx.Result {
	var res httpx.Result
	switch {
	case req.IsCommand():
		res = r.command(ctx, req)
	case req.Repeat > 1:
		res = r.repeat(ctx, req)
	default:
		res = r.send(ctx, req)
	}
	r.tainted = r.tainted || req.UsesFiles()
	if r.tainted && res.Record.Body.Storage != snapshot.Private {
		res.Record.Body.Storage = snapshot.Private
		fmt.Fprintf(r.out, "%s: stored as private, after the upload of a real file\n", req.ID)
	}
	return res
}

type unlimitedKey struct{}

// unlimited reports sends that skip target.rate_limits: the sends of a
// repeated request, which exist to reach the application's own limit. A
// remote target (service.remote) keeps its rate limits anyway.
func unlimited(ctx context.Context) bool { return ctx.Value(unlimitedKey{}) != nil }

// repeat sends a request Repeat times, one after another, and keeps the
// last response with the statuses of all of them ("200×60, 429×1").
func (r *runner) repeat(ctx context.Context, req catalog.Request) httpx.Result {
	ctx = context.WithValue(ctx, unlimitedKey{}, true)
	var statuses []string
	var total time.Duration
	var res httpx.Result
	for i := 0; i < req.Repeat; i++ {
		res.Release() // only the last response is kept
		res = r.send(ctx, req)
		total += res.Duration
		if res.Record.Error != "" {
			res.Record.Error = fmt.Sprintf("send %d of %d: %s", i+1, req.Repeat, res.Record.Error)
			break
		}
		statuses = append(statuses, fmt.Sprint(res.Record.Status))
	}
	res.Duration = total
	res.Record.Repeat = RunLength(statuses)
	return res
}

// RunLength writes a list of statuses as runs: "200×60, 429×1".
func RunLength(statuses []string) string {
	var parts []string
	for i := 0; i < len(statuses); {
		j := i
		for j < len(statuses) && statuses[j] == statuses[i] {
			j++
		}
		parts = append(parts, fmt.Sprintf("%s×%d", statuses[i], j-i))
		i = j
	}
	return strings.Join(parts, ", ")
}

// limitedBuffer keeps at most max bytes and notes that more came.
type limitedBuffer struct {
	bytes.Buffer
	max  int
	over bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room < len(p) {
		b.over = true
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

// command runs a command step without a shell, from the project root. Its
// record has the exit code and, with output: true, standard output (without
// what output_ignore matches) as the body. A command that cannot start is a
// failed step, like a request without a response.
func (r *runner) command(ctx context.Context, req catalog.Request) httpx.Result {
	rec := &snapshot.Record{ID: req.ID, Headers: [][2]string{}, Body: snapshot.Body{Storage: req.Storage}}
	dir, stdin, err := r.p.CommandPaths(stepCommand(req))
	if err != nil {
		rec.Error = err.Error()
		return httpx.Result{Record: rec}
	}
	ctx, cancel := context.WithTimeout(ctx, CommandTimeout)
	defer cancel()
	cmd := procx.Command(ctx, req.Command[0], req.Command[1:]...)
	cmd.Dir = dir
	in, closeIn, err := openStdin(stdin)
	if err != nil {
		rec.Error = err.Error()
		return httpx.Result{Record: rec}
	}
	defer closeIn()
	cmd.Stdin = in
	cmd.Env = append(secret.Environ(), r.env...) // the project key never reaches a command
	stdout := &limitedBuffer{max: httpx.MaxBody}
	stderr := &limitedBuffer{max: 64 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	start := time.Now()
	err = procx.Run(cmd)
	dur := time.Since(start)
	var exit *exec.ExitError
	code := 0
	switch {
	case ctx.Err() != nil && err != nil:
		rec.Error = fmt.Sprintf("command did not finish within %s", CommandTimeout)
	case errors.As(err, &exit) && exit.ExitCode() >= 0:
		code = exit.ExitCode()
	case err != nil:
		rec.Error = err.Error()
	case stdout.over:
		rec.Error = fmt.Sprintf("output larger than %d bytes", httpx.MaxBody)
	}
	if code != 0 || rec.Error != "" {
		// What the command said on standard error, for the reader of the
		// log; never compared. Its standard output is its body, which is
		// not shown: of a private step it is private, and of a public one
		// it has not been scanned for personal data yet (a log of CI is
		// read by more people than the repository).
		lines := strings.Split(strings.TrimRight(stderr.String(), "\n"), "\n")
		if len(lines) > 20 {
			lines = lines[len(lines)-20:]
		}
		fmt.Fprintf(r.log, "%s: %s (standard output not shown)\n%s\n", req.ID, procx.Line(req.Command), strings.Join(lines, "\n"))
	}
	if rec.Error != "" {
		return httpx.Result{Record: rec, Duration: dur}
	}
	rec.Exit = &code
	body := []byte{}
	if req.Output {
		body = stdout.Bytes()
		for _, pattern := range req.OutputIgnore {
			body = regexp.MustCompile(pattern).ReplaceAll(body, nil) // checked when the catalog was loaded
		}
	}
	return httpx.Result{Record: rec, Body: body, Duration: dur}
}
