package data

import (
	"cmp"
	"encoding/base64"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/orangehill/driftgate/internal/pii"
)

// testIndex builds the checks over the given originals.
func testIndex(t *testing.T, allow []string, originals ...PersonalValue) *leakIndex {
	t.Helper()
	det, err := pii.New(allow)
	if err != nil {
		t.Fatal(err)
	}
	x, err := newLeakIndex(testKey, det)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range originals {
		if v.Category == "" {
			v.Category = CatText
		}
		if err := x.add(v); err != nil {
			t.Fatal(err)
		}
	}
	return x
}

// found scans one text value and lists the findings as check:kind.
func found(x *leakIndex, text string) []string {
	s := x.scanner("test")
	s.value("t", "c", "id=1", text, true)
	var out []string
	for _, f := range s.findings() {
		out = append(out, f.Check+":"+f.Kind)
	}
	return out
}

func TestLeakCheckFindsOriginals(t *testing.T) {
	x := testIndex(t, nil,
		PersonalValue{Table: "users", Column: "name", Kind: StrategyUniversal, Value: "Šćepan Žižić"},
		PersonalValue{Table: "users", Column: "name", Kind: StrategyUniversal, Value: "Јовановић Љубица"},
		PersonalValue{Table: "users", Column: "name", Kind: StrategyUniversal, Value: "Ana Ilić"},
		PersonalValue{Table: "users", Column: "email", Kind: StrategyEmail, Value: "Pera.Peric@Firma-X.example"},
		PersonalValue{Table: "users", Column: "address", Kind: StrategyUniversal, Value: "Bulevar kralja Aleksandra 73, stan 5"},
		PersonalValue{Table: "users", Column: "zip", Kind: StrategyUniversal, Value: "11000"},
		PersonalValue{Table: "users", Column: "account", Kind: StrategyToken, Value: "160-0000012345678-21"},
	)
	for _, c := range []struct {
		text string
		want string // "" for nothing
	}{
		{"Šćepan Žižić", "leak:universal"},
		{"Written by: ŠĆEPAN ŽIŽIĆ, 2024.", "leak:universal"},
		{"scepan-zizic", "leak:universal"},                               // a slug: no case, no diacritics
		{"Шћепан Жижић", "leak:universal"},                               // the other script
		{"Šćepan\nŽižić", "leak:universal"},                              // split across lines
		{"S\u030cc\u0301epan Z\u030ciz\u030cic\u0301", "leak:universal"}, // decomposed diacritics
		{"Ljubica Jovanović won an award", "leak:universal/word"},        // other order: by words
		{"Mr Jovanović called.", "leak:universal/word"},                  // the surname alone
		{"Žižićeva ulica", ""},                                           // inside a longer word
		{"Ana Ilić", "leak:universal"},                                   // seven letters and digits
		{"Ilić", ""},                                                     // a word shorter than minLeak
		{"pisati na pera.peric@firma-x.example", "detector:email,leak:email"},
		{`{"note": "pi\u0161ite: Pera.Peric@Firma-X.example", "by": "\u0160\u0107epan \u017di\u017ei\u0107"}`, "detector:email,leak:email,leak:universal"},
		{"&#352;&#263;epan &Zcaron;i&zcaron;i&#263;", "leak:universal"},
		{"<a href=\"mailto:Pera.Peric%40Firma-X.example\">", "detector:email,leak:email"},
		{"Bulevar kralja Aleksandra 73, stan 5", "leak:universal"},
		{"Bulevar kralja Aleksandra", ""}, // words of an address are not looked for alone
		{"11000 Beograd", ""},             // too few digits
		{"account 160-0000012345678-21", "leak:token"},
		{"Aleksandra", ""},
	} {
		got := strings.Join(found(x, c.text), ",")
		if got != c.want {
			t.Errorf("%q: %q, want %q", c.text, got, c.want)
		}
	}
}

func TestLeakCheckReportsWhereNotWhat(t *testing.T) {
	x := testIndex(t, nil, PersonalValue{Table: "users", Column: "name", Kind: StrategyUniversal, Value: "Petar Petrović"})
	s := x.scanner("dump")
	for i := 1; i <= 8; i++ {
		s.value("posts", "body", fmt.Sprintf("id=%d", i), "Author: Petar Petrović", true)
	}
	s.value("posts", "body", "id=9", "nista", true)
	fs := s.findings()
	if len(fs) != 1 {
		t.Fatalf("%+v", fs)
	}
	f := fs[0]
	if f.Check != CheckLeak || f.Table != "posts" || f.Column != "body" || f.Origin != "users.name" || f.Values != 8 || len(f.Rows) != maxRows || f.Rows[0] != "id=1" {
		t.Fatalf("%+v", f)
	}
	if b := fmt.Sprintf("%+v", fs); strings.Contains(strings.ToLower(b), "petrovi") {
		t.Fatalf("a value in the finding: %s", b)
	}
	if strings.Contains(fmt.Sprintf("%v", x.origins), "Petrovi") {
		t.Fatal("the index holds a value")
	}
}

func TestDetectorAllowsStandIns(t *testing.T) {
	r := newTestReplacer(t, testKey)
	ok := okf(t)
	phone := "+381 64 123 4567"
	x := testIndex(t, nil,
		PersonalValue{Table: "users", Column: "phone", Kind: StrategyUniversal, Value: phone},
		PersonalValue{Table: "users", Column: "bio", Kind: "phone", Value: "+381 11 222 3333"},
		PersonalValue{Table: "users", Column: "password", Kind: StrategyPassword, Value: "$2y$10$abcdefghijklmnopqrstuuAbCdEfGhIjKlMnOpQrStUvWxYz01234"},
	)
	standIn := ok(r.Universal(phone))
	inText := ok(r.Universal("+381 11 222 3333"))
	hash, _, _ := r.Password("$2y$10$abcdefghijklmnopqrstuuAbCdEfGhIjKlMnOpQrStUvWxYz01234")
	for _, c := range []struct{ text, want string }{
		{standIn, ""},
		{"zovite " + inText + " posle 5", ""},
		{"zovite " + phone, "detector:phone,leak:universal"},
		{"tel. +381 63 765 4321", "detector:phone"}, // a number that is no original
		{"vaxbkp.user1@qwerty-asdf.invalid", ""},
		{"neko@drugi-mejl.example", "detector:email"},
		{hash, ""},
		{`{"phone": "` + standIn + `"}`, ""},
	} {
		got := strings.Join(found(x, c.text), ",")
		if got != c.want {
			t.Errorf("%q: %q, want %q", c.text, got, c.want)
		}
	}
	if !isTestPasswordHash(hash) || isTestPasswordHash("$2y$10$abcdefghijklmnopqrstuuAbCdEfGhIjKlMnOpQrStUvWxYz01234") {
		t.Fatal("isTestPasswordHash")
	}
}

func TestVerifyAllowPatterns(t *testing.T) {
	x := testIndex(t, []string{`info@firma\.example`, `(?i)Beograd`},
		PersonalValue{Table: "users", Column: "email", Kind: StrategyEmail, Value: "info@firma.example"},
		PersonalValue{Table: "users", Column: "city", Kind: StrategyUniversal, Value: "Beograd"},
	)
	for _, text := range []string{"write to info@firma.example", "BEOGRAD, 2024"} {
		if got := found(x, text); len(got) != 0 {
			t.Errorf("%q: %v", text, got)
		}
	}
}

func TestStatementsAndRows(t *testing.T) {
	mysqlDump := "-- DriftGate anonymized data\n\nSET NAMES utf8mb4;\n" +
		"CREATE TABLE `t` (\n  `id` int NOT NULL COMMENT 'it''s; a \\'key\\'',\n  `v` text -- a comment; with ' a quote\n);\n" +
		"DELIMITER ;;\nCREATE TRIGGER x AFTER INSERT ON t FOR EACH ROW BEGIN\n  SET @a = 1; /* ; ' */\nEND;;\nDELIMITER ;\n" +
		"INSERT INTO `t` (`id`, `v`) VALUES\n(1, 'a\\'b;\\nc\\\\'),\n(2, NULL),\n(3, X'00ff41'),\n(4, -1.5e3);\n"
	st := newStatements(strings.NewReader(mysqlDump), MySQL)
	var got []string
	for {
		q, err := st.next()
		if err != nil {
			break
		}
		got = append(got, q)
	}
	if len(got) != 4 || !strings.HasPrefix(got[2], "CREATE TRIGGER") || !strings.HasSuffix(got[2], "END") || !strings.HasPrefix(got[3], "INSERT") {
		t.Fatalf("%d statements: %q", len(got), got)
	}
	_, rows, _ := strings.Cut(got[3], " VALUES\n")
	var vals []string
	if err := parseRows(rows, 2, MySQL, func(v [][]byte) {
		for _, b := range v {
			if b == nil {
				vals = append(vals, "<nil>")
			} else {
				vals = append(vals, string(b))
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{"1", "a'b;\nc\\", "2", "<nil>", "3", "\x00\xffA", "4", "-1.5e3"}
	if fmt.Sprint(vals) != fmt.Sprint(want) {
		t.Fatalf("%q, want %q", vals, want)
	}

	pgDump := "CREATE FUNCTION f() RETURNS trigger LANGUAGE plpgsql AS $fn$\nBEGIN\n  RAISE NOTICE 'a;b';\nEND\n$fn$;\n" +
		"CREATE TABLE \"t;x\" (v text DEFAULT E'\\';');\n" +
		"INSERT INTO \"t;x\" (\"v\") VALUES\n('it''s\nfine'),\n('\\x00');\n"
	st = newStatements(strings.NewReader(pgDump), Postgres)
	got = nil
	for {
		q, err := st.next()
		if err != nil {
			break
		}
		got = append(got, q)
	}
	if len(got) != 3 || !strings.HasSuffix(got[0], "$fn$") || !strings.HasPrefix(got[2], "INSERT") {
		t.Fatalf("%d statements: %q", len(got), got)
	}
	_, rows, _ = strings.Cut(got[2], " VALUES\n")
	vals = nil
	if err := parseRows(rows, 1, Postgres, func(v [][]byte) { vals = append(vals, string(v[0])) }); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(vals) != fmt.Sprint([]string{"it's\nfine", `\x00`}) {
		t.Fatalf("%q", vals)
	}
	if _, err := newStatements(strings.NewReader("INSERT INTO t VALUES ('open"), MySQL).next(); err == nil {
		t.Fatal("an unterminated string was accepted")
	}
}

// TestVerifyScanSpeed measures the scan of text without findings over an
// index of originals: DRIFTGATE_TEST_SCAN_ORIGINALS sets how many names
// (each with an e-mail address; default 5000), go test -run ScanSpeed -v.
func TestVerifyScanSpeed(t *testing.T) {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	var originals []PersonalValue
	n := 5_000
	if v, err := strconv.Atoi(os.Getenv("DRIFTGATE_TEST_SCAN_ORIGINALS")); err == nil && v > 0 {
		n = v
	}
	for i := 0; i < n; i++ {
		originals = append(originals,
			PersonalValue{Table: "users", Column: "name", Kind: StrategyUniversal, Value: fmt.Sprintf("Ime%d Prezimenović%d", i, i)},
			PersonalValue{Table: "users", Column: "email", Kind: StrategyEmail, Value: fmt.Sprintf("user%d@primer.example", i)})
	}
	start := time.Now()
	x := testIndex(t, nil, originals...)
	built := time.Since(start)
	originals = nil
	runtime.GC()
	runtime.ReadMemStats(&after)
	perValue := float64(after.HeapAlloc-before.HeapAlloc) / float64(x.count())
	text := strings.Repeat("This is an ordinary news story about an event in Čačak and Niš, with the number 2024 and a link https://primer.example/news/12345. ", 10)
	s := x.scanner("test")
	start = time.Now()
	const values = 20_000
	for i := 0; i < values; i++ {
		s.value("news", "body", "", text, true)
	}
	d := time.Since(start)
	if fs := s.findings(); len(fs) != 0 {
		t.Fatalf("%+v", fs)
	}
	mb := float64(len(text)*values) / (1 << 20)
	t.Logf("index of %d originals in %s, %.0f bytes each; scanned %.0f MiB in %s (%.1f MiB/s)", x.count(), built.Round(time.Millisecond), perValue, mb, d.Round(time.Millisecond), mb/d.Seconds())
}

func TestLeakCheckFindsEncodedAndInflected(t *testing.T) {
	x := testIndex(t, nil,
		PersonalValue{Table: "users", Column: "name", Kind: StrategyUniversal, Value: "Ana Petrović"},
		PersonalValue{Table: "users", Column: "last_name", Kind: StrategyUniversal, Value: "Jovana"},
		PersonalValue{Table: "users", Column: "email", Kind: StrategyEmail, Value: "ana.petrovic@posta.example"},
		PersonalValue{Table: "users", Column: "phone", Kind: StrategyUniversal, Value: "641234567", Category: CatNumeric},
	)
	esc := `ć`
	b64 := base64.StdEncoding.EncodeToString([]byte("Ana Petrović ana.petrovic@posta.example"))
	for _, c := range []struct{ text, want string }{
		{"greetings to Petroviću and Ani", "leak:universal/word"},
		{"razgovor sa Jovanom", "leak:universal"},
		{"payload " + b64, "detector:email,leak:email,leak:universal"},
		{`payload: {"name":"Petrovi` + esc + `"}`, "leak:universal/word"},
		{`{"p":"{\"name\":\"Petrovi\\u0107\"}"}`, "leak:universal/word"}, // JSON in a JSON string
		{"Petrovi&amp;#263;", "leak:universal/word"},
		{"AnaPetrovic", "leak:universal/word"},
		{"tel 0641234567", "detector:phone,leak:universal"}, // a mobile number without separators
		{"tel +381 64 123 4567", "detector:phone,leak:universal"},
		{"tel 064/123-4567", "detector:phone,leak:universal"},
		{"Markovićeva i Petrovićeva ulica", ""},
		{"Jovanović", ""},
		{"broj 96412345678", ""},
		{"YWJjZGVmZ2hpamtsbW5vcA==", ""}, // base64 of text without personal data
	} {
		got := strings.Join(found(x, c.text), ",")
		if got != c.want {
			t.Errorf("%q: %q, want %q", c.text, got, c.want)
		}
	}
}

func TestDetectorReadsJSONNumbersByTheirKey(t *testing.T) {
	r := newTestReplacer(t, testKey)
	ok := okf(t)
	x := testIndex(t, nil, PersonalValue{Table: "users", Column: "settings", Kind: StrategyUniversal, Value: "381641234567", Category: CatNumeric})
	for _, c := range []struct{ text, want string }{
		{`{"jmbg": 1501990710008}`, "detector:jmbg"}, // by its key: the checksum is not needed
		{`{"contact": {"phone": 381111234567}}`, "detector:phone"},
		{`{"phone": ` + ok(r.Number("381641234567")) + `}`, ""}, // its stand-in
		{`{"amount": 1501990710008, "id": 12, "created": 1701012345678}`, ""},
	} {
		if got := strings.Join(found(x, c.text), ","); got != c.want {
			t.Errorf("%s: %q, want %q", c.text, got, c.want)
		}
	}
}

func TestDetectorReadsSchemaStatements(t *testing.T) {
	x := testIndex(t, nil)
	rules, err := ParseRules([]byte("version: 1\nschema:\n  view vip: [email]\n"))
	if err != nil {
		t.Fatal(err)
	}
	x.rules = rules
	s := x.scanner("dump")
	for _, st := range schemaWithLiterals().texts() {
		s.value(schemaTable, st.object, "", st.sql, true)
	}
	fs := s.findings()
	if len(fs) != 1 || fs[0].Table != schemaTable || fs[0].Column != "table users" || fs[0].Kind != "email" {
		t.Fatalf("%+v", fs) // the zero date is not a phone number; view vip is confirmed
	}
}

func TestTextOfOtherTypesIsScanned(t *testing.T) {
	x := testIndex(t, nil)
	s := x.scanner("dump")
	tbl := &Table{Name: "contacts", PrimaryKey: []string{"id"}}
	cols := []*Column{{Name: "id", Category: CatNumeric}, {Name: "cc", Type: "text[]", Category: CatOther},
		{Name: "card", Type: "xml", Category: CatOther}, {Name: "blob", Type: "longblob", Category: CatBinary}, {Name: "at", Type: "datetime", Category: CatOther}}
	s.row(tbl, cols, newRowLabel(tbl, cols), 1, [][]byte{[]byte("1"), []byte(`{marko.markovic@firma.example,"+381 64 123 4567"}`),
		[]byte("<v><jmbg>0101990710008</jmbg></v>"), []byte("write to ana@firma.example"), []byte("0000-00-00 00:00:00")})
	var got []string
	for _, f := range s.findings() {
		got = append(got, f.Column+":"+f.Kind)
	}
	if strings.Join(got, ",") != "blob:email,card:jmbg,cc:email,cc:phone" {
		t.Fatalf("%v", got)
	}
}

// TestIndexInOnePass: the index made in one pass over the originals is the
// one made by reserving them all first and adding them after, also when a
// stand-in made early is an original that comes later.
func TestIndexInOnePass(t *testing.T) {
	det, err := pii.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	r := newTestReplacer(t, testKey)
	early, err := r.Number("1234567890")
	if err != nil {
		t.Fatal(err)
	}
	var values []PersonalValue
	for i := range 300 {
		values = append(values,
			PersonalValue{Table: "users", Column: "name", Kind: StrategyUniversal, Value: fmt.Sprintf("Petar Petrović %d", i), Category: CatText},
			PersonalValue{Table: "users", Column: "phone", Kind: StrategyUniversal, Value: fmt.Sprintf("+381 64 %03d %04d", i, i*7), Category: CatText},
			PersonalValue{Table: "users", Column: "bio", Kind: "email", Value: fmt.Sprintf("user%d@primer.example", i), Category: CatText},
			PersonalValue{Table: "users", Column: "token", Kind: StrategyToken, Value: fmt.Sprintf("%040x", i*104729), Category: CatText})
	}
	each := func(vs []PersonalValue) func(func(PersonalValue) error) error {
		return func(fn func(PersonalValue) error) error {
			for _, v := range vs {
				if err := fn(v); err != nil {
					return err
				}
			}
			return nil
		}
	}
	for _, c := range []struct {
		name   string
		values []PersonalValue
		again  bool // made again in two passes
	}{
		{"plain", values, false},
		// The stand-in of the first number is the second number, an original.
		{"stand-in is a later original", append([]PersonalValue{
			{Table: "a", Column: "n", Kind: StrategyUniversal, Value: "1234567890", Category: CatNumeric},
			{Table: "a", Column: "n", Kind: StrategyUniversal, Value: early, Category: CatNumeric},
		}, values...), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			one, err := newLeakIndex(testKey, det)
			if err != nil {
				t.Fatal(err)
			}
			passes := 0
			counted := func(fn func(PersonalValue) error) error { passes++; return each(c.values)(fn) }
			var also int
			if err := one.fill(counted, func(PersonalValue) error { also++; return nil }); err != nil {
				t.Fatal(err)
			}
			if want := map[bool]int{false: 1, true: 3}[c.again]; passes != want || also != len(c.values) {
				t.Fatalf("%d passes, also %d times; want %d and %d", passes, also, want, len(c.values))
			}
			two, err := newLeakIndex(testKey, det)
			if err != nil {
				t.Fatal(err)
			}
			two.seed = one.seed // the same hashes
			if err := each(c.values)(two.r.Reserve); err != nil {
				t.Fatal(err)
			}
			if err := each(c.values)(two.add); err != nil {
				t.Fatal(err)
			}
			one.freeze()
			two.freeze()
			if !slices.Equal(one.values.e, two.values.e) || !slices.Equal(one.stems.e, two.stems.e) || !slices.Equal(one.phones.e, two.phones.e) ||
				!slices.Equal(one.first, two.first) || !slices.Equal(one.standIns, two.standIns) || !slices.Equal(one.origins, two.origins) || !slices.Equal(one.lengths, two.lengths) {
				t.Fatal("the index of one pass differs from the index of two")
			}
			if len(one.standIns) == 0 || len(one.values.e) < len(values)/2 {
				t.Fatalf("%d stand-ins, %d values", len(one.standIns), len(one.values.e))
			}
		})
	}
}

// TestLeakSetKeepsTheFirst: compacting in steps keeps, of each hash, the
// entry added first, as one stable sort of all entries does.
func TestLeakSetKeepsTheFirst(t *testing.T) {
	var l leakSet
	var all []leakEntry
	for i := range 200_000 {
		e := leakEntry{h: uint64(i*7919) % 50_000, origin: uint32(i)}
		l.add(e)
		all = append(all, e)
	}
	l.compact()
	slices.SortStableFunc(all, func(a, b leakEntry) int { return cmp.Compare(a.h, b.h) })
	var want []leakEntry
	for _, e := range all {
		if len(want) == 0 || want[len(want)-1].h != e.h {
			want = append(want, e)
		}
	}
	if !slices.Equal(l.e, want) {
		t.Fatalf("%d entries, want %d", len(l.e), len(want))
	}
}
