package httpx

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/orangehill/driftgate/internal/catalog"
)

// A body of unknown length reads whole at every size around the small
// limit, small bodies on the heap and the others in a region that Release
// gives back (on the heap too where there are no regions, Windows), with
// room for the tag of an encryption in place either way; a read that fails (a chunked body cut off) is an error, not
// a short body, and leaves no region behind.
func TestReadOffHeap(t *testing.T) {
	for _, n := range []int{0, 1, smallBody - 1, smallBody, smallBody + 1, 3<<20 + 7} {
		want := make([]byte, n)
		for i := range want {
			want[i] = byte(i * 7)
		}
		got, reg, err := readBody(iotest.HalfReader(bytes.NewReader(want)), -1, true)
		if err != nil || !bytes.Equal(got, want) || got == nil {
			t.Fatalf("%d bytes: got %d, %v", n, len(got), err)
		}
		if cap(got) < n+16 {
			t.Fatalf("%d bytes: no room for the tag of the encryption in place (cap %d)", n, cap(got))
		}
		if (reg != nil) != (mapped && n >= smallBody) || (n < smallBody && cap(got) > n+bytes.MinRead) {
			t.Fatalf("%d bytes: region %v, cap %d", n, reg != nil, cap(got))
		}
		if Unreleased() != btoi(reg != nil) {
			t.Fatalf("%d bytes: %d regions held", n, Unreleased())
		}
		Result{region: reg}.Release()
		Result{region: reg}.Release() // a second release does nothing
		if Unreleased() != 0 {
			t.Fatalf("%d bytes: %d regions held after release", n, Unreleased())
		}
	}
	for _, n := range []int{smallBody / 2, 3 * smallBody} {
		cut := io.MultiReader(bytes.NewReader(make([]byte, n)), iotest.ErrReader(io.ErrUnexpectedEOF))
		if _, _, err := readBody(cut, -1, true); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("cut body of %d bytes: %v", n, err)
		}
		if Unreleased() != 0 {
			t.Fatalf("cut body: %d regions held", Unreleased())
		}
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Through the client: a chunked body, a body with a length and a small
// chunked body come back as sent; the large ones need Release (where
// there are regions).
func TestDoOffHeap(t *testing.T) {
	large := bytes.Repeat([]byte(`{"a":"b"},`), 300<<10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/length":
			w.Header().Set("Content-Length", strconv.Itoa(len(large)))
			w.Write(large)
		case "/small":
			w.Write(large[:100])
		default:
			w.(http.Flusher).Flush() // chunked
			w.Write(large)
		}
	}))
	defer srv.Close()
	c, err := New(Options{BaseURL: srv.URL, OffHeap: true, Allow: mustAllow(t)})
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string][]byte{"/chunked": large, "/length": large, "/small": large[:100]} {
		res := c.Do(context.Background(), catalog.Request{ID: "x", Method: "GET", Path: path}, "")
		if res.Record.Error != "" || !bytes.Equal(res.Body, want) {
			t.Fatalf("%s: error %q, %d bytes", path, res.Record.Error, len(res.Body))
		}
		if held := Unreleased(); held != btoi(mapped && path != "/small") {
			t.Fatalf("%s: %d regions held", path, held)
		}
		res.Release()
	}
	if Unreleased() != 0 {
		t.Fatalf("%d regions held", Unreleased())
	}
}

func mustAllow(t *testing.T) *Allow {
	a, err := NewAllow(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// A server's Content-Length is not taken on trust: one over MaxBody is an
// error before anything is read, and one the body does not live up to
// takes no memory for the bytes that never come, on the heap or off it.
func TestLyingContentLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := MaxBody
		if r.URL.Path == "/over" {
			n++
		}
		w.Header().Set("Content-Length", strconv.Itoa(n))
		w.Write([]byte(`{"a":1}`))
	}))
	defer srv.Close()
	for _, offHeap := range []bool{true, false} {
		c, err := New(Options{BaseURL: srv.URL, OffHeap: offHeap, Allow: mustAllow(t)})
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"/over", "/short"} {
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			res := c.Do(context.Background(), catalog.Request{ID: "x", Method: "GET", Path: path}, "")
			runtime.ReadMemStats(&after)
			res.Release()
			if res.Record.Error == "" {
				t.Fatalf("%s (off heap %v): no error", path, offHeap)
			}
			if path == "/over" && !strings.Contains(res.Record.Error, "larger than") {
				t.Fatalf("%s: %s", path, res.Record.Error)
			}
			if grew := after.TotalAlloc - before.TotalAlloc; grew > 16<<20 {
				t.Fatalf("%s (off heap %v): %d MiB allocated for a body of 7 bytes", path, offHeap, grew>>20)
			}
		}
	}
	if Unreleased() != 0 {
		t.Fatalf("%d regions held", Unreleased())
	}
}

// Decode reads gzip into a buffer of the size gzip records, not grown to
// twice the body; a body over the cap or in an unknown encoding is not ok.
func TestDecode(t *testing.T) {
	plain := bytes.Repeat([]byte(`{"name":"x","id":12345},`), 100<<10)
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	w.Write(plain)
	w.Close()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	out, ok := Decode(gz.Bytes(), "gzip", MaxBody)
	runtime.ReadMemStats(&after)
	if !ok || !bytes.Equal(out, plain) {
		t.Fatalf("gzip: ok %v, %d bytes", ok, len(out))
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > uint64(len(plain))+1<<20 {
		t.Fatalf("decoding %d bytes took %d", len(plain), grew)
	}
	if _, ok := Decode(gz.Bytes(), "gzip", int64(len(plain)-1)); ok {
		t.Fatal("a body over the cap decoded")
	}
	if _, ok := Decode(gz.Bytes(), "br", MaxBody); ok {
		t.Fatal("br decoded")
	}
	if out, ok := Decode(plain, "identity", MaxBody); !ok || len(out) != len(plain) {
		t.Fatal("identity")
	}
}

// A released mapping is kept for the next large body, whose pages are then
// there already: not one of a body over keepBytes, nor while another body
// is held. FreePool gives it back.
func TestRegionPool(t *testing.T) {
	if !mapped {
		t.Skip("no regions on this system")
	}
	FreePool()
	body := bytes.Repeat([]byte("x"), 1<<20)
	read := func(n int, length int64) *region {
		t.Helper()
		got, reg, err := readBody(bytes.NewReader(body[:n]), length, true)
		if err != nil || reg == nil || len(got) != n {
			t.Fatal(err)
		}
		return reg
	}
	reg := read(len(body), int64(len(body)))
	m := reg.m
	reg.free()
	if pool.kept != m {
		t.Fatal("not kept")
	}
	again := read(200<<10, -1)
	if again.m != m || pool.kept != nil {
		t.Fatal("not reused")
	}
	other := read(200<<10, -1) // while again is held: a mapping of its own
	other.free()
	if pool.kept != nil {
		t.Fatal("kept while another body is held")
	}
	again.free()
	big := make([]byte, keepBytes+1)
	_, huge, _ := readBody(bytes.NewReader(big), int64(len(big)), true)
	huge.free()
	if pool.kept != nil {
		t.Fatal("the mapping of a body over keepBytes is kept")
	}
	reg = read(len(body), -1)
	reg.free()
	FreePool()
	if pool.kept != nil || Unreleased() != 0 {
		t.Fatal("FreePool kept the mapping")
	}
}
