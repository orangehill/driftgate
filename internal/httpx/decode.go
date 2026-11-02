package httpx

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/binary"
	"io"
	"strings"
)

// Decode undoes the Content-Encoding of a body, the last encoding applied
// first: gzip and deflate (zlib, or raw as some servers send it); identity
// and no encoding leave it as it is. The decoded body is capped at max, so
// a small body that expands without end is not read past it. ok is false
// for an encoding it cannot decode (br, zstd), a body that does not decode,
// or one over max. The buffer is sized from the size gzip records, so a
// large body is not grown into twice its size.
func Decode(body []byte, encoding string, max int64) (out []byte, ok bool) {
	if len(body) == 0 {
		return body, true
	}
	codings := strings.Split(encoding, ",")
	for i := len(codings) - 1; i >= 0; i-- {
		var r io.Reader
		var err error
		size := int64(len(body)) * 4 // a guess, grown as needed
		switch strings.ToLower(strings.TrimSpace(codings[i])) {
		case "", "identity":
			continue
		case "gzip", "x-gzip":
			r, err = gzip.NewReader(bytes.NewReader(body))
			if len(body) >= 4 {
				// The size of the last member, modulo 4 GiB: of one member
				// (as servers send it), the decoded size.
				size = int64(binary.LittleEndian.Uint32(body[len(body)-4:]))
			}
		case "deflate":
			if r, err = zlib.NewReader(bytes.NewReader(body)); err != nil {
				r, err = flate.NewReader(bytes.NewReader(body)), nil
			}
		default:
			return nil, false
		}
		if err != nil {
			return nil, false
		}
		b := bytes.NewBuffer(make([]byte, 0, min(size, max, maxPrealloc)+bytes.MinRead))
		if _, err = b.ReadFrom(io.LimitReader(r, max+1)); err != nil || int64(b.Len()) > max {
			return nil, false
		}
		body = b.Bytes()
	}
	return body, true
}
