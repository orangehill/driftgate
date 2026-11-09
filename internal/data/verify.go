package data

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/maphash"
	"html"
	"io"
	"math/bits"
	"net/url"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/orangehill/driftgate/internal/jsonx"
	"github.com/orangehill/driftgate/internal/pii"
)

// Checks of data verify.
const (
	CheckDetector = "detector" // the personal-data detector over the output
	CheckLeak     = "leak"     // an original personal value in the output
)

// The leak check looks for an original value by its words: runs of letters
// and digits, folded (see foldRune) and joined by one space. Short values
// appear in any output by chance, so a value counts only with at least
// minLeak letters and digits, or minLeakDigits for digits only (amounts,
// dates and counters share short digit runs with phone numbers). A value of
// a universal column with up to maxNameWords words (a name) is also looked
// for word by word, for words of at least minLeak letters: anonymize
// replaces a name in free text only in the same row, so a surname in
// another table stays. Longer values (addresses, titles) are not split:
// their single words are not identifying and would match ordinary text.
// Of a value with more than maxLeakWords words, the first maxLeakWords are
// looked for.
const (
	minLeak       = 6
	minLeakDigits = 8
	maxNameWords  = 4
	maxLeakWords  = 16
	// maxRows is the most rows a finding lists.
	maxRows = 5
)

// VerifyOptions configure Verify.
type VerifyOptions struct {
	Source Database  // the original, read in one snapshot: its personal values are looked for
	Target *Database // an output database to check; nil checks none
	Dump   io.Reader // an output dump to check; nil checks none
	Rules  *Rules
	Key    []byte        // the project key the output was made with
	PII    *pii.Detector // nil: the default detector
	// Index, filled by Anonymize (Options.Index) under the same key, holds
	// the original values to look for: the source is not read again, and
	// the originals are those of the snapshot the output was copied from.
	// The detector is the one Anonymize used.
	Index *VerifyIndex
	Log   io.Writer
}

// VerifyIndex is what data verify knows about the original personal values
// of a source, as keyed hashes; Anonymize fills it (Options.Index) in its
// own pass over them, for Verify on the output of the same run. The values
// come from the source, by the same rules as PersonalValues gives them, and
// not from what anonymize did with them: the leak check finds what the copy
// let through as it does after a read of its own.
type VerifyIndex struct {
	key    []byte
	x      *leakIndex
	schema *Schema
}

// VerifyReport says what Verify found, in counts and places only: it never
// holds a value.
type VerifyReport struct {
	Originals int64           `json:"originals"` // distinct original values the leak check looked for
	Values    int64           `json:"values"`    // output values scanned
	Seconds   float64         `json:"seconds"`
	Findings  []VerifyFinding `json:"findings"`
}

// Clean reports whether neither check found anything.
func (r *VerifyReport) Clean() bool { return len(r.Findings) == 0 }

// VerifyFinding is one kind of finding in one column of an output.
type VerifyFinding struct {
	Check  string `json:"check"`  // detector or leak
	Output string `json:"output"` // "database <name>" or "dump"
	Table  string `json:"table"`  // "(schema)" for a statement of a dump that holds no rows
	Column string `json:"column,omitempty"`
	// Kind is the detector's kind (email, phone, jmbg, card, iban), or for
	// the leak check the kind of the original value (its strategy, or the
	// detector's kind for a part of free text), with "/word" for one word of
	// a name.
	Kind   string   `json:"kind"`
	Origin string   `json:"origin,omitempty"` // leak: the table.column the original value comes from
	Values int64    `json:"values"`           // output values with it
	Rows   []string `json:"rows,omitempty"`   // up to five: the primary key when it is numeric, else #n in key order
}

// Verify checks an output of Anonymize (a database, a dump or both) against
// its source with two independent checks:
//
//   - the detector looks at every text and JSON value of the output (JSON
//     strings decoded, HTML entities resolved); an e-mail address at a
//     .invalid domain, a known test password hash and the stand-in of a
//     personal value (computed again under the key) are not findings;
//   - the leak check looks for every original personal value of the source
//     (PersonalValues) in every value of the output, as a whole value and
//     inside text, regardless of case, script (Serbian Cyrillic and Latin)
//     and diacritics.
//
// The original values are never kept: the leak check holds two keyed 64-bit
// hashes of each (a fast hash under a random per-run seed for the scan, and
// the start of Replacer.Digest to confirm a match), about 65 bytes per
// distinct value. A match found by the project's pii.allow patterns is not
// a finding. The source is read once, in one snapshot, unless opt.Index
// holds the originals already.
func Verify(ctx context.Context, opt VerifyOptions) (*VerifyReport, error) {
	start := time.Now()
	if opt.Target == nil && opt.Dump == nil {
		return nil, errors.New("nothing to verify: no target database and no dump")
	}
	if opt.Rules == nil {
		return nil, errors.New("no rules")
	}
	if opt.Log == nil {
		opt.Log = io.Discard
	}
	var x *leakIndex
	var schema *Schema
	if ix := opt.Index; ix != nil {
		switch {
		case ix.x == nil:
			return nil, errors.New("the index of originals is empty: Anonymize has not filled it")
		case !bytes.Equal(ix.key, opt.Key):
			return nil, errors.New("the index of originals was made under another key")
		case opt.PII != nil && opt.PII != ix.x.det:
			return nil, errors.New("the index of originals was made with another detector")
		}
		x, schema = ix.x, ix.schema
	} else {
		det := opt.PII
		if det == nil {
			var err error
			if det, err = pii.New(nil); err != nil {
				return nil, err
			}
		}
		var err error
		if x, err = newLeakIndex(opt.Key, det); err != nil {
			return nil, err
		}
		dl, err := dialectFor(opt.Source.Driver)
		if err != nil {
			return nil, err
		}
		schema, err = inSnapshot(ctx, opt.Source, opt.Rules, func(c *sql.Conn, s *Schema) error {
			return x.fill(func(fn func(PersonalValue) error) error {
				return eachPersonal(ctx, c, dl, det, s, opt.Rules, x.ahead, fn)
			}, nil)
		})
		if err != nil {
			return nil, fmt.Errorf("source: %w", err)
		}
	}
	x.rules = opt.Rules
	fmt.Fprintf(opt.Log, "%-40s %d original values to look for\n", "verify", x.count())
	rep := &VerifyReport{Originals: int64(x.count())}
	// The database and the dump are scanned at the same time; the findings
	// of the database come first.
	type scanned struct {
		values   int64
		findings []VerifyFinding
		err      error
	}
	var db, dump scanned
	var wg sync.WaitGroup
	log := &lockedWriter{w: opt.Log}
	scan := func(out *scanned, output string, run func(*scanner) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sc := x.scanner(output)
			defer sc.close()
			if out.err = run(sc); out.err == nil {
				out.values, out.findings = sc.values, sc.findings()
			}
		}()
	}
	if opt.Target != nil {
		scan(&db, "database "+opt.Target.Name, func(sc *scanner) error { return sc.database(ctx, *opt.Target, log) })
	}
	if opt.Dump != nil {
		scan(&dump, "dump", func(sc *scanner) error { return sc.dump(opt.Dump, schema, log) })
	}
	wg.Wait()
	switch {
	case db.err != nil:
		return nil, fmt.Errorf("target: %w", db.err)
	case dump.err != nil:
		return nil, fmt.Errorf("dump: %w", dump.err)
	}
	rep.Values = db.values + dump.values
	rep.Findings = append(db.findings, dump.findings...)
	rep.Seconds = time.Since(start).Seconds()
	return rep, nil
}

// lockedWriter writes the lines of goroutines one at a time.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// leakIndex holds what the checks know about the source, without values.
// Its sets are sorted arrays of 16-byte entries (a 64-bit hash under a
// per-run seed, 32 bits of the keyed digest to confirm a match, the
// origin), compacted while values are added and searched by binary search
// behind a bit filter: about a third of the memory of hash maps.
type leakIndex struct {
	seed  maphash.Seed
	key   []byte
	r     *Replacer
	det   *pii.Detector
	rules *Rules // confirmations of schema findings; nil: none

	values leakSet  // the folded words of an original, joined by spaces
	stems  leakSet  // a name word without its final vowel, or itself: inflected forms
	phones leakSet  // the digits of a phone number without its prefix (see phoneDigits)
	first  []uint64 // hashes of the first word of every original of more than one word, sorted when frozen
	filter []uint64 // bits: the first word of an original may be here

	firstSorted int
	lengths     []int // the word counts of the originals, ascending
	origins     []string
	originN     map[string]uint32
	// standIns are the stand-ins in which the detector finds something (a
	// replaced phone number still looks like one), by exact text.
	standIns []uint64
	frozen   bool
}

type leakEntry struct {
	h      uint64
	digest uint32 // the start of Replacer.Digest of the folded text
	origin uint32 // index into origins: table.column and kind
}

// leakSet is a set of entries, sorted and without repeats up to sorted;
// add compacts it when it has doubled, so repeats of a value (a city in
// every row) do not pile up.
type leakSet struct {
	e      []leakEntry
	sorted int
}

func (l *leakSet) add(e leakEntry) {
	l.e = append(l.e, e)
	if len(l.e) >= 2*max(l.sorted, 1<<14) {
		l.compact()
	}
}

// compact sorts by hash, keeping the first entry added of each hash: the
// entries added since the last compaction are sorted (stable) and merged
// into the sorted ones, which were added before them.
func (l *leakSet) compact() {
	head, tail := l.e[:l.sorted], l.e[l.sorted:]
	slices.SortStableFunc(tail, func(a, b leakEntry) int { return cmp.Compare(a.h, b.h) })
	out := make([]leakEntry, 0, len(l.e))
	for i, j := 0, 0; i < len(head) || j < len(tail); {
		var e leakEntry
		if j == len(tail) || i < len(head) && head[i].h <= tail[j].h {
			e, i = head[i], i+1
		} else {
			e, j = tail[j], j+1
		}
		if len(out) == 0 || e.h != out[len(out)-1].h {
			out = append(out, e)
		}
	}
	l.e = append(make([]leakEntry, 0, len(out)+len(out)/4), out...) // room to grow, without the repeats
	l.sorted = len(l.e)
}

func (l *leakSet) get(h uint64) (leakEntry, bool) {
	i := sort.Search(len(l.e), func(i int) bool { return l.e[i].h >= h })
	if i < len(l.e) && l.e[i].h == h {
		return l.e[i], true
	}
	return leakEntry{}, false
}

func newLeakIndex(key []byte, det *pii.Detector) (*leakIndex, error) {
	r, err := NewReplacer(key)
	if err != nil {
		return nil, err
	}
	return &leakIndex{seed: maphash.MakeSeed(), key: key, r: r, det: det, originN: map[string]uint32{}}, nil
}

// fill takes the originals of a source from each, which streams them to
// its function, in one pass: every value is reserved (as anonymize
// reserves them, so the stand-ins the detector accepts are the ones
// anonymize wrote) and added at once, and also gets it first (anonymize's
// own reservation). A stand-in is computed before all originals are
// reserved, without looking at them (Replacer.later); when one of them is
// a stand-in made so (Replacer.madeBeforeReserved, which is rare), the
// index is made again in two passes, all originals reserved before any is
// added.
func (x *leakIndex) fill(each func(fn func(PersonalValue) error) error, also func(PersonalValue) error) error {
	x.r.later = true
	err := each(func(v PersonalValue) error {
		if also != nil {
			if err := also(v); err != nil {
				return err
			}
		}
		if err := x.r.Reserve(v); err != nil {
			return err
		}
		return x.add(v)
	})
	x.r.later = false
	if err != nil || !x.r.madeBeforeReserved() {
		return err
	}
	fresh, err := newLeakIndex(x.key, x.det)
	if err != nil {
		return err
	}
	fresh.seed = x.seed // the entries made ahead stay valid
	*x = *fresh
	if err := each(x.r.Reserve); err != nil {
		return err
	}
	return each(x.add)
}

// freeze makes the index ready for scanning: no more add.
func (x *leakIndex) freeze() {
	if x.frozen {
		return
	}
	x.frozen = true
	x.values.compact()
	x.stems.compact()
	x.phones.compact()
	x.compactFirst()
	x.values.e, x.stems.e, x.phones.e = slices.Clone(x.values.e), slices.Clone(x.stems.e), slices.Clone(x.phones.e)
	slices.Sort(x.standIns)
	x.standIns = slices.Clone(slices.Compact(x.standIns))
	n := 64
	for n < 8*(len(x.first)+len(x.values.e)) {
		n *= 2
	}
	x.filter = make([]uint64, n/64)
	set := func(h uint64) { x.filter[(h&uint64(n-1))/64] |= 1 << (h % 64) }
	for _, h := range x.first {
		set(h)
	}
	for _, e := range x.values.e {
		set(e.h)
	}
}

// count is the number of distinct originals looked for.
func (x *leakIndex) count() int {
	x.freeze()
	return len(x.values.e) + len(x.stems.e) + len(x.phones.e)
}

func (x *leakIndex) compactFirst() {
	slices.Sort(x.first)
	x.first = slices.Compact(x.first)
	x.first = append(make([]uint64, 0, len(x.first)+len(x.first)/4+1), x.first...)
	x.firstSorted = len(x.first)
}

func (x *leakIndex) maybeFirst(h uint64) bool {
	n := uint64(len(x.filter) * 64)
	if x.filter[(h&(n-1))/64]&(1<<(h%64)) == 0 {
		return false
	}
	if _, found := slices.BinarySearch(x.first, h); found {
		return true
	}
	_, found := x.values.get(h)
	return found
}

// foldRune writes the folded form of a letter or digit: lower case, Serbian
// Cyrillic as Serbian Latin, Latin letters without diacritics (đ is dj), so
// "Јовановић", "JOVANOVIĆ" and "jovanovic" are one word. Combining marks
// are dropped by the caller, which also folds decomposed letters.
func foldRune(b []byte, c rune) []byte {
	if c < utf8.RuneSelf {
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		return append(b, byte(c))
	}
	c = unicode.ToLower(c)
	if s, ok := foldTable()[c]; ok {
		return append(b, s...)
	}
	return utf8.AppendRune(b, c)
}

var foldTable = sync.OnceValue(func() map[rune]string {
	m := map[rune]string{}
	add := func(from, to string) {
		for _, c := range from {
			m[c] = to
		}
	}
	for _, p := range [][2]string{
		{"àáâãäåāăą", "a"}, {"çćčĉċ", "c"}, {"ďđ", "dj"}, {"èéêëēĕėęě", "e"}, {"ìíîïĩīĭįı", "i"},
		{"ñńňņ", "n"}, {"òóôõöøōŏő", "o"}, {"ùúûüũūŭůűų", "u"}, {"ýÿ", "y"}, {"śšşŝ", "s"},
		{"źžż", "z"}, {"ľĺłļ", "l"}, {"ŕřŗ", "r"}, {"ťţ", "t"}, {"ğĝġģ", "g"},
		// Serbian Cyrillic, then the rest of the basic Cyrillic letters.
		{"а", "a"}, {"б", "b"}, {"в", "v"}, {"г", "g"}, {"д", "d"}, {"ђ", "dj"}, {"е", "e"}, {"ж", "z"},
		{"з", "z"}, {"и", "i"}, {"ј", "j"}, {"к", "k"}, {"л", "l"}, {"љ", "lj"}, {"м", "m"}, {"н", "n"},
		{"њ", "nj"}, {"о", "o"}, {"п", "p"}, {"р", "r"}, {"с", "s"}, {"т", "t"}, {"ћ", "c"}, {"у", "u"},
		{"ф", "f"}, {"х", "h"}, {"ц", "c"}, {"ч", "c"}, {"џ", "dz"}, {"ш", "s"},
		{"й", "j"}, {"щ", "sc"}, {"ы", "y"}, {"э", "e"}, {"ю", "ju"}, {"я", "ja"}, {"ё", "e"}, {"ъь", ""},
	} {
		add(p[0], p[1])
	}
	return m
})

// eachWord calls fn with every word of s, folded into buf (valid until the
// next call), and its byte offsets in s. A word is a run of letters, digits
// and combining marks; marks are dropped.
func eachWord(s string, buf []byte, fn func(w []byte, start, end int)) []byte {
	start := -1
	buf = buf[:0]
	for i, c := range s {
		mark := unicode.Is(unicode.Mn, c)
		if unicode.IsLetter(c) || unicode.IsDigit(c) || (mark && start >= 0) {
			if start < 0 {
				start = i
				buf = buf[:0]
			}
			if !mark {
				buf = foldRune(buf, c)
			}
			continue
		}
		if start >= 0 {
			fn(buf, start, i)
			start = -1
		}
	}
	if start >= 0 {
		fn(buf, start, len(s))
	}
	return buf
}

// foldedWords returns the folded words of s.
func foldedWords(s string) []string {
	var out []string
	eachWord(s, nil, func(w []byte, _, _ int) { out = append(out, string(w)) })
	return out
}

// mix chains the hash of one more word onto the hash of the words before;
// the hash of one word is its own.
func mix(h, w uint64, k int) uint64 {
	if k == 0 {
		return w
	}
	return (bits.RotateLeft64(h, 23) ^ w) * 0x9e3779b97f4a7c15
}

// longEnough applies minLeak and minLeakDigits to folded words.
func longEnough(words []string) bool {
	n, letter := 0, false
	for _, w := range words {
		for _, c := range w {
			n++
			if !letter && unicode.IsLetter(c) {
				letter = true
			}
		}
	}
	if letter {
		return n >= minLeak
	}
	return n >= minLeakDigits
}

// add takes one original personal value (PersonalValues calls it).
func (x *leakIndex) add(v PersonalValue) error {
	if x.frozen {
		return errors.New("the leak index is frozen")
	}
	ins, _ := v.ahead.([]leakInsert)
	if v.ahead == nil {
		ins = x.inserts(v)
	}
	for _, in := range ins {
		x.insert(in)
	}
	if len(ins) == 0 && (v.Category == CatBinary || v.Kind == StrategyPassword && isTestPasswordHash(v.Value)) {
		return nil
	}
	return x.addStandIn(v)
}

// ahead makes the entries of a value on a worker of eachPersonal.
func (x *leakIndex) ahead(v PersonalValue) any { return x.inserts(v) }

// leakInsert is one entry add puts into the index, made by inserts.
type leakInsert struct {
	set    *leakSet
	entry  leakEntry // without its origin
	origin string
	words  int    // the number of words of an entry of values; 0 for the other sets
	first  uint64 // the hash of the first of more than one word
}

// inserts makes the entries of one original value. It changes nothing in
// the index, so it may run on other goroutines than add (eachPersonal
// runs it ahead, on the workers that find the values).
func (x *leakIndex) inserts(v PersonalValue) []leakInsert {
	if v.Category == CatBinary {
		return nil // bytes: an image or a file, not text to look for
	}
	if v.Kind == StrategyPassword && isTestPasswordHash(v.Value) {
		return nil // already the stand-in
	}
	var out []leakInsert
	put := func(set *leakSet, words []string, origin string) {
		var h uint64
		for k, w := range words {
			h = mix(h, maphash.String(x.seed, w), k)
		}
		d := x.r.Digest(strings.Join(words, " "))
		in := leakInsert{set: set, entry: leakEntry{h: h, digest: binary.BigEndian.Uint32(d[:4])}, origin: origin}
		if set == &x.values {
			in.words = len(words)
			if len(words) > 1 { // a single word is its own entry in values
				in.first = maphash.String(x.seed, words[0])
			}
		}
		out = append(out, in)
	}
	stemOf := func(w, origin string) {
		if st, ok := stem(w); ok {
			if st == "" {
				st = w
			}
			put(&x.stems, []string{st}, origin)
		}
	}
	origin := v.Table + "." + v.Column + "\x00" + v.Kind
	words := foldedWords(v.Value)
	if len(words) > maxLeakWords {
		words = words[:maxLeakWords]
	}
	if longEnough(words) {
		put(&x.values, words, origin)
	}
	name := v.Kind == StrategyUniversal && v.Category != CatNumeric
	if name && len(words) > 1 && len(words) <= maxNameWords {
		for _, w := range words {
			if utf8.RuneCountInString(w) >= minLeak && strings.IndexFunc(w, unicode.IsLetter) >= 0 {
				put(&x.values, []string{w}, origin+"/word")
				stemOf(w, origin+"/word")
			}
		}
		if j := strings.Join(words, ""); utf8.RuneCountInString(j) >= minLeak {
			put(&x.values, []string{j}, origin+"/word") // written together: AnaPetrovic
		}
	}
	if name && len(words) == 1 {
		stemOf(words[0], origin)
	}
	if d := phoneDigits(v.Value); d != "" {
		put(&x.phones, []string{d}, origin)
	}
	if out == nil {
		out = []leakInsert{} // not nil: made, with nothing to insert
	}
	return out
}

func (x *leakIndex) originID(origin string) uint32 {
	id, ok := x.originN[origin]
	if !ok {
		id = uint32(len(x.origins))
		x.origins = append(x.origins, origin)
		x.originN[origin] = id
	}
	return id
}

func (x *leakIndex) insert(in leakInsert) {
	e := in.entry
	e.origin = x.originID(in.origin)
	in.set.add(e)
	if in.words == 0 {
		return
	}
	if in.words > 1 {
		x.first = append(x.first, in.first)
		if len(x.first) >= 2*max(x.firstSorted, 1<<14) {
			x.compactFirst()
		}
	}
	i := sort.SearchInts(x.lengths, in.words)
	if i == len(x.lengths) || x.lengths[i] != in.words {
		x.lengths = append(x.lengths, 0)
		copy(x.lengths[i+1:], x.lengths[i:])
		x.lengths[i] = in.words
	}
}

// phoneDigits is how a phone number is looked for whatever way it is
// written: its digits without the international or national prefix (00,
// +381, 0), so 064 123 4567, +381641234567 and 641234567 (a number column)
// are one. "" for a value that is no phone number: other characters than
// digits and separators, or fewer than minLeakDigits or more than 15 digits.
func phoneDigits(s string) string {
	var d []byte
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= '0' && c <= '9':
			d = append(d, c)
		case c == ' ' || c == '-' || c == '/' || c == '.' || c == '(' || c == ')' || c == '+':
		default:
			return ""
		}
	}
	return phoneCanon(d)
}

func phoneCanon(d []byte) string {
	if len(d) > 15 {
		return ""
	}
	if len(d) > 2 && d[0] == '0' && d[1] == '0' {
		d = d[2:]
	}
	if len(d) >= 3+minLeakDigits && string(d[:3]) == "381" {
		d = d[3:]
	}
	if len(d) > 0 && d[0] == '0' {
		d = d[1:]
	}
	if len(d) < minLeakDigits {
		return ""
	}
	return string(d)
}

// addStandIn computes the stand-in of a value that has enough digits to
// look like a phone, JMBG, card or IBAN after replacement, and keeps it
// when the detector finds something in it, or when it is a number (a JSON
// number the detector looks at by its key). An e-mail stand-in is at a
// .invalid domain and needs none.
func (x *leakIndex) addStandIn(v PersonalValue) error {
	digits := 0
	for i := 0; i < len(v.Value); i++ {
		if v.Value[i] >= '0' && v.Value[i] <= '9' {
			digits++
		}
	}
	if digits < minLeakDigits {
		return nil
	}
	var out string
	var err error
	numeric := v.Category == CatNumeric
	switch v.Kind {
	case StrategyEmail, StrategyNull, StrategyEmpty:
		return nil
	case StrategyPassword:
		out, _, err = x.r.Password(v.Value)
	case StrategyToken:
		if numeric {
			out, err = x.r.Number(v.Value)
		} else {
			out, err = x.r.Token(v.Value)
		}
	default:
		if numeric {
			out, err = x.r.Number(v.Value)
		} else {
			out, err = x.r.Universal(v.Value)
		}
	}
	if err != nil {
		return err
	}
	if numeric || len(x.det.Spans(out)) > 0 {
		x.standIns = append(x.standIns, maphash.String(x.seed, out))
	}
	return nil
}

func (x *leakIndex) standIn(s string) bool {
	if len(x.standIns) == 0 {
		return false
	}
	_, ok := slices.BinarySearch(x.standIns, maphash.String(x.seed, s))
	return ok
}

// isTestPasswordHash reports a hash of TestPassword as Replacer.Password
// writes it.
func isTestPasswordHash(s string) bool {
	switch {
	case len(s) == 60 && (strings.HasPrefix(s, "$2y$") || strings.HasPrefix(s, "$2a$") || strings.HasPrefix(s, "$2b$")):
		for _, h := range bcryptHashes {
			if s[4:] == h[4:] {
				return true
			}
		}
		return false
	case s == argon2idHash, s == argon2iHash:
		return true
	case strings.HasPrefix(s, "pbkdf2_sha256$"):
		parts := strings.Split(s, "$")
		return len(parts) == 4 && parts[2] == "driftgatesalt"
	}
	l := strings.ToLower(s)
	return l == md5Hex(TestPassword) || l == sha1Hex(TestPassword) || l == sha256Hex(TestPassword)
}

// scanner runs both checks over the values of one output, on a worker per
// CPU (the detector's patterns are most of the cost). Values are handed out
// in batches in output order; each finding keeps its first rows in that
// order.
type scanner struct {
	x       *leakIndex
	output  string
	values  int64
	batch   []cellJob
	jobs    chan []cellJob
	wg      sync.WaitGroup
	workers []*scanWorker
	closed  bool
}

// cellJob is one value of the output to scan.
type cellJob struct {
	seq                int64 // position in the output
	table, column, row string
	text               string
	detect             bool
	array              bool // a PostgreSQL array: its elements are values too
}

// scanWorker holds what one worker found.
type scanWorker struct {
	x      *leakIndex
	groups map[groupKey]*workerFinding
	cell   map[groupKey]bool // what the current value has
	words  []wordAt
	hashes []uint64
	buf    []byte
	fold   []byte
}

type workerFinding struct {
	values int64
	rows   []rowRef // the first maxRows, in output order
}

type rowRef struct {
	seq   int64
	label string
}

type groupKey struct{ check, table, column, kind, origin string }

type wordAt struct {
	h          uint64
	start, end int
	stems      [2]uint64 // hashes of the word without a case ending (see endings)
	nstems     int
	digits     bool
}

const scanBatch = 256

func (x *leakIndex) scanner(output string) *scanner {
	x.freeze()
	n := runtime.GOMAXPROCS(0)
	s := &scanner{x: x, output: output, jobs: make(chan []cellJob, 2*n)}
	for range n {
		w := &scanWorker{x: x, groups: map[groupKey]*workerFinding{}, cell: map[groupKey]bool{}}
		s.workers = append(s.workers, w)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			for b := range s.jobs {
				for _, j := range b {
					w.value(j)
				}
			}
		}()
	}
	return s
}

// value queues one value of the output: the detector runs only over text
// (not numbers, dates or bytes, where its patterns mean nothing).
func (s *scanner) value(table, column, row string, v string, detect bool) {
	s.queue(cellJob{table: table, column: column, row: row, text: v, detect: detect})
}

func (s *scanner) queue(j cellJob) {
	s.values++
	j.seq = s.values
	s.batch = append(s.batch, j)
	if len(s.batch) >= scanBatch {
		s.jobs <- s.batch
		s.batch = make([]cellJob, 0, scanBatch)
	}
}

// close waits for the workers; it is safe to call more than once.
func (s *scanner) close() {
	if s.closed {
		return
	}
	s.closed = true
	if len(s.batch) > 0 {
		s.jobs <- s.batch
		s.batch = nil
	}
	close(s.jobs)
	s.wg.Wait()
}

// findings waits for the workers and lists what they found, in a stable
// order.
func (s *scanner) findings() []VerifyFinding {
	s.close()
	merged := map[groupKey]*VerifyFinding{}
	refs := map[groupKey][]rowRef{}
	for _, w := range s.workers {
		for k, wf := range w.groups {
			f := merged[k]
			if f == nil {
				f = &VerifyFinding{Check: k.check, Output: s.output, Table: k.table, Column: k.column, Kind: k.kind, Origin: k.origin}
				merged[k] = f
			}
			f.Values += wf.values
			refs[k] = append(refs[k], wf.rows...)
		}
	}
	var out []VerifyFinding
	for k, f := range merged {
		r := refs[k]
		sort.Slice(r, func(i, j int) bool { return r[i].seq < r[j].seq })
		for _, ref := range r {
			if ref.label != "" && len(f.Rows) < maxRows && (len(f.Rows) == 0 || f.Rows[len(f.Rows)-1] != ref.label) {
				f.Rows = append(f.Rows, ref.label)
			}
		}
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		for _, p := range [][2]string{{a.Check, b.Check}, {a.Table, b.Table}, {a.Column, b.Column}, {a.Kind, b.Kind}, {a.Origin, b.Origin}} {
			if p[0] != p[1] {
				return p[0] < p[1]
			}
		}
		return false
	})
	return out
}

func (w *scanWorker) value(j cellJob) {
	texts := []string{j.text}
	if j.array {
		texts = append(texts, arrayElements(j.text)...)
	}
	for _, text := range texts {
		for _, v := range textViews(text) {
			w.leak(j.table, j.column, v.text)
			switch {
			case j.detect && v.number:
				w.detectNumber(j.table, j.column, v)
			case j.detect && v.detect:
				w.detect(j.table, j.column, v.text)
			}
		}
	}
	for k := range w.cell {
		f := w.groups[k]
		if f == nil {
			f = &workerFinding{}
			w.groups[k] = f
		}
		f.values++
		if len(f.rows) < maxRows {
			f.rows = append(f.rows, rowRef{j.seq, j.row})
		}
		delete(w.cell, k)
	}
}

// view is one form of a value both checks read.
type view struct {
	text   string
	detect bool   // the detector reads it: not the numbers of a JSON value
	number bool   // a JSON number, by its key (see personalNumber)
	path   string // of a JSON number
}

// maxDecode is how many layers of encoding a view is decoded through
// (&amp;#263; is HTML encoded twice).
const maxDecode = 3

// textViews are the forms of a value both checks read: the value itself;
// with HTML entities resolved, URL-decoded, JSON escapes resolved (\u0107
// anywhere in the text) — each repeatedly, for text encoded more than once;
// base64 runs of at least minBase64 characters that decode to text; and
// every string, key and number of a JSON value. The detector reads JSON
// numbers only by their key (personalNumber): the rest are amounts, IDs
// and times.
func textViews(v string) []view {
	var views []view
	seen := map[string]bool{}
	var add func(t string, depth int)
	add = func(t string, depth int) {
		if seen[t] {
			return
		}
		seen[t] = true
		views = append(views, view{text: t, detect: true})
		if depth >= maxDecode {
			return
		}
		if strings.IndexByte(t, '&') >= 0 {
			if u := html.UnescapeString(t); u != t {
				add(u, depth+1)
			}
		}
		if strings.IndexByte(t, '%') >= 0 {
			if u, err := url.QueryUnescape(t); err == nil && u != t {
				add(u, depth+1)
			}
		}
		if strings.IndexByte(t, '\\') >= 0 {
			if u := unescapeJSON(t); u != t {
				add(u, depth+1)
			}
		}
		for _, d := range base64Runs(t) {
			add(d, depth+1)
		}
		if j := jsonOf(t); j != nil {
			views[len(views)-1].detect = false // its strings are read one by one
			var walk func(path string, j *jsonx.Value)
			walk = func(path string, j *jsonx.Value) {
				switch j.Kind {
				case jsonx.String:
					add(j.Text, depth+1)
				case jsonx.Int, jsonx.Float:
					if personalNumber(path, j.Text) {
						views = append(views, view{text: j.Text, number: true, path: path})
					}
				case jsonx.Object:
					for _, m := range j.Members {
						add(m.Key, depth+1)
						walk(jsonx.KeyPath(path, m.Key), m.Value)
					}
				case jsonx.Array:
					for _, item := range j.Items {
						walk(path+"[*]", item)
					}
				}
			}
			walk("$", j)
		}
	}
	add(v, 0)
	return views
}

// jsonOf parses a text that is a JSON object or array.
func jsonOf(t string) *jsonx.Value {
	s := strings.TrimLeft(t, " \t\r\n")
	if s == "" || (s[0] != '{' && s[0] != '[') {
		return nil
	}
	j, err := jsonx.Parse([]byte(t))
	if err != nil {
		return nil
	}
	return j
}

// unescapeJSON resolves the escapes of a JSON string wherever they are in
// a text (a JSON document inside free text, or inside a JSON string).
func unescapeJSON(t string) string {
	var b strings.Builder
	for i := 0; i < len(t); i++ {
		if t[i] != '\\' || i+1 >= len(t) {
			b.WriteByte(t[i])
			continue
		}
		if r, size, esc := decodeChar(t, i); esc {
			b.WriteRune(r)
			i += size - 1
			continue
		}
		switch c := t[i+1]; c {
		case '"', '\\', '/':
			b.WriteByte(c)
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		default:
			b.WriteByte('\\')
			continue
		}
		i++
	}
	return b.String()
}

// minBase64 is the shortest run of base64 characters that is decoded.
const minBase64 = 16

// base64Runs decodes the runs of base64 (standard or URL alphabet) in a
// text that decode to printable UTF-8 text: a value stored encoded.
func base64Runs(t string) []string {
	var out []string
	isB64 := func(c byte) bool {
		return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '-' || c == '_'
	}
	for i := 0; i < len(t); {
		if !isB64(t[i]) {
			i++
			continue
		}
		j := i
		for j < len(t) && isB64(t[j]) {
			j++
		}
		run := t[i:j]
		for j < len(t) && t[j] == '=' {
			j++
		}
		i = j
		if len(run) < minBase64 {
			continue
		}
		if len(run)%4 == 1 {
			run = run[:len(run)-1] // no base64 has this length: the run ends in a word
		}
		enc := base64.RawStdEncoding
		if strings.ContainsAny(run, "-_") {
			enc = base64.RawURLEncoding
		}
		dec, err := enc.DecodeString(run)
		if err == nil && printable(dec) {
			out = append(out, string(dec))
		}
	}
	return out
}

// printable reports UTF-8 text without control characters but line breaks
// and tabs.
func printable(b []byte) bool {
	if !utf8.Valid(b) {
		return false
	}
	for _, c := range string(b) {
		if c < 0x20 && c != '\n' && c != '\r' && c != '\t' || c == 0x7f || c == utf8.RuneError {
			return false
		}
	}
	return true
}

// arrayElements reads the elements of a PostgreSQL array as text
// ({a,"b c",NULL} → a, b c), nested arrays flattened.
func arrayElements(v string) []string {
	if len(v) < 2 || v[0] != '{' {
		return nil
	}
	var out []string
	var b strings.Builder
	quoted, inQuote := false, false
	flush := func() {
		if e := b.String(); quoted || (e != "" && e != "NULL") {
			out = append(out, e)
		}
		b.Reset()
		quoted = false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case inQuote && c == '\\' && i+1 < len(v):
			i++
			b.WriteByte(v[i])
		case inQuote && c == '"':
			inQuote = false
		case inQuote:
			b.WriteByte(c)
		case c == '"':
			inQuote, quoted = true, true
		case c == '{':
		case c == ',' || c == '}':
			if b.Len() > 0 || quoted {
				flush()
			}
		default:
			b.WriteByte(c)
		}
	}
	return out
}

// leak looks for the originals in one text: at every word that starts an
// original, the runs of as many words as the originals have, longest first;
// every word inflected (a name word with a case ending); and every run of
// digit groups as a phone number. A word of a name inside a longer match
// (the whole name) is not counted again.
func (w *scanWorker) leak(table, column, t string) {
	x := w.x
	w.words = w.words[:0]
	w.buf = eachWord(t, w.buf, func(word []byte, start, end int) {
		wa := wordAt{h: maphash.Bytes(x.seed, word), start: start, end: end, digits: digitsOnly(string(word))}
		if len(x.stems.e) > 0 && !wa.digits {
			for _, e := range endings {
				if n := len(word) - len(e); n >= minStem && wa.nstems < len(wa.stems) && string(word[n:]) == e {
					wa.stems[wa.nstems] = maphash.Bytes(x.seed, word[:n])
					wa.nstems++
				}
			}
		}
		w.words = append(w.words, wa)
	})
	covered := 0 // words before this index are part of a match
	hit := func(i, n int, e leakEntry, part string) {
		from, kind, _ := strings.Cut(x.origins[e.origin], "\x00")
		if i < covered && strings.HasSuffix(kind, "/word") {
			return
		}
		if !w.confirm(part, e.digest) || x.det.Allowed(part) {
			return
		}
		covered = max(covered, i+n)
		w.cell[groupKey{CheckLeak, table, column, kind, from}] = true
	}
	for i := range w.words {
		wi := &w.words[i]
		if x.maybeFirst(wi.h) {
			w.hashes = w.hashes[:0]
			var h uint64
			for k := 0; k < x.lengths[len(x.lengths)-1] && i+k < len(w.words); k++ {
				h = mix(h, w.words[i+k].h, k)
				w.hashes = append(w.hashes, h)
			}
			for j := len(x.lengths) - 1; j >= 0; j-- {
				n := x.lengths[j]
				if n > len(w.hashes) {
					continue
				}
				if e, ok := x.values.get(w.hashes[n-1]); ok {
					hit(i, n, e, t[w.words[i].start:w.words[i+n-1].end])
				}
			}
		}
		for k := 0; k < wi.nstems; k++ {
			if e, ok := x.stems.get(wi.stems[k]); ok {
				hit(i, 1, e, w.stemOf(t[wi.start:wi.end], k))
			}
		}
		if wi.digits && len(x.phones.e) > 0 {
			w.phone(table, column, t, i, hit)
		}
	}
}

// stemOf is the k-th stem of a word as text to confirm.
func (w *scanWorker) stemOf(word string, k int) string {
	w.fold = w.fold[:0]
	w.buf = eachWord(word, w.buf, func(f []byte, _, _ int) { w.fold = append(w.fold, f...) })
	n := 0
	for _, e := range endings {
		if l := len(w.fold) - len(e); l >= minStem && string(w.fold[l:]) == e {
			if n == k {
				return string(w.fold[:l])
			}
			n++
		}
	}
	return ""
}

// phone looks for phone numbers in the digit groups that start at word i
// and are written with at most two separators between them.
func (w *scanWorker) phone(table, column, t string, i int, hit func(i, n int, e leakEntry, part string)) {
	if i > 0 && w.words[i-1].digits && gapOK(t[w.words[i-1].end:w.words[i].start]) {
		return // inside a run that started earlier
	}
	var d []byte
	for k := i; k < len(w.words) && w.words[k].digits; k++ {
		if k > i && !gapOK(t[w.words[k-1].end:w.words[k].start]) {
			break
		}
		d = append(d, t[w.words[k].start:w.words[k].end]...)
		if len(d) > 15 {
			break
		}
		if c := phoneCanon(slices.Clone(d)); c != "" {
			if e, ok := w.x.phones.get(maphash.String(w.x.seed, c)); ok {
				hit(i, k-i+1, e, c)
			}
		}
	}
}

// gapOK is what may stand between the digit groups of a phone number.
func gapOK(g string) bool {
	if len(g) == 0 || len(g) > 2 {
		return false
	}
	for i := 0; i < len(g); i++ {
		if !strings.ContainsRune(" -/.()", rune(g[i])) {
			return false
		}
	}
	return true
}

// confirm checks a match of the fast hash against the keyed digest.
func (w *scanWorker) confirm(part string, digest uint32) bool {
	w.fold = w.fold[:0]
	w.buf = eachWord(part, w.buf, func(word []byte, _, _ int) {
		if len(w.fold) > 0 {
			w.fold = append(w.fold, ' ')
		}
		w.fold = append(w.fold, word...)
	})
	d := w.x.r.Digest(string(w.fold))
	return binary.BigEndian.Uint32(d[:4]) == digest
}

// detect runs the detector over one text. A finding in a schema object
// that classify.yaml confirms under schema: is not one.
func (w *scanWorker) detect(table, column, t string) {
	if strings.IndexByte(t, '@') < 0 && !hasDigits(t, 2) {
		return // every kind needs an @ or digits
	}
	spans := w.x.det.Spans(t)
	if len(spans) == 0 || isTestPasswordHash(t) || w.x.standIn(t) {
		return
	}
	for _, sp := range spans {
		v := t[sp.Start:sp.End]
		if sp.Kind == "email" && strings.HasSuffix(strings.ToLower(v), ".invalid") {
			continue // a stand-in address (RFC 2606): no mailbox can have it
		}
		if w.x.standIn(v) {
			continue
		}
		if table == schemaTable && w.x.rules != nil && w.x.rules.confirmed(column, sp.Kind) {
			continue
		}
		w.cell[groupKey{check: CheckDetector, table: table, column: column, kind: sp.Kind}] = true
	}
}

// detectNumber reports a JSON number whose key names a personal identifier
// unless it is a stand-in or allowed.
func (w *scanWorker) detectNumber(table, column string, v view) {
	if w.x.standIn(v.text) || w.x.det.Allowed(v.text) {
		return
	}
	kind := "number"
	if pii.Kind(lastKeyKind(v.path)) {
		kind = lastKeyKind(v.path)
	}
	w.cell[groupKey{check: CheckDetector, table: table, column: column, kind: kind}] = true
}

// lastKeyKind is the detector's kind a JSON key names: phone for a phone
// number, jmbg, card or iban by name, "" otherwise.
func lastKeyKind(path string) string {
	key := strings.ToLower(path[strings.LastIndexAny(path, ".[")+1:])
	switch {
	case hint(key, phoneWords):
		return "phone"
	case strings.Contains(key, "jmbg"):
		return "jmbg"
	case strings.Contains(key, "card"):
		return "card"
	case strings.Contains(key, "iban"):
		return "iban"
	}
	return ""
}

func hasDigits(s string, n int) bool {
	for i := 0; i < len(s) && n > 0; i++ {
		if s[i] >= '0' && s[i] <= '9' {
			n--
		}
	}
	return n == 0
}

// rowLabel names a row in a report: its primary key when every key column
// is a number (it cannot hold a personal value as text), otherwise its
// position in key order.
type rowLabel struct {
	keys []int // indexes of the key columns among the read columns
	cols []string
}

func newRowLabel(t *Table, cols []*Column) rowLabel {
	var l rowLabel
	for _, k := range t.PrimaryKey {
		found := false
		for i, c := range cols {
			if c.Name == k && c.Category == CatNumeric {
				l.keys = append(l.keys, i)
				l.cols = append(l.cols, k)
				found = true
			}
		}
		if !found {
			return rowLabel{}
		}
	}
	return l
}

func (l rowLabel) label(n int64, vals [][]byte) string {
	if len(l.keys) == 0 {
		return fmt.Sprintf("#%d", n)
	}
	parts := make([]string, len(l.keys))
	for i, k := range l.keys {
		parts[i] = l.cols[i] + "=" + string(vals[k])
	}
	return strings.Join(parts, ",")
}

// row scans the values of one row.
func (s *scanner) row(t *Table, cols []*Column, label rowLabel, n int64, vals [][]byte) {
	var name string
	for i, c := range cols {
		v := vals[i]
		if v == nil {
			continue
		}
		if name == "" {
			name = label.label(n, vals)
		}
		text := string(v)
		detect := c.Category == CatText || c.Category == CatJSON || textual(c)
		if c.Category == CatBinary {
			text = binaryText(text)
			detect = printable([]byte(text)) // text kept as bytes
		}
		s.queue(cellJob{table: t.Name, column: c.Name, row: name, text: text, detect: detect, array: isArray(c)})
	}
}

// binaryText is the bytes of a PostgreSQL bytea (read as \x… hex) or the
// raw bytes of a MySQL binary column.
func binaryText(v string) string {
	if strings.HasPrefix(v, `\x`) {
		var b strings.Builder
		for i := 2; i+1 < len(v); i += 2 {
			hi, lo := unhex(v[i]), unhex(v[i+1])
			if hi < 0 || lo < 0 {
				return v
			}
			b.WriteByte(byte(hi<<4 | lo))
		}
		return b.String()
	}
	return v
}

func unhex(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// database scans every row of every table of an output database, in one
// read-only snapshot.
func (s *scanner) database(ctx context.Context, d Database, log io.Writer) error {
	dl, err := dialectFor(d.Driver)
	if err != nil {
		return err
	}
	db, err := open(d, d.Name)
	if err != nil {
		return err
	}
	defer db.Close()
	c, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := dl.session(ctx, c); err != nil {
		return err
	}
	if err := dl.snapshot(ctx, c); err != nil {
		return err
	}
	defer c.ExecContext(context.Background(), "ROLLBACK")
	schema, err := dl.schema(ctx, c, d.Name)
	if err != nil {
		return err
	}
	for _, t := range schema.Tables {
		n, err := s.table(ctx, c, dl, t)
		if err != nil {
			return fmt.Errorf("%s: %w", t.Name, err)
		}
		fmt.Fprintf(log, "%-40s verified %d rows\n", t.Name, n)
	}
	for _, st := range schema.texts() {
		s.value(schemaTable, st.object, "", st.sql, true)
	}
	return nil
}

// schemaTable is the table of a finding in a schema object; its column is
// the object ("view vip").
const schemaTable = "(schema)"

func (s *scanner) table(ctx context.Context, c *sql.Conn, dl dialect, t *Table) (int64, error) {
	rows, err := c.QueryContext(ctx, dl.selectRows(t, t.Columns, ""))
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	raw := make([]sql.RawBytes, len(t.Columns))
	ptrs := make([]any, len(raw))
	vals := make([][]byte, len(raw))
	for i := range raw {
		ptrs[i] = &raw[i]
	}
	label := newRowLabel(t, t.Columns)
	var n int64
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return 0, err
		}
		n++
		for i, r := range raw {
			vals[i] = r
		}
		s.row(t, t.Columns, label, n, vals)
	}
	return n, rows.Err()
}

// dumpTable is a table of the source as its INSERT statements in a dump
// name it.
type dumpTable struct {
	t     *Table
	cols  []*Column
	label rowLabel
	n     int64
}

// dump scans a dump of Anonymize: the rows of every INSERT (matched to the
// source schema by the statement's head), and every other statement
// (tables with their comments and defaults, views, triggers, functions,
// constraints) as a value of the schema object it makes.
func (s *scanner) dump(r io.Reader, schema *Schema, log io.Writer) error {
	dl, err := dialectFor(schema.Driver)
	if err != nil {
		return err
	}
	heads := map[string]*dumpTable{}
	for _, t := range schema.Tables {
		var cols []*Column
		for _, c := range t.Columns {
			if !c.Generated {
				cols = append(cols, c)
			}
		}
		heads[dl.insert(t, cols)] = &dumpTable{t: t, cols: cols, label: newRowLabel(t, cols)}
	}
	st := newStatements(r, schema.Driver)
	if err := st.header(schema.Driver); err != nil {
		return err
	}
	var last *dumpTable
	for {
		q, err := st.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if !strings.HasPrefix(q, "INSERT INTO ") {
			s.value(schemaTable, objectName(q), "", q, true)
			continue
		}
		i := strings.Index(q, " VALUES\n")
		if i < 0 {
			return fmt.Errorf("an INSERT without rows: %.80s", q)
		}
		dt := heads[q[:i+len(" VALUES")]]
		if dt == nil {
			return fmt.Errorf("an INSERT into a table the source does not have with these columns: %.120s", q[:i])
		}
		if last != nil && last != dt {
			fmt.Fprintf(log, "%-40s verified %d rows\n", last.t.Name, last.n)
		}
		last = dt
		if err := parseRows(q[i+len(" VALUES\n"):], len(dt.cols), schema.Driver, func(vals [][]byte) {
			dt.n++
			s.row(dt.t, dt.cols, dt.label, dt.n, vals)
		}); err != nil {
			return fmt.Errorf("%s: %w", dt.t.Name, err)
		}
	}
	if last != nil {
		fmt.Fprintf(log, "%-40s verified %d rows\n", last.t.Name, last.n)
	}
	return nil
}

// statements reads a dump statement by statement, the way mysql and psql
// split it: quotes, PostgreSQL dollar quotes, comments and the DELIMITER
// lines of MySQL. A statement is at most one INSERT batch (about 1 MiB), so
// memory stays bounded.
type statements struct {
	r      *bufio.Reader
	driver string
	delim  string
	b      []byte
}

func newStatements(r io.Reader, driver string) *statements {
	if driver == "mariadb" {
		driver = MySQL
	}
	return &statements{r: bufio.NewReaderSize(r, 1<<20), driver: driver, delim: ";"}
}

// header reads the first line of the dump, which must be the header
// Anonymize writes: the tokenizer follows how mysql and psql split the
// statements this module writes, and a dump of another tool (mysqldump,
// pg_dump's COPY) would not have its rows read as rows.
func (st *statements) header(driver string) error {
	l, err := st.r.ReadString('\n')
	if err != nil && err != io.EOF {
		return err
	}
	for format := 1; format <= DumpFormat; format++ {
		for _, d := range []string{driver, st.driver, "mariadb"} {
			if (d != "mariadb" || st.driver == MySQL) && l == dumpHeader(format, d) {
				return nil
			}
		}
	}
	return fmt.Errorf("not a dump of driftgate data anonymize or subset for %s (the first line is %.80q)", driver, strings.TrimSuffix(l, "\n"))
}

func isIdentByte(c byte) bool {
	return c == '_' || c >= 0x80 || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

var errUnterminated = errors.New("the dump ends inside a quoted string or a comment")

// next returns the next statement without its delimiter, or io.EOF.
func (st *statements) next() (string, error) {
	st.b = st.b[:0]
	for {
		// Between statements: blank lines, comment lines, DELIMITER.
		p, err := st.r.Peek(1)
		if err != nil {
			return "", err
		}
		if c := p[0]; c == ' ' || c == '\n' || c == '\r' || c == '\t' {
			st.r.ReadByte()
			continue
		}
		if p, _ := st.r.Peek(2); string(p) == "--" || (st.driver == MySQL && len(p) > 0 && p[0] == '#') {
			if _, err := st.r.ReadString('\n'); err != nil {
				return "", err
			}
			continue
		}
		if p, _ := st.r.Peek(10); st.driver == MySQL && string(p) == "DELIMITER " {
			l, err := st.r.ReadString('\n')
			if err != nil && err != io.EOF {
				return "", err
			}
			st.delim = strings.TrimSpace(l[len("DELIMITER "):])
			continue
		}
		break
	}
	var quote byte // ' " ` or $ for a dollar quote
	var tag string // the dollar-quote tag, with its dollars
	opened := 0    // where the text of the dollar quote starts
	escapes := false
	read := func() (byte, error) {
		c, err := st.r.ReadByte()
		if err == io.EOF {
			err = errUnterminated
		}
		if err == nil {
			st.b = append(st.b, c)
		}
		return c, err
	}
	for {
		c, err := st.r.ReadByte()
		if err == io.EOF {
			if quote != 0 {
				return "", errUnterminated
			}
			if s := strings.TrimSpace(string(st.b)); s != "" {
				return s, nil
			}
			return "", io.EOF
		}
		if err != nil {
			return "", err
		}
		st.b = append(st.b, c)
		n := len(st.b)
		switch {
		case quote == '$':
			if c == '$' && n-len(tag) >= opened && string(st.b[n-len(tag):]) == tag {
				quote = 0
			}
			continue
		case quote != 0:
			if c == '\\' && escapes {
				if _, err := read(); err != nil {
					return "", err
				}
			} else if c == quote {
				if p, _ := st.r.Peek(1); len(p) == 1 && p[0] == quote {
					read()
				} else {
					quote = 0
				}
			}
			continue
		case c == '\'' || c == '"' || (c == '`' && st.driver == MySQL):
			quote = c
			// MySQL escapes with a backslash in strings; PostgreSQL only in E'…'.
			escapes = c != '`' && (st.driver == MySQL ||
				(c == '\'' && n >= 2 && (st.b[n-2] == 'E' || st.b[n-2] == 'e') && (n < 3 || !isIdentByte(st.b[n-3]))))
			continue
		case c == '$' && st.driver == Postgres && (n < 2 || !isIdentByte(st.b[n-2])):
			// $tag$ or $$; not $1.
			p, _ := st.r.Peek(64)
			if end := strings.IndexByte(string(p), '$'); end >= 0 {
				t := p[:end]
				ok := len(t) == 0 || !(t[0] >= '0' && t[0] <= '9')
				for _, b := range t {
					ok = ok && isIdentByte(b)
				}
				if ok {
					st.b = append(st.b, p[:end+1]...)
					st.r.Discard(end + 1)
					quote, tag, opened = '$', "$"+string(t)+"$", len(st.b)
					continue
				}
			}
		case c == '#' && st.driver == MySQL,
			c == '-' && n >= 2 && st.b[n-2] == '-':
			// A comment to the end of the line (MySQL: # or "-- " with a space).
			if p, _ := st.r.Peek(1); c == '#' || st.driver != MySQL || len(p) == 0 || p[0] == ' ' || p[0] == '\t' || p[0] == '\n' || p[0] == '\r' {
				l, err := st.r.ReadString('\n')
				st.b = append(st.b, l...)
				if err != nil && err != io.EOF {
					return "", err
				}
				continue
			}
		case c == '*' && n >= 2 && st.b[n-2] == '/':
			for {
				c, err := read()
				if err != nil {
					return "", err
				}
				if c == '/' && st.b[len(st.b)-2] == '*' {
					break
				}
			}
			continue
		}
		if len(st.b) >= len(st.delim) && string(st.b[len(st.b)-len(st.delim):]) == st.delim {
			s := strings.TrimSpace(string(st.b[:len(st.b)-len(st.delim)]))
			st.b = st.b[:0]
			if s == "" {
				continue
			}
			return s, nil
		}
	}
}

// parseRows reads the rows of an INSERT as Anonymize writes them:
// (v, v, …),\n(…) with NULL, numbers, '…' strings (MySQL with backslash
// escapes, PostgreSQL with doubled quotes) and MySQL X'…' bytes.
func parseRows(s string, ncols int, driver string, fn func(vals [][]byte)) error {
	mysql := driver != Postgres
	vals := make([][]byte, 0, ncols)
	// Never nil, so that an empty string is never read as NULL (a value
	// is a slice of b).
	b := make([]byte, 0, 256)
	i := 0
	skip := func() {
		for i < len(s) && (s[i] == ' ' || s[i] == '\n' || s[i] == '\r' || s[i] == '\t') {
			i++
		}
	}
	for {
		skip()
		if i >= len(s) || s[i] != '(' {
			return fmt.Errorf("a row does not start with ( at byte %d", i)
		}
		i++
		vals = vals[:0]
		b = b[:0]
		for {
			skip()
			if i >= len(s) {
				return errors.New("a row is not closed")
			}
			start := len(b)
			switch {
			case strings.HasPrefix(s[i:], "NULL"):
				i += 4
				vals = append(vals, nil)
			case s[i] == '\'':
				i++
				for {
					if i >= len(s) {
						return errors.New("a string is not closed")
					}
					c := s[i]
					if c == '\\' && mysql && i+1 < len(s) {
						i += 2
						switch e := s[i-1]; e {
						case '0':
							b = append(b, 0)
						case 'n':
							b = append(b, '\n')
						case 'r':
							b = append(b, '\r')
						case 'Z':
							b = append(b, 0x1a)
						default:
							b = append(b, e)
						}
						continue
					}
					if c == '\'' {
						if i+1 < len(s) && s[i+1] == '\'' {
							b = append(b, '\'')
							i += 2
							continue
						}
						i++
						break
					}
					b = append(b, c)
					i++
				}
				vals = append(vals, b[start:len(b):len(b)])
			case mysql && strings.HasPrefix(s[i:], "X'"):
				i += 2
				end := strings.IndexByte(s[i:], '\'')
				if end < 0 || end%2 != 0 {
					return errors.New("a hex literal is not closed")
				}
				for j := i; j < i+end; j += 2 {
					hi, lo := unhex(s[j]), unhex(s[j+1])
					if hi < 0 || lo < 0 {
						return errors.New("a hex literal has a non-hex digit")
					}
					b = append(b, byte(hi<<4|lo))
				}
				i += end + 1
				vals = append(vals, b[start:len(b):len(b)])
			default:
				j := i
				for j < len(s) && s[j] != ',' && s[j] != ')' {
					j++
				}
				b = append(b, strings.TrimSpace(s[i:j])...)
				i = j
				vals = append(vals, b[start:len(b):len(b)])
			}
			skip()
			if i >= len(s) {
				return errors.New("a row is not closed")
			}
			if s[i] == ',' {
				i++
				continue
			}
			if s[i] != ')' {
				return fmt.Errorf("unexpected %q in a row", s[i])
			}
			i++
			break
		}
		if len(vals) != ncols {
			return fmt.Errorf("a row has %d values, the table %d columns", len(vals), ncols)
		}
		fn(vals)
		skip()
		if i >= len(s) {
			return nil
		}
		if s[i] != ',' {
			return fmt.Errorf("unexpected %q after a row", s[i])
		}
		i++
	}
}
