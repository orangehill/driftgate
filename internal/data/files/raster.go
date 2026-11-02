package files

import (
	"bytes"
	"compress/lzw"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"sync"
)

// The images are written by hand rather than with image/png, image/jpeg and
// image/gif: a flat image needs only a few fixed bytes per row or block, so
// any size is written in a stream, and the binary stays small.

// grey is the level of every placeholder pixel.
const grey = 0x80

var pngMagic = []byte("\x89PNG\r\n\x1a\n")

// rasterPlan reads the dimensions of an image. When the header cannot be
// read, the placeholder is a 1×1 image of the same type.
func rasterPlan(r io.ReaderAt, size int64, info *Info, opt Options) (plan, error) {
	var w, h int
	var err error
	switch info.Kind {
	case PNG:
		w, h, err = pngSize(r)
	case JPEG:
		w, h, err = jpegSize(r, size)
	case GIF:
		w, h, err = gifSize(r)
	case WebP:
		w, h, err = webpSize(r)
	}
	if err != nil {
		info.Fallback = fmt.Sprintf("%s header not readable: %v", info.Kind, err)
		w, h = 1, 1
	}
	if int64(w)*int64(h) > opt.maxPixels() {
		return nil, fmt.Errorf("%d×%d pixels: %w", w, h, ErrTooLarge)
	}
	info.Width, info.Height = w, h
	write := map[string]func(context.Context, io.Writer, int, int) error{
		PNG: writePNG, JPEG: writeJPEG, GIF: writeGIF, WebP: writeWebP,
	}[info.Kind]
	return func(ctx context.Context, dst io.Writer) error { return write(ctx, dst, w, h) }, nil
}

func readAt(r io.ReaderAt, off int64, n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := r.ReadAt(b, off); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return b, nil
}

var errDims = errors.New("dimensions out of range")

func checkDims(w, h, max int) (int, int, error) {
	if w <= 0 || h <= 0 || w > max || h > max {
		return 0, 0, errDims
	}
	return w, h, nil
}

// pngSize reads the IHDR chunk, which must come first.
func pngSize(r io.ReaderAt) (int, int, error) {
	b, err := readAt(r, 8, 16)
	if err != nil {
		return 0, 0, err
	}
	if string(b[4:8]) != "IHDR" {
		return 0, 0, errors.New("no IHDR chunk")
	}
	return checkDims(int(binary.BigEndian.Uint32(b[8:])), int(binary.BigEndian.Uint32(b[12:])), 1<<31-1)
}

// jpegSize walks the marker segments to the first start of frame, skipping
// each segment by its length.
func jpegSize(r io.ReaderAt, size int64) (int, int, error) {
	off := int64(2)
	for off+4 <= size {
		b, err := readAt(r, off, 2)
		if err != nil {
			return 0, 0, err
		}
		if b[0] != 0xFF {
			return 0, 0, errors.New("no marker where one was expected")
		}
		m := b[1]
		switch {
		case m == 0xFF: // fill byte
			off++
			continue
		case m == 0x01 || m >= 0xD0 && m <= 0xD7: // no length
			off += 2
			continue
		case m == 0xD9 || m == 0xDA:
			return 0, 0, errors.New("no frame header before the image data")
		}
		b, err = readAt(r, off+2, 2)
		if err != nil {
			return 0, 0, err
		}
		n := int64(binary.BigEndian.Uint16(b))
		if n < 2 {
			return 0, 0, errors.New("bad segment length")
		}
		if m >= 0xC0 && m <= 0xCF && m != 0xC4 && m != 0xC8 && m != 0xCC {
			b, err := readAt(r, off+5, 4)
			if err != nil {
				return 0, 0, err
			}
			return checkDims(int(binary.BigEndian.Uint16(b[2:])), int(binary.BigEndian.Uint16(b)), 65535)
		}
		off += 2 + n
	}
	return 0, 0, errors.New("no frame header")
}

func gifSize(r io.ReaderAt) (int, int, error) {
	b, err := readAt(r, 6, 4)
	if err != nil {
		return 0, 0, err
	}
	return checkDims(int(binary.LittleEndian.Uint16(b)), int(binary.LittleEndian.Uint16(b[2:])), 65535)
}

// webpSize reads the first chunk: a lossy (VP8), lossless (VP8L) or extended
// (VP8X) header.
func webpSize(r io.ReaderAt) (int, int, error) {
	b, err := readAt(r, 12, 18)
	if err != nil {
		return 0, 0, err
	}
	d := b[8:]
	switch string(b[:4]) {
	case "VP8 ":
		if !bytes.Equal(d[3:6], []byte{0x9D, 0x01, 0x2A}) {
			return 0, 0, errors.New("no VP8 start code")
		}
		return checkDims(int(binary.LittleEndian.Uint16(d[6:])&0x3FFF), int(binary.LittleEndian.Uint16(d[8:])&0x3FFF), 16383)
	case "VP8L":
		if d[0] != 0x2F {
			return 0, 0, errors.New("no VP8L signature")
		}
		v := binary.LittleEndian.Uint32(d[1:])
		return checkDims(int(v&0x3FFF)+1, int(v>>14&0x3FFF)+1, 16384)
	case "VP8X":
		w := int(d[4]) | int(d[5])<<8 | int(d[6])<<16
		h := int(d[7]) | int(d[8])<<8 | int(d[9])<<16
		return checkDims(w+1, h+1, 16384) // a still lossless image is at most 16384 wide
	}
	return 0, 0, fmt.Errorf("unknown chunk %q", b[:4])
}

// writePNG writes an 8-bit greyscale image.
func writePNG(ctx context.Context, w io.Writer, width, height int) error {
	var idat bytes.Buffer
	z, _ := zlib.NewWriterLevel(&idat, zlib.BestCompression)
	if err := pngRows(ctx, z, width, height); err != nil {
		return err
	}
	if err := z.Close(); err != nil {
		return err
	}
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr, uint32(width))
	binary.BigEndian.PutUint32(ihdr[4:], uint32(height))
	ihdr[8] = 8 // bit depth; colour type 0 (grey), no interlace
	var out bytes.Buffer
	out.Write(pngMagic)
	for _, c := range []struct {
		typ  string
		data []byte
	}{{"IHDR", ihdr}, {"IDAT", idat.Bytes()}, {"IEND", nil}} {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(c.data)))
		out.Write(n[:])
		crc := crc32.NewIEEE()
		crc.Write([]byte(c.typ))
		crc.Write(c.data)
		out.WriteString(c.typ)
		out.Write(c.data)
		binary.BigEndian.PutUint32(n[:], crc.Sum32())
		out.Write(n[:])
	}
	_, err := w.Write(out.Bytes())
	return err
}

// jpegHeader is a baseline greyscale JPEG up to the image data, with the
// dimensions at jpegDims. Both Huffman tables hold a single 1-bit code for
// symbol 0, so every 8×8 block is two zero bits: DC difference 0, then end
// of block. A zero DC coefficient is level 128 after the level shift.
var jpegHeader = sync.OnceValue(func() []byte {
	var b bytes.Buffer
	b.Write([]byte{0xFF, 0xD8})                                                          // SOI
	b.Write([]byte{0xFF, 0xE0, 0, 16, 'J', 'F', 'I', 'F', 0, 1, 1, 0, 0, 1, 0, 1, 0, 0}) // APP0 JFIF, no thumbnail
	b.Write(append([]byte{0xFF, 0xDB, 0, 67, 0}, bytes.Repeat([]byte{1}, 64)...))        // DQT
	b.Write([]byte{0xFF, 0xC0, 0, 11, 8, 0, 0, 0, 0, 1, 1, 0x11, 0})                     // SOF0, 1 component
	b.Write(append([]byte{0xFF, 0xC4, 0, 20, 0x00, 1}, append(make([]byte, 15), 0)...))  // DHT DC 0
	b.Write(append([]byte{0xFF, 0xC4, 0, 20, 0x10, 1}, append(make([]byte, 15), 0)...))  // DHT AC 0
	b.Write([]byte{0xFF, 0xDA, 0, 8, 1, 1, 0x00, 0, 63, 0})                              // SOS
	return b.Bytes()
})

const jpegDims = 2 + 18 + 69 + 5 // offset of the height in SOF0

func writeJPEG(ctx context.Context, w io.Writer, width, height int) error {
	head := bytes.Clone(jpegHeader())
	binary.BigEndian.PutUint16(head[jpegDims:], uint16(height))
	binary.BigEndian.PutUint16(head[jpegDims+2:], uint16(width))
	if _, err := w.Write(head); err != nil {
		return err
	}
	bits := 2 * int64((width+7)/8) * int64((height+7)/8)
	if err := fill(ctx, w, bits/8, nil); err != nil {
		return err
	}
	tail := []byte{0xFF, 0xD9}
	if r := bits % 8; r > 0 {
		tail = []byte{0xFF >> r, 0xFF, 0xD9} // pad with one bits
	}
	_, err := w.Write(tail)
	return err
}

// writeGIF writes a two-colour image of index 0, LZW-coded as GIF requires.
func writeGIF(ctx context.Context, w io.Writer, width, height int) error {
	var hdr bytes.Buffer
	hdr.WriteString("GIF89a")
	binary.Write(&hdr, binary.LittleEndian, [2]uint16{uint16(width), uint16(height)})
	hdr.Write([]byte{0x80, 0, 0, grey, grey, grey, grey, grey, grey}) // global table of 2 colours
	hdr.WriteByte(0x2C)
	binary.Write(&hdr, binary.LittleEndian, [4]uint16{0, 0, uint16(width), uint16(height)})
	hdr.Write([]byte{0, 2}) // no local table; LZW minimum code size
	if _, err := w.Write(hdr.Bytes()); err != nil {
		return err
	}
	bw := &gifBlocks{w: w}
	lw := lzw.NewWriter(bw, lzw.LSB, 2)
	if err := fill(ctx, lw, int64(width)*int64(height), nil); err != nil {
		return err
	}
	if err := lw.Close(); err != nil {
		return err
	}
	if err := bw.flush(); err != nil {
		return err
	}
	_, err := w.Write([]byte{0, 0x3B}) // block terminator, trailer
	return err
}

// gifBlocks splits data into sub-blocks of at most 255 bytes.
type gifBlocks struct {
	w   io.Writer
	buf [256]byte
	n   int
}

func (g *gifBlocks) Write(p []byte) (int, error) {
	done := 0
	for done < len(p) {
		k := copy(g.buf[1+g.n:], p[done:])
		g.n += k
		done += k
		if g.n == 255 {
			if err := g.flush(); err != nil {
				return done - k, err
			}
		}
	}
	return len(p), nil
}

func (g *gifBlocks) flush() error {
	if g.n == 0 {
		return nil
	}
	g.buf[0] = byte(g.n)
	_, err := g.w.Write(g.buf[:1+g.n])
	g.n = 0
	return err
}

// writeWebP writes a lossless (VP8L) image without transforms or colour
// cache, whose five prefix codes each hold a single symbol: such a code takes
// no bits, so the pixels take none at all.
func writeWebP(_ context.Context, w io.Writer, width, height int) error {
	var bw bitWriter
	bw.write(0x2F, 8)
	bw.write(uint32(width-1), 14)
	bw.write(uint32(height-1), 14)
	bw.write(0, 1)                                            // alpha unused
	bw.write(0, 3)                                            // version
	bw.write(0, 1)                                            // no transform
	bw.write(0, 1)                                            // no colour cache
	bw.write(0, 1)                                            // no meta prefix codes
	for _, sym := range []uint32{grey, grey, grey, 0xFF, 0} { // green, red, blue, alpha, distance
		bw.write(1, 1) // simple code
		bw.write(0, 1) // one symbol
		bw.write(1, 1) // of 8 bits
		bw.write(sym, 8)
	}
	data := bw.bytes()
	var out bytes.Buffer
	pad := len(data) % 2
	out.WriteString("RIFF")
	binary.Write(&out, binary.LittleEndian, uint32(4+8+len(data)+pad))
	out.WriteString("WEBPVP8L")
	binary.Write(&out, binary.LittleEndian, uint32(len(data)))
	out.Write(data)
	if pad == 1 {
		out.WriteByte(0)
	}
	_, err := w.Write(out.Bytes())
	return err
}

// bitWriter packs bits least significant first.
type bitWriter struct {
	buf  []byte
	acc  uint64
	nacc uint
}

func (b *bitWriter) write(v uint32, n uint) {
	b.acc |= uint64(v) << b.nacc
	b.nacc += n
	for b.nacc >= 8 {
		b.buf = append(b.buf, byte(b.acc))
		b.acc >>= 8
		b.nacc -= 8
	}
}

func (b *bitWriter) bytes() []byte {
	if b.nacc > 0 {
		return append(b.buf, byte(b.acc))
	}
	return b.buf
}

// pngRows writes the rows, each a filter byte (none) and the pixels, to the
// compressor in writes of about 64 KiB: many short rows at once, or a long
// row in parts. The compressed bytes do not depend on how they are split,
// but the compressor's cost grows with the number of writes.
func pngRows(ctx context.Context, z io.Writer, width, height int) error {
	const chunk = 64 << 10
	if width < chunk {
		rows := chunk / (width + 1)
		buf := bytes.Repeat(append([]byte{0}, bytes.Repeat([]byte{grey}, width)...), rows)
		for y := 0; y < height; y += rows {
			if err := ctx.Err(); err != nil {
				return err
			}
			z.Write(buf[:min(rows, height-y)*(width+1)])
		}
	} else {
		row := bytes.Repeat([]byte{grey}, chunk)
		for y := 0; y < height; y++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			z.Write([]byte{0})
			for n := width; n > 0; n -= len(row) {
				z.Write(row[:min(n, len(row))])
			}
		}
	}
	return nil
}
