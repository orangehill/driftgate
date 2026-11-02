package snapshot

import (
	"bytes"
	"crypto/rand"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/orangehill/driftgate/internal/secret"
)

func benchWrite(b *testing.B, storage string, size int) {
	k := make([]byte, secret.KeySize)
	rand.Read(k)
	dir := b.TempDir()
	s := &Set{Dir: filepath.Join(dir, "set"), Private: &PrivateStore{Dir: filepath.Join(dir, "private"), Key: func() (secret.Key, error) { return k, nil }}}
	body := bytes.Repeat([]byte("x"), size)
	b.SetBytes(int64(size))
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		i++
		copy(body, strconv.Itoa(i)) // a new body each time
		rec := &Record{ID: "r" + strconv.Itoa(i%50), Headers: [][2]string{}, Body: Body{Storage: storage}}
		if err := s.Write(rec, body); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWritePublicSmall(b *testing.B)  { benchWrite(b, Public, 4<<10) }
func BenchmarkWritePrivateSmall(b *testing.B) { benchWrite(b, Private, 4<<10) }
func BenchmarkWritePublicLarge(b *testing.B)  { benchWrite(b, Public, 4<<20) }
