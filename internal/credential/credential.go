// Package credential tells names and values that hold credentials:
// passwords, tokens, keys, signatures. The recording pipeline (har) leaves
// such requests out of the catalog; a capture keeps responses that hold
// them out of git and masks them in headers.
package credential

import (
	"strings"
	"unicode"
	"unsafe"

	"github.com/orangehill/driftgate/internal/lazy"
)

// secretWords are words that name a credential on their own.
var secretWords = map[string]bool{
	"pass": true, "password": true, "passwd": true, "pwd": true, "passphrase": true, "passcode": true,
	"secret": true, "token": true, "apikey": true, "key": true, "auth": true, "authorization": true,
	"session": true, "sessionid": true, "sid": true, "signature": true, "sig": true,
	"otp": true, "totp": true, "pin": true, "credential": true, "credentials": true,
	"jwt": true, "bearer": true, "ticket": true, "nonce": true, "csrf": true, "xsrf": true,
	"hash": true, "salt": true, "cookie": true,
}

// secretSuffixes catch words written together in lower case
// (accesstoken, newpassword, csrfmiddlewaretoken).
var secretSuffixes = []string{"password", "passwd", "token", "secret", "apikey", "sessionid"}

// codeWords make "code" a credential: alone (OAuth's ?code=) or with one of
// them (verification_code, resetCode). A country, zip or status code is not.
var codeWords = map[string]bool{
	"auth": true, "authorization": true, "verification": true, "verify": true, "confirm": true,
	"confirmation": true, "reset": true, "otp": true, "sms": true, "mfa": true, "2fa": true,
	"totp": true, "recovery": true, "activation": true, "invite": true, "invitation": true,
	"access": true, "login": true, "security": true, "one": true,
}

// Name reports whether the name of a parameter, key, form field or header
// holds a credential. A name is split into words at every character that is
// not a letter or digit and at camelCase boundaries (currentPassword,
// authToken, XSRFToken), so a credential word counts wherever it stands in
// the name.
func Name(name string) bool { return named(name, false) }

// Key is Name for a key of a response body, whose value is checked too
// (Value): there "hash" alone does not count, as a content hash (of a
// file, a commit) is common in responses; password_hash still counts.
func Key(name string) bool { return named(name, true) }

func named(name string, body bool) bool {
	// Names are checked for every header and many keys: an ASCII name is
	// split on the stack, without allocating.
	var buf [64]byte
	var ends [16]uint8
	n, ok := asciiWords(name, &buf, &ends)
	var slow []string
	if !ok {
		slow = nameWords(name)
		n = len(slow)
	}
	word := func(i int) string {
		if slow != nil {
			return slow[i]
		}
		start := 0
		if i > 0 {
			start = int(ends[i-1])
		}
		return unsafe.String(&buf[start], int(ends[i])-start) // only compared, never kept
	}
	for i := 0; i < n; i++ {
		w := word(i)
		switch {
		case body && w == "hash":
		case secretWords[w]:
			return true
		case strings.HasPrefix(w, "csrf"), strings.HasPrefix(w, "xsrf"):
			return true
		case i == 0 && (w == "access" || w == "refresh"): // access, refresh_token (SimpleJWT, OAuth)
			return true
		case w == "code":
			if n == 1 {
				return true
			}
			for j := 0; j < n; j++ {
				if codeWords[word(j)] {
					return true
				}
			}
		}
		for _, s := range secretSuffixes {
			if strings.HasSuffix(w, s) {
				return true
			}
		}
	}
	return false
}

// asciiWords is nameWords for a name of ASCII letters, digits and
// separators of up to 64 bytes and 16 words: their letters go into buf in
// lower case, ends[i] is where word i ends there. ok is false for another
// name.
func asciiWords(name string, buf *[64]byte, ends *[16]uint8) (n int, ok bool) {
	if len(name) > len(buf) {
		return 0, false
	}
	at := 0
	flush := func() bool {
		if at > 0 && (n == 0 || int(ends[n-1]) < at) {
			if n == len(ends) {
				return false
			}
			ends[n] = uint8(at)
			n++
		}
		return true
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 0x80:
			return 0, false
		case c >= 'A' && c <= 'Z':
			if i > 0 {
				prev := name[i-1]
				nextLower := i+1 < len(name) && name[i+1] >= 'a' && name[i+1] <= 'z'
				if (prev >= 'a' && prev <= 'z' || prev >= '0' && prev <= '9' || prev >= 'A' && prev <= 'Z' && nextLower) && !flush() {
					return 0, false
				}
			}
			c += 'a' - 'A'
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		default:
			if !flush() {
				return 0, false
			}
			continue
		}
		buf[at] = c
		at++
	}
	return n, flush()
}

// nameWords splits a name into lower-case words: at characters other than
// letters and digits, before an upper-case letter that follows a lower-case
// one or a digit (newPassword), and before the last capital of a run that a
// lower-case letter follows (XSRFToken: xsrf, token).
func nameWords(name string) []string {
	var words []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			words = append(words, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	rs := []rune(name)
	for i, r := range rs {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			continue
		}
		if unicode.IsUpper(r) && i > 0 {
			prev := rs[i-1]
			nextLower := i+1 < len(rs) && unicode.IsLower(rs[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
				flush()
			}
		}
		cur = append(cur, r)
	}
	flush()
	return words
}

// JWT reports a JSON Web Token anywhere in v.
func JWT(v string) bool { return MayHoldJWT(v) && jwtShape().MatchString(v) }

// MayHoldJWT reports whether v has "eyJ", with which every JWT starts: a
// quick test before JWT, also of a whole body. It looks for the J, which
// is rare in text, where a search for "eyJ" would stop at every e.
func MayHoldJWT(v string) bool {
	for i := 0; i < len(v); {
		j := strings.IndexByte(v[i:], 'J')
		if j < 0 {
			return false
		}
		if i += j; i >= 2 && v[i-2] == 'e' && v[i-1] == 'y' {
			return true
		}
		i++
	}
	return false
}

// jwtShape is the start of a JSON Web Token: a base64url JSON header, a
// payload and a dot.
var jwtShape = lazy.Regexp(`eyJ[\w-]+\.[\w-]+\.`)

// uuidShape is a UUID with its dashes: an identifier of a resource, not a
// credential, so it is not taken for a token.
var uuidShape = lazy.Regexp(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// tokenRun is a run of base64 and base64url characters long enough to be a
// generated token.
var tokenRun = lazy.Regexp(`[A-Za-z0-9_+=-]{20,}`)

// Value reports a value shaped like a credential: a JWT anywhere in it, or
// a run of at least 20 token characters that looks random (randomToken).
func Value(v string) bool {
	if JWT(v) {
		return true
	}
	v = uuidShape().ReplaceAllString(v, " ")
	for _, run := range tokenRun().FindAllString(v, -1) {
		if randomToken(run) {
			return true
		}
	}
	return false
}

// randomToken tells a generated token from an identifier or a word. Hex
// counts from 32 characters with letters and digits (an MD5, SHA or reset
// token; a 24-character MongoDB ObjectID or a short commit hash is an
// identifier). Otherwise the run must mix at least two of upper case, lower
// case and digits, and change between them often: its runs of one class
// are two characters long or shorter on average, or it has digits in two
// places or more and changes at least every third character. Random
// base64url passes in about 98% of 32-character tokens (99.5% of 40, 94%
// of 20); a camelCase name with a number at the end
// (getUserProfileById2024), a slug (my-post-2024), a CONSTANT_NAME or a long
// number does not. Short camelCase words with digits inside
// (v2ApiUserProfile3Settings) are taken for a token: such a request is
// added by hand.
func randomToken(run string) bool {
	hex := true
	letters, digits, digitGroups, alnum := 0, 0, 0, 0
	var classes [4]bool
	changes, prev := 0, 0
	for _, r := range run {
		class := 0
		switch {
		case r >= '0' && r <= '9':
			class, digits = 1, digits+1
			if prev != 1 {
				digitGroups++
			}
		case r >= 'a' && r <= 'z':
			class, letters = 2, letters+1
		case r >= 'A' && r <= 'Z':
			class, letters = 3, letters+1
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			hex = false
		}
		if class != 0 {
			alnum++
			classes[class] = true
			if prev != 0 && class != prev {
				changes++
			}
		}
		prev = class
	}
	if hex {
		return len(run) >= 32 && letters > 0 && digits > 0
	}
	if n := btoi(classes[1]) + btoi(classes[2]) + btoi(classes[3]); n < 2 {
		return false
	}
	return 2*(changes+1) >= alnum || (digitGroups >= 2 && 3*changes >= alnum)
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
