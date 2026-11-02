package data

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/pbkdf2"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"hash/fnv"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	"github.com/orangehill/driftgate/internal/lazy"
	"github.com/orangehill/driftgate/internal/pii"
)

// keyLabel separates the key that pseudonymises data from the other uses of
// the project key (encryption, naming private bodies).
const keyLabel = "driftgate data v1"

// TestPassword is the password every replaced password hash verifies with.
const TestPassword = "driftgate"

// maxTries bounds the search for a replacement no other value already has.
const maxTries = 1000

// Replacer turns values into stand-ins deterministically: the same input
// under the same key always gives the same output, in every table and run,
// so joins, uniqueness and a CI baseline survive. Different inputs keep
// different outputs: a collision is resolved by the next candidate, and the
// first value (in processing order) to reach an output keeps it.
type Replacer struct {
	key []byte

	mu    sync.Mutex
	reg   map[string]map[uint64]uint64 // namespace → output hash → input hash
	colls map[string]*collation        // by name, for ForColumn
	// taken holds the original values of the source by namespace (hashes of
	// their folded text), so no stand-in is another original (Reserve).
	taken map[string]*hashSet
	// later makes unique leave taken out while originals are still being
	// reserved: a stand-in that is an original is then found after the
	// last one (madeBeforeReserved), and taken is not sorted again for every
	// stand-in.
	later bool

	macs    sync.Pool // HMACs under key, reset before use
	domains sync.Map  // domain → its stand-in (see domain), up to maxDomains
	ndomain atomic.Int32
}

// maxDomains bounds the stand-ins of domains that are kept: a run sees few
// domains, and every further one is computed each time.
const maxDomains = 1 << 14

// NewReplacer derives the replacement key from the project key.
func NewReplacer(projectKey []byte) (*Replacer, error) {
	if len(projectKey) < 16 {
		return nil, errors.New("data: the key must have at least 16 bytes")
	}
	mac := hmac.New(sha256.New, projectKey)
	mac.Write([]byte(keyLabel))
	return &Replacer{key: mac.Sum(nil), reg: map[string]map[uint64]uint64{}, colls: map[string]*collation{}, taken: map[string]*hashSet{}}, nil
}

// Namespaces keep the strategies apart: the same text as a name and as an
// e-mail address gets unrelated stand-ins, and their uniqueness is tracked
// separately.
const (
	nsUniversal = "universal"
	nsEmail     = "email"
	nsDomain    = "domain"
	nsToken     = "token"
)

// stream is the keystream of one replacement: HMAC blocks under a key that
// depends on the namespace, the input and the attempt.
type stream struct {
	seed  []byte
	mac   hash.Hash // keyed with seed, made for the first block
	block []byte
	n     uint32
	pos   int
}

func (r *Replacer) stream(ns, input string, attempt int) *stream {
	mac := r.mac()
	mac.Write([]byte(ns))
	mac.Write([]byte{0})
	mac.Write([]byte(input))
	mac.Write([]byte{0})
	mac.Write(binary.BigEndian.AppendUint32(nil, uint32(attempt)))
	st := &stream{seed: mac.Sum(nil)}
	r.macs.Put(mac)
	return st
}

// next returns a number in [0, n).
func (s *stream) next(n int) int {
	if s.pos+2 > len(s.block) {
		if s.mac == nil {
			s.mac = hmac.New(sha256.New, s.seed)
		}
		s.mac.Reset()
		s.mac.Write(binary.BigEndian.AppendUint32(nil, s.n))
		s.block, s.pos = s.mac.Sum(s.block[:0]), 0
		s.n++
	}
	v := int(binary.BigEndian.Uint16(s.block[s.pos:]))
	s.pos += 2
	return v % n
}

// Alphabets. Letters are replaced within their own set, so a Latin letter
// stays Latin (and ASCII stays ASCII, which keeps byte lengths and narrow
// character sets), Serbian diacritics stay diacritics and Cyrillic stays
// Cyrillic. Case is kept by the caller.
var (
	asciiLower   = []rune("abcdefghijklmnopqrstuvwxyz")
	serbianLatin = []rune("čćđšž")
	serbianCyr   = []rune("абвгдђежзијклљмнњопрстћуфхцчџш")
	basicCyr     = []rune("абвгдежзийклмнопрстуфхцчшщъыьэюя")
	latin1Lower  = []rune("àáâãäåæçèéêëìíîïðñòóôõöøùúûüýþ")
	greekLower   = []rune("αβγδεζηθικλμνξοπρστυφχψω")
)

var sets = sync.OnceValue(func() map[rune][]rune {
	m := map[rune][]rune{}
	// Later sets win for letters in two of them (Serbian Cyrillic ж is also basic).
	for _, set := range [][]rune{asciiLower, latin1Lower, greekLower, basicCyr, serbianCyr, serbianLatin} {
		for _, c := range set {
			m[c] = set
		}
	}
	return m
})

var (
	scriptMu   sync.Mutex
	scriptSets = map[string][]rune{}
)

// letterSet is the set a letter is replaced from: its own alphabet, or for a
// letter of another script, the letters of that script with the same case
// and the same UTF-8 length.
func letterSet(lower rune, upper bool) []rune {
	if set, ok := sets()[lower]; ok {
		return set
	}
	switch {
	case unicode.Is(unicode.Latin, lower):
		return asciiLower
	case unicode.Is(unicode.Cyrillic, lower):
		return basicCyr
	}
	for name, table := range unicode.Scripts {
		if !unicode.Is(table, lower) {
			continue
		}
		size := utf8.RuneLen(lower)
		key := fmt.Sprintf("%s/%d/%t", name, size, unicode.IsLower(lower))
		scriptMu.Lock()
		defer scriptMu.Unlock()
		if set, ok := scriptSets[key]; ok {
			return set
		}
		var set []rune
		for _, rg := range table.R16 {
			for c := rune(rg.Lo); c <= rune(rg.Hi); c += rune(rg.Stride) {
				if unicode.IsLetter(c) && utf8.RuneLen(c) == size && unicode.IsLower(c) == unicode.IsLower(lower) && !unicode.IsUpper(c) {
					set = append(set, c)
				}
			}
		}
		for _, rg := range table.R32 {
			for c := rune(rg.Lo); c <= rune(rg.Hi); c += rune(rg.Stride) {
				if unicode.IsLetter(c) && utf8.RuneLen(c) == size && unicode.IsLower(c) == unicode.IsLower(lower) && !unicode.IsUpper(c) {
					set = append(set, c)
				}
			}
		}
		if len(set) == 0 {
			set = []rune{lower}
		}
		scriptSets[key] = set
		return set
	}
	return []rune{lower}
}

// digitZero is the zero of the run of decimal digits r belongs to.
func digitZero(r rune) rune {
	if r >= '0' && r <= '9' {
		return '0'
	}
	start := r
	for unicode.IsDigit(start - 1) {
		start--
	}
	return start + (r-start)/10*10
}

// shape replaces every letter by a letter of the same set and case and every
// digit by a digit; everything else (punctuation, spaces, marks) and the
// length stay. The keystream comes from seed, so callers choose what makes
// two values equal (the case-folded text for names, the exact text for
// tokens).
func (r *Replacer) shape(ns, s, seed string, attempt int) string {
	st := r.stream(ns, seed, attempt)
	var b strings.Builder
	b.Grow(len(s))
	for _, c := range s {
		switch {
		case unicode.IsLetter(c):
			upper := unicode.IsUpper(c)
			lower := unicode.ToLower(c)
			set := letterSet(lower, upper)
			out := set[st.next(len(set))]
			if upper {
				if u := unicode.ToUpper(out); u != out {
					out = u
				}
			}
			b.WriteRune(out)
		case unicode.IsDigit(c):
			b.WriteRune(digitZero(c) + rune(st.next(10)))
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

func hash64(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}

// hasContent reports whether s has a letter or digit, i.e. whether a stand-in
// can differ from it at all.
func hasContent(s string) bool {
	for _, c := range s {
		if unicode.IsLetter(c) || unicode.IsDigit(c) {
			return true
		}
	}
	return false
}

// unique finds the first candidate that is not the input itself and that no
// other input already holds. Equality goes through fold, so values that
// differ only in case (equal under a case-insensitive collation) get stand-ins
// that differ only in case as well.
func (r *Replacer) unique(ns, input string, fold func(string) string, candidate func(attempt int) string, valid func(string) bool) (string, error) {
	in := fold(input)
	inID := hash64(in)
	r.mu.Lock()
	defer r.mu.Unlock()
	reg := r.reg[ns]
	if reg == nil {
		reg = map[uint64]uint64{}
		r.reg[ns] = reg
	}
	content := hasContent(input)
	for attempt := 0; attempt < maxTries; attempt++ {
		out := candidate(attempt)
		if valid != nil && !valid(out) {
			continue
		}
		f := fold(out)
		if content && f == in {
			continue // a stand-in must never be the original
		}
		id := hash64(f)
		if owner, ok := reg[id]; ok && owner != inID {
			continue
		}
		if id != inID && !r.later && r.taken[ns].has(id) {
			continue // another original value: a stand-in must not be one
		}
		reg[id] = inID
		return out, nil
	}
	return "", fmt.Errorf("no distinct stand-in after %d tries: too many different values of the same shape (%d characters)", maxTries, utf8.RuneCountInString(input))
}

func identity(s string) string { return s }

// Universal is the strategy without dictionaries or languages: letters
// become letters of the same script and case, digits digits; punctuation,
// spacing and length stay. A value that is a plain number gets what Number
// gives it, so the same digits in a text column and a numeric column (a
// polymorphic key) get the same stand-in.
func (r *Replacer) Universal(s string) (string, error) {
	if s == "" {
		return s, nil
	}
	if plainNumber().MatchString(s) {
		return r.number(s)
	}
	seed := strings.ToLower(s)
	return r.unique(nsUniversal, s, strings.ToLower, func(attempt int) string {
		return r.shape(nsUniversal, s, seed, attempt)
	}, nil)
}

// Number replaces the digits of a number and keeps the rest (sign, point,
// exponent); a stand-in never gets a new leading zero, which a numeric column
// would drop (two stand-ins could then become equal). NaN and infinities stay:
// they hold nothing personal, and a stand-in for them does not exist.
func (r *Replacer) Number(s string) (string, error) {
	if s == "" || !hasDigits(s, 1) {
		return s, nil
	}
	return r.number(s)
}

func (r *Replacer) number(s string) (string, error) {
	orig := []rune(s)
	seed := strings.ToLower(s)
	return r.unique(nsUniversal, s, strings.ToLower, func(attempt int) string {
		out := []rune(r.shape(nsUniversal, s, seed, attempt))
		for i, c := range orig {
			if !unicode.IsDigit(c) {
				out[i] = c
			}
		}
		return string(out)
	}, func(out string) bool {
		digits := strings.TrimLeft(out, "-+")
		return len(digits) < 2 || digits[0] != '0' || digits[1] == '.' || strings.TrimLeft(s, "-+")[0] == '0'
	})
}

// byName returns the whole-value strategy of that name.
func (r *Replacer) byName(strategy string) replaceFn {
	switch strategy {
	case StrategyEmail:
		return r.Email
	case StrategyToken:
		return r.Token
	}
	return r.Universal
}

// Email replaces the local part like Universal and the domain label by label,
// with the top-level domain set to "invalid" (reserved, RFC 2606), so no
// stand-in can ever reach a real mailbox. The same domain always gets the
// same stand-in; addresses stay distinct.
func (r *Replacer) Email(s string) (string, error) {
	at := strings.LastIndexByte(s, '@')
	if at <= 0 || at == len(s)-1 {
		return r.Universal(s)
	}
	local, domain := s[:at], s[at+1:]
	dom := r.domain(domain)
	seed := strings.ToLower(s)
	return r.unique(nsEmail, s, strings.ToLower, func(attempt int) string {
		return r.shape(nsEmail, local, seed, attempt) + "@" + dom
	}, nil)
}

func (r *Replacer) domain(d string) string {
	if out, ok := r.domains.Load(d); ok {
		return out.(string)
	}
	out := r.makeDomain(d)
	if r.ndomain.Load() < maxDomains {
		r.ndomain.Add(1)
		r.domains.Store(d, out)
	}
	return out
}

func (r *Replacer) makeDomain(d string) string {
	labels := strings.Split(d, ".")
	if len(labels) == 1 {
		return r.shape(nsDomain, d, strings.ToLower(d), 0) + ".invalid"
	}
	for i, l := range labels[:len(labels)-1] {
		labels[i] = r.shape(nsDomain, l, strings.ToLower(l), 0)
	}
	labels[len(labels)-1] = "invalid"
	return strings.Join(labels, ".")
}

// Token replaces a secret with a value of the same length and character
// classes: a hexadecimal value (also a UUID) stays hexadecimal in the same
// case, other letters and digits stay letters and digits, the rest stays.
// Tokens are case-sensitive, so case is not folded.
func (r *Replacer) Token(s string) (string, error) {
	if s == "" {
		return s, nil
	}
	hexOnly := true
	for _, c := range s {
		if (c >= 'g' && c <= 'z') || (c >= 'G' && c <= 'Z') {
			hexOnly = false
			break
		}
	}
	return r.unique(nsToken, s, identity, func(attempt int) string {
		if !hexOnly {
			return r.shape(nsToken, s, s, attempt)
		}
		st := r.stream(nsToken, s, attempt)
		out := []byte(s)
		for i, c := range out {
			switch {
			case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
				out[i] = "0123456789abcdef"[st.next(16)]
			case c >= 'A' && c <= 'F':
				out[i] = "0123456789ABCDEF"[st.next(16)]
			}
		}
		return string(out)
	}, nil)
}

// bcryptHashes are hashes of TestPassword for each cost, made once with PHP's
// password_hash (bcrypt has no implementation in the standard library). A
// $2a$ or $2b$ hash gets the same hash under its own prefix: the algorithm is
// the same for an ASCII password.
var bcryptHashes = map[int]string{
	4:  "$2y$04$PBgzfzId62iz09HwM2zMXe4b4DCEe3tFMxVeCZF451FAKxeNvXcTW",
	5:  "$2y$05$76TYAiYJUkRa8PgiVpEWUOso9ya2cL14KNZ1dD1E6jF3RqrECspKu",
	6:  "$2y$06$Mq41pumA79VPvffjmmuWgu/X6UyABEpG6343QwgxW4Sn4Oe2O5kYa",
	7:  "$2y$07$MPSgmctAbmIPuGXFjwmbF.24y56Vv1s8L1LgXA9htW0CKI241GJ5e",
	8:  "$2y$08$xCHPvYyshH/Bt34bBMAoyu1t0W5qFyPpeNPxiY05kjfE5sbosPXC6",
	9:  "$2y$09$SFZYDhQjum9Mkq8U6ok0j.62l0TsKYzuNsUq9HdEr8OSjhiN0bMhu",
	10: "$2y$10$UA27VjnZnIYNSwBEzD1bq.divaC6Z62cXSBxtud9Ddo7GSGMwtF02",
	11: "$2y$11$xvgjz6VTS3mvOJqzRT6xvebee2aIHoWy1ZCvZLGZs1x9h8hhIp.Oi",
	12: "$2y$12$tvnlwwyQV3En98U1NRViKuHJGNWC2g8Dr6GVtTiiG/lIypy.z8IyC",
	13: "$2y$13$gTkMEcO1qCBtaRDyU3XPjOZHG0xsEv2HMathIqT0vBOaTBF87vNEq",
	14: "$2y$14$20kUd8YWoeVt1ZW05NfiA.FH6Xem7CH9vhF.NbrQFaDiNz.6I2R8u",
	15: "$2y$15$WjR1S/vsZItrqkVGSK2f6.CHftGF7Idsk7WqFevspvMtVpSvNKEo.",
}

// Argon2 hashes of TestPassword with PHP's default parameters (m=65536, t=4,
// p=1); the parameters of the original are not kept.
const (
	argon2idHash = "$argon2id$v=19$m=65536,t=4,p=1$fWExvmWq/bjkuj+Pk+J34Q$OIX7oAxvgjABuhRYBowNkYu7dcmSYgomE57f1+4hPUs"
	argon2iHash  = "$argon2i$v=19$m=65536,t=4,p=1$0nZQ+PyDn+pmfVg6RoEmVw$xGfoOEm09UcO2Dg/WP4w2PN/gHQRz5xhYk2e/TYud48"
)

var (
	pbkdf2Mu    sync.Mutex
	pbkdf2Cache = map[int]string{}
	hexValue    = func(s string, n int) bool {
		if len(s) != n {
			return false
		}
		_, err := hex.DecodeString(s)
		return err == nil
	}
)

// Password replaces a password hash with a hash of TestPassword in the same
// format, recognised by its prefix: bcrypt ($2y$, $2a$, $2b$, cost kept),
// Argon2 ($argon2id$, $argon2i$), Django PBKDF2 (iterations kept, a fixed
// salt) and unsalted MD5, SHA-1 and SHA-256 in hex. A value in another format
// becomes a token-like stand-in that matches no password; known reports which.
func (r *Replacer) Password(s string) (out string, known bool, err error) {
	switch {
	case s == "":
		return s, true, nil
	case len(s) == 60 && (strings.HasPrefix(s, "$2y$") || strings.HasPrefix(s, "$2a$") || strings.HasPrefix(s, "$2b$")):
		cost, err := strconv.Atoi(s[4:6])
		if err != nil {
			break
		}
		cost = min(max(cost, 4), 15)
		return s[:4] + bcryptHashes[cost][4:], true, nil
	case strings.HasPrefix(s, "$argon2id$"):
		return argon2idHash, true, nil
	case strings.HasPrefix(s, "$argon2i$"):
		return argon2iHash, true, nil
	case strings.HasPrefix(s, "pbkdf2_sha256$"):
		parts := strings.Split(s, "$")
		iter, err := strconv.Atoi(parts[1])
		if len(parts) != 4 || err != nil || iter <= 0 || iter > 10_000_000 {
			break
		}
		pbkdf2Mu.Lock()
		defer pbkdf2Mu.Unlock()
		if h, ok := pbkdf2Cache[iter]; ok {
			return h, true, nil
		}
		const salt = "driftgatesalt"
		dk, err := pbkdf2.Key(sha256.New, TestPassword, []byte(salt), iter, 32)
		if err != nil {
			return "", false, err
		}
		h := fmt.Sprintf("pbkdf2_sha256$%d$%s$%s", iter, salt, base64.StdEncoding.EncodeToString(dk))
		pbkdf2Cache[iter] = h
		return h, true, nil
	case hexValue(s, 32):
		return caseLike(s, md5Hex(TestPassword)), true, nil
	case hexValue(s, 40):
		return caseLike(s, sha1Hex(TestPassword)), true, nil
	case hexValue(s, 64):
		return caseLike(s, sha256Hex(TestPassword)), true, nil
	}
	out, err = r.Token(s)
	return out, false, err
}

func md5Hex(s string) string    { h := md5.Sum([]byte(s)); return hex.EncodeToString(h[:]) }
func sha1Hex(s string) string   { h := sha1.Sum([]byte(s)); return hex.EncodeToString(h[:]) }
func sha256Hex(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// caseLike writes h (lowercase hex) in upper case when like is upper case.
func caseLike(like, h string) string {
	if strings.ToUpper(like) == like {
		return strings.ToUpper(h)
	}
	return h
}

// Digest is a keyed hash of a value for the leak check: data verify looks
// for the digests of original personal values in the output without keeping
// the values themselves. It uses its own derived key.
func (r *Replacer) Digest(value string) [32]byte {
	mac := r.mac()
	mac.Write([]byte("leak check\x00"))
	mac.Write([]byte(value))
	var out [32]byte
	mac.Sum(out[:0])
	r.macs.Put(mac)
	return out
}

// mac is an HMAC under the replacement key, ready for input; put it back
// into r.macs after use.
func (r *Replacer) mac() hash.Hash {
	mac, _ := r.macs.Get().(hash.Hash)
	if mac == nil {
		return hmac.New(sha256.New, r.key)
	}
	mac.Reset()
	return mac
}

// collation is what a MySQL or MariaDB collation counts as equal, as the
// server says (WEIGHT_STRING): each character of a candidate set (Latin,
// Greek, Cyrillic, general punctuation, full-width ASCII) maps to one
// representative of the characters of the same weight, or to two ASCII
// letters for one that weighs as two (ß as ss under a UCA collation), or to
// nothing when it is ignored. Values with the same key are equal under the
// collation; with PAD SPACE trailing spaces do not count. A character
// outside the set is its own key (lower case under a case-insensitive
// collation). Contractions of language collations (Croatian lj, nj, dž) are
// not modelled: they change the order of values, not which are equal.
type collation struct {
	name  string
	pad   bool
	ci    bool
	canon map[rune]string
}

var collationRanges = [][2]rune{{0x20, 0x7e}, {0xa0, 0x24f}, {0x370, 0x3ff}, {0x400, 0x52f}, {0x1e00, 0x1eff}, {0x2000, 0x206f}, {0xff01, 0xff5e}}

// trim splits off the trailing spaces a PAD SPACE collation ignores.
func (co *collation) trim(s string) (body, trail string) {
	if !co.pad {
		return s, ""
	}
	body = strings.TrimRight(s, " ")
	return body, s[len(body):]
}

// key is the text that values equal under the collation share.
func (co *collation) key(s string) string {
	s, _ = co.trim(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, c := range s {
		k, ok := co.canon[c]
		if !ok && co.ci {
			c = unicode.ToLower(c)
			k, ok = co.canon[c]
		}
		if !ok {
			k = string(c)
		}
		b.WriteString(k)
	}
	return b.String()
}

var collationName = lazy.Regexp(`^[A-Za-z0-9_]+$`)

// hashSet is a set of 64-bit hashes: appended, then sorted without repeats
// when it has doubled and when it is read, 8 bytes a value.
type hashSet struct {
	h      []uint64
	sorted int
}

func (s *hashSet) add(h uint64) {
	s.h = append(s.h, h)
	if len(s.h) >= 2*max(s.sorted, 1<<14) {
		s.compact()
	}
}

func (s *hashSet) compact() {
	slices.Sort(s.h)
	s.h = slices.Compact(s.h)
	s.sorted = len(s.h)
}

func (s *hashSet) has(h uint64) bool {
	if s == nil {
		return false
	}
	if s.sorted != len(s.h) {
		s.compact()
	}
	_, ok := slices.BinarySearch(s.h, h)
	return ok
}

// Reserve takes one original value of the source (PersonalValues gives them
// all, before anything is replaced): no stand-in of another value may be
// equal to it, or the output would hold a real value of someone else (a
// replaced phone number that is another person's number). A value that
// would be replaced by Universal or Number is reserved in its namespace and
// in that of every collation loaded; a token in its own. E-mail stand-ins
// are at .invalid domains and can never be an original.
func (r *Replacer) Reserve(v PersonalValue) error {
	if v.Value == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	add := func(ns, folded string) {
		if r.taken[ns] == nil {
			r.taken[ns] = &hashSet{}
		}
		r.taken[ns].add(hash64(folded))
	}
	switch v.Kind {
	case StrategyEmail, StrategyPassword, StrategyNull, StrategyEmpty:
	case StrategyToken:
		if v.Category == CatNumeric {
			add(nsUniversal, strings.ToLower(v.Value))
		} else {
			add(nsToken, v.Value)
		}
	default:
		add(nsUniversal, strings.ToLower(v.Value))
		for name, co := range r.colls {
			add(nsUniversal+"\x00"+name, co.key(v.Value))
		}
	}
	return nil
}

// prepare readies the replacer for a run over a source: the collations of
// its columns, then its original values (see Reserve), read in the snapshot
// of the run. With x, the same pass over the originals fills the index of
// data verify (leakIndex.fill), so the source is read once for both.
func (r *Replacer) prepare(ctx context.Context, c *sql.Conn, dl dialect, det *pii.Detector, s *Schema, rules *Rules, x *leakIndex) error {
	if err := r.loadCollations(ctx, c, s, rules); err != nil {
		return err
	}
	if x == nil {
		return eachPersonal(ctx, c, dl, det, s, rules, nil, r.Reserve)
	}
	each := func(fn func(PersonalValue) error) error { return eachPersonal(ctx, c, dl, det, s, rules, x.ahead, fn) }
	return x.fill(each, r.Reserve)
}

// madeBeforeReserved reports whether a stand-in made so far is an original
// value (reserved before or after it was made, with later set). When none
// is, every stand-in is the one it would be had all originals been
// reserved first: unique skips the same candidates (those of other inputs;
// the originals among them were never accepted) and accepts the same, one
// value after the other.
func (r *Replacer) madeBeforeReserved() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for ns, reg := range r.reg {
		for out, in := range reg {
			if out != in && r.taken[ns].has(out) {
				return true
			}
		}
	}
	return false
}

// loadCollations reads the collations of the text columns that universal
// and e-mail rules replace (MySQL and MariaDB; a PostgreSQL collation is
// deterministic, comparing bytes, or refused by Rules.Check). The columns'
// stand-ins then follow them (Replacer.ForColumn).
func (r *Replacer) loadCollations(ctx context.Context, c *sql.Conn, s *Schema, rules *Rules) error {
	if s.Driver != MySQL && s.Driver != "mariadb" {
		return nil
	}
	need := map[string]bool{}
	for _, t := range s.Tables {
		tr := rules.Tables[t.Name]
		if tr == nil || tr.emptied() {
			continue
		}
		for _, col := range t.Columns {
			if cr := tr.Columns[col.Name]; cr != nil && col.Category == CatText && col.Collation != "" && cr.Kind == KindPersonal &&
				(cr.Strategy == StrategyUniversal || cr.Strategy == StrategyEmail) {
				need[col.Collation] = true
			}
		}
	}
	for _, name := range sortedKeys(need) {
		if _, ok := r.colls[name]; ok {
			continue
		}
		co, err := readCollation(ctx, c, name)
		if err != nil {
			return fmt.Errorf("collation %s: %w", name, err)
		}
		r.mu.Lock()
		r.colls[name] = co
		r.mu.Unlock()
	}
	return nil
}

func readCollation(ctx context.Context, c *sql.Conn, name string) (*collation, error) {
	if !collationName().MatchString(name) {
		return nil, errors.New("not a collation name")
	}
	// The character set starts the name (MariaDB lists utf8mb4_uca1400_ai_ci
	// in COLLATIONS only as uca1400_ai_ci); no character set name has a "_".
	charset, _, _ := strings.Cut(name, "_")
	text := func(s string) string {
		return "CONVERT(CONVERT(X'" + hex.EncodeToString([]byte(s)) + "' USING utf8mb4) USING " + charset + ")"
	}
	co := &collation{name: name, canon: map[rune]string{}}
	if err := c.QueryRowContext(ctx, "SELECT "+text("a")+" COLLATE "+name+" = "+text("a ")+" COLLATE "+name).Scan(&co.pad); err != nil {
		return nil, err
	}
	// The weight of every candidate, and of every pair of ASCII letters.
	var cands []string
	for _, rg := range collationRanges {
		for r := rg[0]; r <= rg[1]; r++ {
			if !unicode.Is(unicode.Mn, r) && !unicode.IsControl(r) {
				cands = append(cands, string(r))
			}
		}
	}
	letters := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	pairs := len(cands)
	for i := 0; i < len(letters); i++ {
		for j := 0; j < len(letters); j++ {
			cands = append(cands, letters[i:i+1]+letters[j:j+1])
		}
	}
	weight := make([]string, len(cands))
	fits := make([]bool, len(cands))
	const batch = 250
	for from := 0; from < len(cands); from += batch {
		var q strings.Builder
		for i := from; i < min(from+batch, len(cands)); i++ {
			if i > from {
				q.WriteString(" UNION ALL ")
			}
			t := text(cands[i])
			fmt.Fprintf(&q, "SELECT %d, HEX(WEIGHT_STRING(%s COLLATE %s)), HEX(CONVERT(%s USING utf8mb4)) = '%s'",
				i, t, name, t, strings.ToUpper(hex.EncodeToString([]byte(cands[i]))))
		}
		rows, err := queryStrings(ctx, c, q.String())
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			i, err := strconv.Atoi(row[0])
			if err != nil {
				return nil, err
			}
			weight[i], fits[i] = row[1], row[2] == "1"
		}
	}
	// The representative of each weight: ASCII before the rest, lower case
	// before upper case, then the lowest code point.
	better := func(a, b rune) bool {
		if (a < utf8.RuneSelf) != (b < utf8.RuneSelf) {
			return a < utf8.RuneSelf
		}
		if unicode.IsLower(a) != unicode.IsLower(b) {
			return unicode.IsLower(a)
		}
		return a < b
	}
	single := map[string]rune{}
	for i := 0; i < pairs; i++ {
		if !fits[i] || weight[i] == "" {
			continue
		}
		r, _ := utf8.DecodeRuneInString(cands[i])
		if cur, ok := single[weight[i]]; !ok || better(r, cur) {
			single[weight[i]] = r
		}
	}
	two := map[string]string{} // weight → the first pair of ASCII letters with it
	for i := pairs; i < len(cands); i++ {
		if _, ok := two[weight[i]]; !ok && fits[i] {
			two[weight[i]] = cands[i]
		}
	}
	for i := 0; i < pairs; i++ {
		if !fits[i] {
			continue
		}
		r, _ := utf8.DecodeRuneInString(cands[i])
		switch p, ok := two[weight[i]]; {
		case weight[i] == "":
			co.canon[r] = ""
		case ok:
			co.canon[r] = string(single[weight[pairIndex(cands, pairs, p[0])]]) + string(single[weight[pairIndex(cands, pairs, p[1])]])
		default:
			co.canon[r] = string(single[weight[i]])
		}
	}
	co.ci = co.canon['A'] == co.canon['a']
	return co, nil
}

// pairIndex finds the candidate of an ASCII character.
func pairIndex(cands []string, n int, c byte) int {
	for i := 0; i < n; i++ {
		if cands[i] == string(c) {
			return i
		}
	}
	return 0
}

// ForColumn returns the strategy for the values of one column: Universal
// and Email follow the column's collation, so values it counts as equal
// (case, accents, trailing spaces) get stand-ins it counts as equal, and
// values it counts as different get stand-ins it counts as different. A
// numeric column gets Number.
func (r *Replacer) ForColumn(c *Column, strategy string) replaceFn {
	if isArray(c) {
		fn := r.byName(strategy)
		return func(s string) (string, error) { return mapArray(s, fn) }
	}
	if c.Category == CatNumeric && (strategy == StrategyUniversal || strategy == StrategyToken) {
		return r.Number
	}
	r.mu.Lock()
	co := r.colls[c.Collation]
	r.mu.Unlock()
	switch {
	case co == nil || c.Category != CatText:
		return r.byName(strategy)
	case strategy == StrategyUniversal:
		return func(s string) (string, error) { return r.universalIn(co, s) }
	case strategy == StrategyEmail:
		return func(s string) (string, error) { return r.emailIn(co, s) }
	}
	return r.byName(strategy)
}

func (r *Replacer) universalIn(co *collation, s string) (string, error) {
	if s == "" {
		return s, nil
	}
	if plainNumber().MatchString(s) {
		return r.number(s)
	}
	body, trail := co.trim(s)
	key := co.key(body)
	return r.unique(nsUniversal+"\x00"+co.name, s, co.key, func(attempt int) string {
		return recase(r.shape(nsUniversal, key, key, attempt), body) + trail
	}, nil)
}

func (r *Replacer) emailIn(co *collation, s string) (string, error) {
	body, trail := co.trim(s)
	at := strings.LastIndexByte(body, '@')
	if at <= 0 || at == len(body)-1 {
		return r.universalIn(co, s)
	}
	local := body[:at]
	lk, seed := co.key(local), co.key(body)
	dom := r.domain(co.key(body[at+1:]))
	return r.unique(nsEmail+"\x00"+co.name, s, co.key, func(attempt int) string {
		return recase(r.shape(nsEmail, lk, seed, attempt), local) + "@" + dom + trail
	}, nil)
}

// recase writes the letters of out in the case of the letters of like at
// the same places, when both have as many characters.
func recase(out, like string) string {
	o, l := []rune(out), []rune(like)
	if len(o) != len(l) {
		return out
	}
	for i, c := range l {
		switch {
		case unicode.IsUpper(c):
			o[i] = unicode.ToUpper(o[i])
		case unicode.IsLower(c):
			o[i] = unicode.ToLower(o[i])
		}
	}
	return string(o)
}

// mapArray replaces the elements of a PostgreSQL array literal one by one
// ({"Ana Ilić",ana@x.rs,NULL}); quotes, separators and NULL stay, and an
// element is written quoted when its stand-in needs it.
func mapArray(s string, fn replaceFn) (string, error) {
	var b strings.Builder
	var el strings.Builder
	quoted, inQuote := false, false
	flush := func() error {
		e := el.String()
		el.Reset()
		if !quoted && (e == "" || strings.EqualFold(e, "NULL")) {
			b.WriteString(e)
			return nil
		}
		out, err := fn(e)
		if err != nil {
			return err
		}
		if quoted || out == "" || strings.EqualFold(out, "NULL") || strings.ContainsAny(out, `{}," \`) {
			b.WriteString(`"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(out) + `"`)
		} else {
			b.WriteString(out)
		}
		quoted = false
		return nil
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inQuote && c == '\\' && i+1 < len(s):
			i++
			el.WriteByte(s[i])
		case inQuote && c == '"':
			inQuote = false
		case inQuote:
			el.WriteByte(c)
		case c == '"':
			inQuote, quoted = true, true
		case c == '{':
			b.WriteByte(c)
		case c == ',' || c == '}':
			if el.Len() > 0 || quoted {
				if err := flush(); err != nil {
					return "", err
				}
			}
			b.WriteByte(c)
		default:
			el.WriteByte(c)
		}
	}
	return b.String(), nil
}
