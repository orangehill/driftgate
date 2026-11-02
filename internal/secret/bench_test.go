package secret

import (
	"bytes"
	"testing"
)

func benchKey() Key { return Key(bytes.Repeat([]byte{7}, KeySize)) }

// A capture seals and names every private body; most are a few kilobytes.
func BenchmarkSealSmall(b *testing.B) {
	k, body := benchKey(), bytes.Repeat([]byte("x"), 4<<10)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := k.Seal(body, []byte("ref")); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRefSmall(b *testing.B) {
	k, body := benchKey(), bytes.Repeat([]byte("x"), 4<<10)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		k.Ref(body)
	}
}
