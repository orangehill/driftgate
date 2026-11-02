// Package infer proposes allowlist formats: a regular expression that matches
// every observed value of something that changes on its own (a time, a token,
// an id), and as little else as it can.
package infer

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/orangehill/driftgate/internal/lazy"
)

type token struct {
	run  bool   // a run of letters and digits
	text string // a literal character otherwise
}

func tokens(s string) []token {
	var out []token
	for _, r := range s {
		alnum := r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
		if alnum && len(out) > 0 && out[len(out)-1].run {
			out[len(out)-1].text += string(r)
			continue
		}
		out = append(out, token{run: alnum, text: string(r)})
	}
	return out
}

// class is the smallest of a few common character classes that holds every
// character of the strings.
func class(parts []string) string {
	var digit, lower, upper, hexLetters = false, false, false, true
	for _, p := range parts {
		for _, r := range p {
			switch {
			case r >= '0' && r <= '9':
				digit = true
			case r >= 'a' && r <= 'z':
				lower = true
				hexLetters = hexLetters && r <= 'f'
			case r >= 'A' && r <= 'Z':
				upper = true
				hexLetters = false
			}
		}
	}
	switch {
	case digit && !lower && !upper:
		return `\d`
	case digit && lower && !upper && hexLetters:
		return `[0-9a-f]`
	}
	c := ""
	if digit {
		c += "0-9"
	}
	if upper {
		c += "A-Z"
	}
	if lower {
		c += "a-z"
	}
	return "[" + c + "]"
}

func quantifier(parts []string) string {
	lo, hi := -1, 0
	for _, p := range parts {
		n := len(p)
		if lo < 0 || n < lo {
			lo = n
		}
		if n > hi {
			hi = n
		}
	}
	switch {
	case lo != hi:
		return "+"
	case lo == 1:
		return ""
	}
	return fmt.Sprintf("{%d}", lo)
}

// Pattern returns a format for the values, or "" when they are all equal.
// Values with the same punctuation in the same places get a format that
// keeps it (\d{4}-\d{2}-\d{2}); others get one class for the whole value.
func Pattern(values []string) string {
	if len(values) == 0 {
		return ""
	}
	same, empty := true, false
	for _, v := range values {
		same = same && v == values[0]
		empty = empty || v == ""
	}
	if same {
		return ""
	}
	// Numbers: any integer or decimal, signed, since a value that changes by
	// itself can take any of them.
	ints, decimals := true, true
	for _, v := range values {
		ints = ints && integer().MatchString(v)
		decimals = decimals && decimal().MatchString(v)
	}
	switch {
	case ints:
		return `-?\d+`
	case decimals:
		return `-?\d+(?:\.\d+)?`
	}
	toks := make([][]token, len(values))
	for i, v := range values {
		toks[i] = tokens(v)
	}
	shared := true
	for _, t := range toks[1:] {
		if len(t) != len(toks[0]) {
			shared = false
			break
		}
		for j := range t {
			if t[j].run != toks[0][j].run || (!t[j].run && t[j].text != toks[0][j].text) {
				shared = false
			}
		}
	}
	var b strings.Builder
	if shared {
		for j := 0; j < len(toks[0]); j++ {
			t := toks[0][j]
			if !t.run {
				b.WriteString(regexp.QuoteMeta(t.text))
				continue
			}
			// A UUID that changes is any UUID of the samples' case: its
			// groups are hex by chance digits only (0308), so a format built
			// from the samples would reject the next one. One that is the
			// same everywhere (a tenant) stays as it is.
			if ids := uuidAt(toks, j); ids != nil {
				if allEqual(ids) {
					b.WriteString(regexp.QuoteMeta(ids[0]))
				} else {
					b.WriteString(uuidFormat(ids))
				}
				j += 8 // five runs and the four dashes between them
				continue
			}
			parts := make([]string, len(toks))
			equal := true
			for i := range toks {
				parts[i] = toks[i][j].text
				equal = equal && parts[i] == parts[0]
			}
			// A word that is the same everywhere stays as it is (a cookie
			// name, a weekday); digits stay a class, since times and counters
			// can agree by chance in a few runs.
			if equal && calendarWord().MatchString(parts[0]) {
				b.WriteString(`[A-Z][a-z]{2}`) // the day and month of a date change with time
				continue
			}
			if equal && !allDigits(parts[0]) {
				// Letters stay; digits inside the word still vary (29T10 in a
				// time, which only agreed by chance).
				b.WriteString(digitRun().ReplaceAllStringFunc(regexp.QuoteMeta(parts[0]), func(d string) string {
					if len(d) == 1 {
						return `\d`
					}
					return fmt.Sprintf(`\d{%d}`, len(d))
				}))
				continue
			}
			if sub := byKind(parts); sub != "" {
				b.WriteString(sub)
				continue
			}
			b.WriteString(class(parts) + quantifier(parts))
		}
	} else {
		// One class for everything: letters and digits by range, the rest
		// listed one by one.
		alnum := class(values)
		other := map[rune]bool{}
		for _, v := range values {
			for _, r := range v {
				if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
					other[r] = true
				}
			}
		}
		set := strings.Trim(alnum, "[]")
		if alnum == `\d` {
			set = "0-9"
		}
		rs := make([]rune, 0, len(other))
		for r := range other {
			rs = append(rs, r)
		}
		sort.Slice(rs, func(i, j int) bool { return rs[i] < rs[j] })
		for _, r := range rs {
			if strings.ContainsRune(`-\]^[`, r) {
				set += `\`
			}
			set += string(r)
		}
		if empty {
			b.WriteString("[" + set + "]*")
		} else {
			b.WriteString("[" + set + "]+")
		}
	}
	p := b.String()
	re := regexp.MustCompile("^(?:" + p + ")$")
	for _, v := range values {
		if !re.MatchString(v) {
			// Should not happen; never propose a format that rejects a seen value.
			quoted := make([]string, len(values))
			for i, v := range values {
				quoted[i] = regexp.QuoteMeta(v)
			}
			return strings.Join(quoted, "|")
		}
	}
	return p
}

var (
	digitRun = lazy.Regexp(`\d+`)
	integer  = lazy.Regexp(`^-?\d+$`)
	decimal  = lazy.Regexp(`^-?\d+(?:\.\d+)?$`)
)

var uuid = lazy.Regexp(`^(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// uuidFormat matches any UUID with the letters' case of the samples: a
// serializer that changes the case changes the contract. Samples without
// letters get lowercase.
func uuidFormat(ids []string) string {
	lower, upper := false, false
	for _, id := range ids {
		lower = lower || strings.ContainsAny(id, "abcdef")
		upper = upper || strings.ContainsAny(id, "ABCDEF")
	}
	c := `[0-9a-f]`
	switch {
	case lower && upper:
		c = `[0-9a-fA-F]`
	case upper:
		c = `[0-9A-F]`
	}
	return fmt.Sprintf(`%[1]s{8}-%[1]s{4}-%[1]s{4}-%[1]s{4}-%[1]s{12}`, c)
}

// uuidAt returns the UUID of every value at token j (five runs with dashes
// between them), or nil when one value has none there. The caller has
// checked that the values share their tokens.
func uuidAt(toks [][]token, j int) []string {
	ids := make([]string, len(toks))
	for i, t := range toks {
		if j+9 > len(t) {
			return nil
		}
		var s strings.Builder
		for _, x := range t[j : j+9] {
			s.WriteString(x.text)
		}
		if !uuid().MatchString(s.String()) {
			return nil
		}
		ids[i] = s.String()
	}
	return ids
}

func allEqual(s []string) bool {
	for _, v := range s {
		if v != s[0] {
			return false
		}
	}
	return true
}

var calendarWord = lazy.Regexp(`^(Mon|Tue|Wed|Thu|Fri|Sat|Sun|Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)$`)

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

var (
	kindRun = lazy.Regexp(`\d+|[A-Za-z]+`)
	hexRun  = lazy.Regexp(`^(?:[0-9a-f]+|[0-9A-F]+)$`)
)

// byKind describes runs of letters and digits that differ but share their
// build (896Z, 112Z): digits as \d, letters that agree as they are, letters
// that differ as a class. It returns "" when the builds differ (a hex token).
func byKind(parts []string) string {
	// A run of hex digits only is a token (a hash, an id): its build is chance.
	hexOnly := true
	for _, p := range parts {
		hexOnly = hexOnly && hexRun().MatchString(p)
	}
	if hexOnly {
		return ""
	}
	split := make([][]string, len(parts))
	for i, p := range parts {
		split[i] = kindRun().FindAllString(p, -1)
		if len(split[i]) != len(split[0]) {
			return ""
		}
	}
	if len(split[0]) < 2 {
		return ""
	}
	var b strings.Builder
	for j := range split[0] {
		col := make([]string, len(parts))
		digits, equal := true, true
		for i := range split {
			col[i] = split[i][j]
			isDigits := allDigits(col[i])
			if i > 0 && isDigits != digits {
				return ""
			}
			digits = isDigits
			equal = equal && col[i] == col[0]
		}
		switch {
		case digits:
			b.WriteString(`\d` + quantifier(col))
		case equal:
			b.WriteString(regexp.QuoteMeta(col[0]))
		default:
			b.WriteString(class(col) + quantifier(col))
		}
	}
	return b.String()
}
