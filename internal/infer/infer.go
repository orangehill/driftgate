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

// runFormat is the format of a run of letters and digits that differs
// between the samples (parts), before and after the literal text around
// it. It accepts what the samples' own form allows and little more:
//
//   - digits keep their width ({n}, or {min,max} of the samples), but for
//     a count, measure or id that grows: a run after a key such as id or
//     count, a segment of a path (/jobs/7), or before a unit (ms, s, KB
//     …), takes \d+;
//   - letters only are one of the words seen ((?:pending|shipped): a new
//     status is a change), but a token of eight or more letters of one
//     width keeps its class and width;
//   - hex keeps its width (a hash, an id);
//   - letters and digits keep their width ({n} or {min,max}), but a long
//     token (16 or more, a session cookie) takes as many as it needs.
func runFormat(parts []string, before, after string, grow bool) string {
	c := class(parts)
	lo, hi := len(parts[0]), 0
	for _, p := range parts {
		lo, hi = min(lo, len(p)), max(hi, len(p))
	}
	width := func() string {
		switch {
		case lo == hi && lo == 1:
			return ""
		case lo == hi:
			return fmt.Sprintf("{%d}", lo)
		}
		return fmt.Sprintf("{%d,%d}", lo, hi)
	}
	letters := true
	for _, p := range parts {
		letters = letters && !strings.ContainsAny(p, "0123456789")
	}
	switch {
	case c == `\d`:
		// An id with a word before it (user_42) grows; a code of one width
		// of three or more (HTTP_401, error_code_403) keeps it.
		suffixed := suffixedID().MatchString(before) && !(lo == hi && lo >= 3)
		if counterKey().MatchString(before) || unitAfter().MatchString(after) || pathID(before, after) || suffixed || grow {
			return `\d+`
		}
		return c + width()
	case letters && !(lo == hi && lo >= 8):
		return alternation(parts)
	case c == `[0-9a-f]`, hi < 16:
		return c + width()
	}
	return c + "+"
}

// grows matches the part of a value whose digits count up: a version
// (v1.2.3) or a range of a list (Showing 1-10 of 120). Only digits inside
// it that differ between the samples grow; the rest of the value keeps its
// own form (status 200 after a version stays 200).
var grows = lazy.Regexp(`(?i)\bv\d+\.\d+\.\d+|showing \d+\s*[-–]\s*\d+ of \d+`)

// growing is where the parts that grow are in each sample, by byte
// (spans), and where each token starts (offsets); nil when no sample has
// one.
type growing struct {
	spans   [][][]int
	offsets [][]int
}

func growingParts(values []string, toks [][]token) *growing {
	g := &growing{spans: make([][][]int, len(values)), offsets: make([][]int, len(values))}
	for i, v := range values {
		if g.spans[i] = grows().FindAllStringIndex(v, -1); g.spans[i] == nil {
			return nil // a run must be inside one in every sample
		}
		off := make([]int, len(toks[i])+1)
		for j, t := range toks[i] {
			off[j+1] = off[j] + len(t.text)
		}
		g.offsets[i] = off
	}
	return g
}

// in reports whether run j of every sample lies inside a part of it that
// grows. The caller has checked that the samples share their tokens.
func (g *growing) in(j int) bool {
	if g == nil {
		return false
	}
	for i, spans := range g.spans {
		start, end := g.offsets[i][j], g.offsets[i][j+1]
		in := false
		for _, m := range spans {
			if m[0] <= start && end <= m[1] {
				in = true
				break
			}
		}
		if !in {
			return false
		}
	}
	return true
}

// pathID reports digits that are a segment of a path (/jobs/7/, /jobs/7?):
// an id, which grows.
func pathID(before, after string) bool {
	return strings.HasSuffix(before, "/") && (after == "" || after[0] == '/' || after[0] == '?' || after[0] == '#')
}

// alternation matches exactly the distinct parts.
func alternation(parts []string) string {
	seen := map[string]bool{}
	var list []string
	for _, p := range parts {
		if !seen[p] {
			seen[p] = true
			list = append(list, regexp.QuoteMeta(p))
		}
	}
	sort.Strings(list)
	return "(?:" + strings.Join(list, "|") + ")"
}

var (
	// counterKey ends the text before a run of digits that counts or
	// names something and grows: id 7, count=12, "views": 3.
	counterKey = lazy.Regexp(`(?i)(?:^|[^a-z])(?:id|ids|count|views|size|length|seq|num|number|bytes|\w*_?pages|of)["']?\s*[:=]?\s*["']?$`)
	// suffixedID ends the text before the digits of an id such as user_42.
	suffixedID = lazy.Regexp(`(?i)(?:^|[^a-z0-9])[a-z]+_$`)
	// unitAfter starts the text after a run of digits that measures
	// something: 12ms, 3 s, 512KB.
	unitAfter = lazy.Regexp(`(?i)^\s?(?:ms|s|sec|secs|seconds|us|µs|ns|min|minutes|h|hours|b|kb|mb|gb|bytes)(?:[^a-z]|$)`)
	// timeLike is a value with a date or a time in it, whose digits that
	// agree in the samples agree only by chance (the same day, hour).
	timeLike = lazy.Regexp(`\d{4}-\d{2}-\d{2}|\d{1,2}:\d{2}|\b(?:Mon|Tue|Wed|Thu|Fri|Sat|Sun|Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)\b`)
)

// digitsIn is the format of the digits inside a word that is the same in
// every sample (29T10 of a time, 5ms): \d+ before a unit (a duration
// grows), else their width.
func digitsIn(word string) string {
	q := regexp.QuoteMeta(word)
	var b strings.Builder
	last := 0
	for _, m := range digitRun().FindAllStringIndex(q, -1) {
		b.WriteString(q[last:m[0]])
		switch n := m[1] - m[0]; {
		case unitAfter().MatchString(q[m[1]:]):
			b.WriteString(`\d+`)
		case n == 1:
			b.WriteString(`\d`)
		default:
			fmt.Fprintf(&b, `\d{%d}`, n)
		}
		last = m[1]
	}
	b.WriteString(q[last:])
	return b.String()
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
	negative := false
	for _, v := range values {
		ints = ints && integer().MatchString(v)
		decimals = decimals && decimal().MatchString(v)
		negative = negative || strings.HasPrefix(v, "-")
	}
	switch {
	case ints && negative:
		return `-?\d+`
	case ints:
		return `\d+`
	case decimals && negative:
		return `-?\d+(?:\.\d+)?`
	case decimals:
		return `\d+(?:\.\d+)?`
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
		grown := growingParts(values, toks)
		timey := timeLike().MatchString(values[0])
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
				switch {
				case allEqual(ids):
					b.WriteString(regexp.QuoteMeta(ids[0]))
				case equalFold(ids):
					b.WriteString(`(?i:` + regexp.QuoteMeta(ids[0]) + `)`)
				default:
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
				b.WriteString(digitsIn(parts[0]))
				continue
			}
			if equal && !timey {
				// Digits that agree in every sample stay (page 1 of 3); in a
				// date or time they agree by chance (the same day).
				b.WriteString(regexp.QuoteMeta(parts[0]))
				continue
			}
			if sub := byKind(parts, timey); sub != "" {
				b.WriteString(sub)
				continue
			}
			// The word before the run with what separates them (count 7),
			// and what follows it up to the next word (12 ms).
			before, after := "", ""
			k := j - 1
			for ; k >= 0 && !toks[0][k].run; k-- {
				before = toks[0][k].text + before
			}
			if k >= 0 {
				before = toks[0][k].text + before
			}
			for k := j + 1; k < len(toks[0]); k++ {
				after += toks[0][k].text
				if toks[0][k].run {
					break
				}
			}
			b.WriteString(runFormat(parts, before, after, grown.in(j)))
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

func equalFold(s []string) bool {
	for _, v := range s {
		if !strings.EqualFold(v, s[0]) {
			return false
		}
	}
	return true
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
func byKind(parts []string, timey bool) string {
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
	// Two to four parts (896Z, 29T10, v2beta1): more of them that agree
	// are chance in a token, not its build.
	if len(split[0]) < 2 || len(split[0]) > 4 {
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
		case equal && digits && timey: // the same day or hour by chance (04T10)
			if len(col[0]) == 1 {
				b.WriteString(`\d`)
			} else {
				fmt.Fprintf(&b, `\d{%d}`, len(col[0]))
			}
		case equal:
			b.WriteString(regexp.QuoteMeta(col[0]))
		default:
			after := ""
			if j+1 < len(split[0]) {
				after = split[0][j+1] // 896Z, 12ms
			}
			b.WriteString(runFormat(col, "", after, false))
		}
	}
	return b.String()
}
