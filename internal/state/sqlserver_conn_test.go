package state

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTDSServer is a TCP server that answers every connection's PRELOGIN
// with answer (whole TDS packets, written in chunks of split bytes when
// split > 0), then records the type of the next packet the client sends
// (0 when it sends none): 0x10 is LOGIN7, the password in clear; 0x12 a
// TLS handshake.
type fakeTDSServer struct {
	addr string
	mu   sync.Mutex
	next []byte
}

func newFakeTDSServer(t *testing.T, answer []byte, split int) *fakeTDSServer {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeTDSServer{addr: l.Addr().String()}
	var wg sync.WaitGroup
	t.Cleanup(func() { l.Close(); wg.Wait() })
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				c.SetDeadline(time.Now().Add(10 * time.Second))
				if _, err := readTDSPacket(c); err != nil { // the client's PRELOGIN
					return
				}
				for b := answer; len(b) > 0; {
					n := len(b)
					if split > 0 && n > split {
						n = split
					}
					if _, err := c.Write(b[:n]); err != nil {
						return
					}
					b = b[n:]
				}
				typ, _ := readTDSPacket(c)
				f.mu.Lock()
				f.next = append(f.next, typ)
				f.mu.Unlock()
			}()
		}
	}()
	return f
}

// sent is the types of the packets clients sent after the PRELOGIN answer.
func (f *fakeTDSServer) sent() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.next...)
}

func readTDSPacket(c net.Conn) (byte, error) {
	h := make([]byte, tdsHeader)
	if _, err := io.ReadFull(c, h); err != nil {
		return 0, err
	}
	n := int(binary.BigEndian.Uint16(h[2:4]))
	if n < tdsHeader {
		return 0, errors.New("short packet")
	}
	_, err := io.ReadFull(c, make([]byte, n-tdsHeader))
	return h[0], err
}

// preloginAnswer is the body of a PRELOGIN answer with VERSION and the
// ENCRYPTION values given (one option each, in order).
func preloginAnswer(encrypt ...byte) []byte {
	n := 1 + len(encrypt)
	opts := make([]byte, 0, 5*n+1)
	data := []byte{16, 0, 0x10, 0x00, 0, 0} // VERSION
	at := 5*n + 1
	opts = append(opts, 0, byte(at>>8), byte(at), 0, 6)
	for i, e := range encrypt {
		o := at + 6 + i
		opts = append(opts, tdsOptEncryption, byte(o>>8), byte(o), 0, 1)
		data = append(data, e)
	}
	return append(append(opts, tdsOptTerminator), data...)
}

// tdsPackets frames a body as TDS reply packets of at most size bytes of
// data each, the last with status 1.
func tdsPackets(body []byte, size int) []byte {
	var out []byte
	for {
		n := min(len(body), size)
		status := byte(0)
		if n == len(body) {
			status = 1
		}
		h := []byte{tdsReply, status, 0, 0, 0, 0, 1, 0}
		binary.BigEndian.PutUint16(h[2:], uint16(tdsHeader+n))
		out = append(append(out, h...), body[:n]...)
		body = body[n:]
		if status == 1 {
			return out
		}
	}
}

// TestSQLServerNoTLSRefused: with encrypt=false go-mssqldb would send the
// password in clear to a server that answers ENCRYPT_NOT_SUP; DriftGate
// refuses the connection before the driver sends LOGIN7, however the
// answer is split.
func TestSQLServerNoTLSRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer []byte
		split  int
	}{
		{"one packet", tdsPackets(preloginAnswer(tdsEncryptNotSup), 4096), 0},
		{"byte by byte", tdsPackets(preloginAnswer(tdsEncryptNotSup), 4096), 1},
		{"three packets", tdsPackets(preloginAnswer(tdsEncryptNotSup), 7), 3},
		{"the last of two", tdsPackets(preloginAnswer(0, tdsEncryptNotSup), 4096), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeTDSServer(t, tc.answer, tc.split)
			for _, enc := range []string{"false", "true"} {
				s, err := NewSQLServer(Options{Name: "db", DSN: "sqlserver://app:Secret-pw@" + f.addr + "?encrypt=" + enc + "&TrustServerCertificate=true&dial+timeout=5", Working: "App", Pristine: "App_p"})
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				_, err = s.conn(ctx, "")
				cancel()
				s.Close()
				if err == nil {
					t.Fatalf("encrypt=%s: connected", enc)
				}
				if enc == "false" && !strings.Contains(err.Error(), "ENCRYPT_NOT_SUP") {
					t.Errorf("encrypt=%s: %v", enc, err)
				}
				if strings.Contains(err.Error(), "Secret-pw") {
					t.Errorf("the error shows the password: %v", err)
				}
			}
			for _, typ := range f.sent() {
				if typ != 0 {
					t.Fatalf("the client sent packet type %#x after ENCRYPT_NOT_SUP", typ)
				}
			}
		})
	}
}

// TestSQLServerTLSGuardPassesThrough: an answer the guard accepts reaches
// the driver unchanged, also split into packets and reads: the driver goes
// on to the TLS handshake (packet type 0x12), never to LOGIN7.
func TestSQLServerTLSGuardPassesThrough(t *testing.T) {
	f := newFakeTDSServer(t, tdsPackets(preloginAnswer(0), 9), 2) // ENCRYPT_OFF: TLS for the sign-in only
	s, err := NewSQLServer(Options{Name: "db", DSN: "sqlserver://app:pw@" + f.addr + "?encrypt=false&TrustServerCertificate=true", Working: "App", Pristine: "App_p"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.conn(ctx, ""); err == nil {
		t.Fatal("connected to a server that does no TLS")
	}
	if got := f.sent(); len(got) == 0 || got[0] != 0x12 {
		t.Fatalf("the driver sent %x after the PRELOGIN answer, want a TLS handshake (12)", got)
	}
}

func TestCheckSQLServerPrelogin(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
		ok   bool
	}{
		{"off", preloginAnswer(0), true},
		{"on", preloginAnswer(1), true},
		{"required", preloginAnswer(3), true},
		{"not supported", preloginAnswer(2), false},
		{"not supported first", preloginAnswer(2, 0), false},
		{"not supported last", preloginAnswer(0, 2), false},
		{"no encryption option", preloginAnswer(), false},
		{"empty", nil, false},
		{"terminator only", []byte{tdsOptTerminator}, false},
		{"no terminator", preloginAnswer(0)[:5], false},
		{"option past the end", []byte{tdsOptEncryption, 0, 200, 0, 1, tdsOptTerminator}, false},
		{"empty encryption", []byte{tdsOptEncryption, 0, 6, 0, 0, tdsOptTerminator, 2}, false},
		{"trace id skipped", append([]byte{tdsOptTraceID, 0xff, 0xff, 0xff, 0xff}, preloginAnswerShifted(5, 0)...), true},
		{"trace id then not supported", append([]byte{tdsOptTraceID, 0xff, 0xff, 0xff, 0xff}, preloginAnswerShifted(5, 2)...), false},
	} {
		if err := checkSQLServerPrelogin(tc.body); (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	if !errors.Is(checkSQLServerPrelogin(preloginAnswer(2)), errSQLServerNoTLS) {
		t.Error("ENCRYPT_NOT_SUP is not errSQLServerNoTLS")
	}
}

// preloginAnswerShifted is preloginAnswer(encrypt) for a body that has
// shift bytes of options before it.
func preloginAnswerShifted(shift int, encrypt byte) []byte {
	at := shift + 5 + 1
	return []byte{tdsOptEncryption, byte(at >> 8), byte(at), 0, 1, tdsOptTerminator, encrypt}
}

func TestReadSQLServerPrelogin(t *testing.T) {
	answer := tdsPackets(preloginAnswer(0), 4)
	got, err := readSQLServerPrelogin(strings.NewReader(string(answer) + "rest"))
	if err != nil || string(got) != string(answer) {
		t.Fatalf("%x, %v; want %x", got, err, answer)
	}
	bad := tdsPackets(preloginAnswer(0), 4096)
	bad[0] = 0x12
	if _, err := readSQLServerPrelogin(strings.NewReader(string(bad))); err == nil {
		t.Error("a packet that is no reply is accepted")
	}
	short := append([]byte(nil), answer...)
	binary.BigEndian.PutUint16(short[2:], 7)
	if _, err := readSQLServerPrelogin(strings.NewReader(string(short))); err == nil {
		t.Error("a packet shorter than its header is accepted")
	}
	if _, err := readSQLServerPrelogin(strings.NewReader(string(answer[:len(answer)-1]))); err == nil {
		t.Error("a cut answer is accepted")
	}
	endless := tdsPackets(make([]byte, 70000), 4000) // no ENCRYPTION, and longer than any answer
	if _, err := readSQLServerPrelogin(strings.NewReader(string(endless))); err == nil {
		t.Error("an endless answer is accepted")
	}
}

func TestSQLServerDialerStrictUnguarded(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for _, strict := range []bool{false, true} {
		c, err := sqlServerDialer{host: "127.0.0.1", strict: strict}.DialContext(context.Background(), "tcp", l.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		if _, guarded := c.(*sqlServerTLSGuard); guarded == strict {
			t.Errorf("strict %v: guarded %v", strict, guarded)
		}
		c.Close()
	}
}
