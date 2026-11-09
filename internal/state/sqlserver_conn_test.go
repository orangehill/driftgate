package state

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
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

// negativeSerialCert is a self-signed certificate whose serial number is
// negative, as SQL Server 2017 makes its own: one made with serial 0x55,
// its serial's byte changed to 0xd5 (the signature no longer fits, which
// parsing does not check).
func negativeSerialCert(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(0x55), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	i := bytes.Index(der, []byte{0x02, 0x01, 0x55})
	if i < 0 {
		t.Fatal("no serial in the certificate")
	}
	der[i+2] = 0xd5
	return der
}

// TestNegativeSerialHelper is the process TestNegativeSerial starts with a
// GODEBUG of its own; it runs only from there. It prints whether the
// certificate parses before any component, after a MySQL and a PostgreSQL
// component, and after a SQL Server one, then GODEBUG.
func TestNegativeSerialHelper(t *testing.T) {
	if os.Getenv("DG_TEST_NEGATIVE_SERIAL") == "" {
		t.Skip("run by TestNegativeSerial")
	}
	der := negativeSerialCert(t)
	parses := func() bool { _, err := x509.ParseCertificate(der); return err == nil }
	before := parses()
	if c, err := NewMySQL(Options{Name: "m", DSN: "mysql://u:p@127.0.0.1:1/", Working: "w"}); err == nil {
		c.Close()
	}
	if c, err := NewPostgres(Options{Name: "p", DSN: "postgres://u:p@127.0.0.1:1/", Working: "w"}); err == nil {
		c.Close()
	}
	others := parses()
	s, err := NewSQLServer(Options{Name: "db", DSN: "sqlserver://u:p@127.0.0.1:1?encrypt=true", Working: "w"})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	fmt.Printf("RESULT %t %t %t %s\n", before, others, parses(), os.Getenv("GODEBUG"))
}

// TestNegativeSerial: a certificate with a negative serial number parses
// only once a SQL Server component is made (GODEBUG x509negativeserial=1,
// applied at run time), never with MySQL or PostgreSQL alone; the other
// GODEBUG settings stay, and so does a value of it the user set.
func TestNegativeSerial(t *testing.T) {
	for _, c := range []struct{ godebug, want string }{
		{"", "false false true x509negativeserial=1"},
		{"http2client=0", "false false true http2client=0,x509negativeserial=1"},
		{"x509negativeserial=0", "false false false x509negativeserial=0"},
		{"x509negativeserial=1", "true true true x509negativeserial=1"},
	} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestNegativeSerialHelper$", "-test.count=1", "-test.v")
		cmd.Env = append(os.Environ(), "DG_TEST_NEGATIVE_SERIAL=1", "GODEBUG="+c.godebug)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%q: %v\n%s", c.godebug, err, out)
		}
		_, got, _ := strings.Cut(string(out), "RESULT ")
		got, _, _ = strings.Cut(got, "\n")
		if got != c.want {
			t.Errorf("GODEBUG=%q: %q, want %q", c.godebug, got, c.want)
		}
	}
}

// TestSQLServerOthersTransition: an error 927 or 952 about another
// database of the server makes an operation run again, up to
// sqlServerOthersWait; one about a database of this component, or any
// other error, does not.
func TestSQLServerOthersTransition(t *testing.T) {
	s, err := NewSQLServer(Options{Name: "db", DSN: "sqlserver://u:p@127.0.0.1:1?encrypt=true", Working: "Shop", Pristine: "Shop_pristine"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	other := mssql.Error{Number: 927, Message: "Database 'other_pristine_dgnew' cannot be opened. It is in the middle of a restore."}
	for _, c := range []struct {
		err  error
		want bool
	}{
		{other, true},
		{fmt.Errorf("state db: restore: %w", other), true},
		// DropPristine's: a read of the copy's mark, and dropLeft's drop.
		{mssql.Error{Number: 927, Message: "Database 'dgt_cap_f36f692b_pristine_dgnew' cannot be opened. It is in the middle of a restore."}, true},
		{fmt.Errorf("state db: dropping Shop_pristine: %w", other), true},
		{fmt.Errorf("state db: dropping Shop_pristine: %w", mssql.Error{Number: 927, Message: "Database 'Shop_pristine' cannot be opened. It is in the middle of a restore."}), false},
		{mssql.Error{Number: 952, Message: "Database 'it''s' is in transition. Try the statement later."}, true},
		// Another database set SINGLE_USER, as a parallel seed's
		// <pristine>_dgnew before its rename or a drop: 924.
		{mssql.Error{Number: 924, Message: "Database 'dgt_ss_791297a2_p_dgnew' is already open and can only have one user at a time."}, true},
		{fmt.Errorf("state db: restore: %w", mssql.Error{Number: 924, Message: "Database 'other' is already open and can only have one user at a time."}), true},
		{mssql.Error{Number: 924, Message: "Database 'Shop' is already open and can only have one user at a time."}, false},
		{mssql.Error{Number: 924, Message: "Database 'Shop_pristine_dgnew' is already open and can only have one user at a time."}, false},
		{errors.Join(other, mssql.Error{Number: 924, Message: "Database 'Shop_pristine' is already open and can only have one user at a time."}), false},
		// Another database taken offline, as a restore of the whole
		// database does with its working database: 942. The working
		// database's own is not (workingOffline).
		{mssql.Error{Number: 942, Message: "Database 'dgt_ss_c1e97d93' cannot be opened because it is offline."}, true},
		{mssql.Error{Number: 942, Message: "Database 'Shop' cannot be opened because it is offline."}, false},
		{mssql.Error{Number: 942, Message: "Database 'shop_pristine' cannot be opened because it is offline."}, false},
		{mssql.Error{Number: 927, Message: "Database 'shop_pristine_dgnew' cannot be opened. It is in the middle of a restore."}, false},
		{mssql.Error{Number: 927, Message: "Database 'Shop' cannot be opened. It is in the middle of a restore."}, false},
		{mssql.Error{Number: 208, Message: "Database 'x'"}, false},
		{errors.New("Database 'x' is in transition"), false},
		// The name of the 927's own message, not one before it.
		{fmt.Errorf("restore of Database 'other': %w", mssql.Error{Number: 927, Message: "Database 'Shop' cannot be opened. It is in the middle of a restore."}), false},
		{mssql.Error{Number: 927, Message: "Database 'Shop' cannot be opened. It is in the middle of a restore.",
			All: []mssql.Error{{Number: 3013, Message: "Database 'other' stopped"}, {Number: 927, Message: "Database 'Shop' cannot be opened. It is in the middle of a restore."}}}, false},
		// Every 927 and 952 must name another database: the last message
		// (Number, Message) or an earlier one that names this component's,
		// in one error, joined or wrapped, is not retried.
		{mssql.Error{Number: 927, Message: "Database 'Shop' cannot be opened. It is in the middle of a restore.",
			All: []mssql.Error{{Number: 952, Message: "Database 'other' is in transition. Try the statement later."}, {Number: 927, Message: "Database 'Shop' cannot be opened. It is in the middle of a restore."}}}, false},
		{mssql.Error{Number: 952, Message: "Database 'other' is in transition. Try the statement later.",
			All: []mssql.Error{{Number: 927, Message: "Database 'Shop' cannot be opened. It is in the middle of a restore."}, {Number: 952, Message: "Database 'other' is in transition. Try the statement later."}}}, false},
		{errors.Join(other, mssql.Error{Number: 927, Message: "Database 'Shop' cannot be opened."}), false},
		{fmt.Errorf("x: %w", errors.Join(mssql.Error{Number: 927, Message: "Database 'Shop' cannot be opened."}, other)), false},
		{errors.Join(other, mssql.Error{Number: 952, Message: "Database 'another' is in transition."}), true},
		{&mssql.Error{Number: 927, Message: other.Message}, true},
		{&mssql.Error{Number: 927, Message: "Database 'Shop_pristine' cannot be opened."}, false},
		{mssql.Error{Number: 927, Message: "Database 'SHOP' cannot be opened."}, false},
		{mssql.Error{Number: 927, Message: "Database 'Shop_pristine_dgx' cannot be opened."}, false},
		{mssql.Error{Number: 952, Message: "is in transition",
			All: []mssql.Error{other, {Number: 952, Message: "is in transition"}}}, false},
	} {
		if _, got := s.othersTransition(c.err); got != c.want {
			t.Errorf("%v: %t", c.err, got)
		}
	}
	// A restore also takes a 942 or 927 about the working database, which
	// a stopped restore of the whole database left offline or restoring;
	// never one about another database of this component, nor another
	// error about the working database.
	for i, c := range []struct {
		err  error
		want bool
	}{
		{other, true},
		{mssql.Error{Number: 942, Message: "Database 'Shop' cannot be opened because it is offline."}, true},
		{fmt.Errorf("x: %w", mssql.Error{Number: 927, Message: "Database 'shop' cannot be opened. It is in the middle of a restore."}), true},
		{mssql.Error{Number: 924, Message: "Database 'Shop' is already open and can only have one user at a time."}, false},
		{mssql.Error{Number: 952, Message: "Database 'Shop' is in transition. Try the statement later."}, false},
		{mssql.Error{Number: 942, Message: "Database 'Shop_pristine' cannot be opened because it is offline."}, false},
		{mssql.Error{Number: 927, Message: "Database 'Shop_pristine_dgnew' cannot be opened. It is in the middle of a restore."}, false},
		{errors.Join(mssql.Error{Number: 942, Message: "Database 'Shop' cannot be opened because it is offline."}, mssql.Error{Number: 927, Message: "Database 'Shop_pristine_dgx' cannot be opened."}), false},
	} {
		if _, got := s.workingStopped(c.err); got != c.want {
			t.Errorf("workingStopped %v: %t", c.err, got)
		}
		if _, got := s.othersTransition(c.err); got != (i == 0) {
			t.Errorf("othersTransition %v: %t", c.err, got)
		}
	}
	// A failure without a reason right after another database's
	// transition is waited out as that one; on its own it is not.
	noReason := mssql.Error{Message: "Request failed but didn't provide reason"}
	if !sqlServerNoReason(fmt.Errorf("x: %w", noReason)) || sqlServerNoReason(other) || sqlServerNoReason(errors.New("Request failed but didn't provide reason")) {
		t.Error("sqlServerNoReason")
	}
	calls := 0
	err = s.retryOthers(context.Background(), func() error {
		switch calls++; calls {
		case 1:
			return other
		case 2:
			return noReason
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Errorf("no reason after a transition: %d calls, %v", calls, err)
	}
	calls = 0
	if err := s.retryOthers(context.Background(), func() error { calls++; return noReason }); err == nil || calls != 1 {
		t.Errorf("no reason alone: %d calls, %v", calls, err)
	}
	calls = 0
	err = s.retryOthers(context.Background(), func() error {
		if calls++; calls < 3 {
			return other
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Errorf("retried %d times: %v", calls, err)
	}
	calls = 0
	own := mssql.Error{Number: 927, Message: "Database 'Shop_pristine' cannot be opened."}
	if err := s.retryOthers(context.Background(), func() error { calls++; return own }); err == nil || calls != 1 {
		t.Errorf("own database: %d calls, %v", calls, err)
	}
	defer func(w time.Duration) { sqlServerOthersWait = w }(sqlServerOthersWait)
	sqlServerOthersWait = 50 * time.Millisecond
	calls = 0
	if err := s.retryOthers(context.Background(), func() error { calls++; return other }); err == nil || calls < 2 {
		t.Errorf("past the wait: %d calls, %v", calls, err)
	}
	// A first try that runs longer than the wait is still tried again:
	// the wait counts from the first failure.
	calls = 0
	err = s.retryOthers(context.Background(), func() error {
		if calls++; calls == 1 {
			time.Sleep(2 * sqlServerOthersWait)
			return other
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Errorf("a slow first try: %d calls, %v", calls, err)
	}
}

// TestSQLServerWorkingOffline: a fingerprint takes the working database
// for offline only from a 942 whose own message names it.
func TestSQLServerWorkingOffline(t *testing.T) {
	s, err := NewSQLServer(Options{Name: "db", DSN: "sqlserver://u:p@127.0.0.1:1?encrypt=true", Working: "Shop", Pristine: "Shop_pristine"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	offline := func(db string) mssql.Error {
		return mssql.Error{Number: 942, Message: "Database '" + db + "' cannot be opened because it is offline."}
	}
	for _, c := range []struct {
		err  error
		want bool
	}{
		{offline("Shop"), true},
		{fmt.Errorf("pinning: %w", offline("shop")), true},
		{offline("other"), false},
		{fmt.Errorf("Database 'Shop' x: %w", offline("other")), false},
		{mssql.Error{Number: 942, Message: offline("other").Message, All: []mssql.Error{{Number: 5701, Message: "Changed database context to 'Shop'."}, offline("other")}}, false},
		{mssql.Error{Number: 927, Message: "Database 'Shop' cannot be opened."}, false},
	} {
		if got := s.workingOffline(c.err); got != c.want {
			t.Errorf("%v: %t", c.err, got)
		}
	}
}

// TestSQLServerRestoreRetried: Restore runs a try again that a 942 or 927
// about the working database stopped (workingStopped), as one another
// database's transition stopped; never one about another database of
// this component. Without a database: each try is the hook's.
func TestSQLServerRestoreRetried(t *testing.T) {
	s, err := NewSQLServer(Options{Name: "db", DSN: "sqlserver://u:p@127.0.0.1:1?encrypt=true", Working: "Shop", Pristine: "Shop_pristine"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, c := range []struct {
		err   error
		tries int
	}{
		{mssql.Error{Number: 942, Message: "Database 'Shop' cannot be opened because it is offline."}, 2},
		{mssql.Error{Number: 927, Message: "Database 'Shop' cannot be opened. It is in the middle of a restore."}, 2},
		{mssql.Error{Number: 927, Message: "Database 'other' cannot be opened. It is in the middle of a restore."}, 2},
		{mssql.Error{Number: 942, Message: "Database 'Shop_pristine' cannot be opened because it is offline."}, 1},
		{mssql.Error{Number: 924, Message: "Database 'Shop' is already open and can only have one user at a time."}, 1},
	} {
		tries := 0
		s.hooks = &ssHooks{restore: func(context.Context, []string) error {
			if tries++; tries == 1 {
				return c.err
			}
			return nil
		}}
		err := s.Restore(context.Background(), nil)
		if want := c.tries == 2; tries != c.tries || (err == nil) != want {
			t.Errorf("%v: %d tries, %v", c.err, tries, err)
		}
	}
}

// TestSQLServerRetryGivesUp: after the wait, the error names the database
// that stayed in transition and says it may be so for good; an operation
// around one that gave up does not wait again.
func TestSQLServerRetryGivesUp(t *testing.T) {
	s, err := NewSQLServer(Options{Name: "db", DSN: "sqlserver://u:p@127.0.0.1:1?encrypt=true", Working: "Shop", Pristine: "Shop_pristine"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	defer func(w time.Duration) { sqlServerOthersWait = w }(sqlServerOthersWait)
	sqlServerOthersWait = 30 * time.Millisecond
	stuck := mssql.Error{Number: 927, Message: "Database 'left_restoring' cannot be opened. It is in the middle of a restore."}
	inner, outer := 0, 0
	err = s.retryOthers(context.Background(), func() error {
		outer++
		return s.retryOthers(context.Background(), func() error { inner++; return stuck })
	})
	if sqlServerNumber(err) != 927 || !strings.Contains(err.Error(), "left_restoring") || !strings.Contains(err.Error(), "for good") || outer != 1 || inner < 2 {
		t.Fatalf("%d outer, %d inner tries: %v", outer, inner, err)
	}
	if strings.Count(err.Error(), "tried again") != 1 {
		t.Fatalf("said twice: %v", err)
	}
	if _, ok := s.othersTransition(err); !ok {
		t.Fatalf("the 927 is still told: %v", err)
	}
	s.hooks = &ssHooks{restore: func(context.Context, []string) error {
		return mssql.Error{Number: 942, Message: "Database 'Shop' cannot be opened because it is offline."}
	}}
	if err := s.Restore(context.Background(), nil); !strings.Contains(err.Error(), "working database Shop stays offline or restoring") {
		t.Fatalf("working database: %v", err)
	}
}
