package data

import (
	"fmt"
	"hash/maphash"
	"html"
	"sort"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/orangehill/driftgate/internal/glob"
	"github.com/orangehill/driftgate/internal/jsonx"
	"github.com/orangehill/driftgate/internal/pii"
)

// replaceFn turns one value into its stand-in.
type replaceFn func(string) (string, error)

// needle is a personal value of the same row, looked for in free text: the
// whole value, or one word of it.
type needle struct {
	text string
	fn   replaceFn
	fold bool // matched regardless of case (not for tokens)

	// What matching a folded needle needs, made once for every text of the
	// row (see prepare).
	ready   bool
	folded  string // the text folded (foldString)
	stem    string // see stem
	inflect bool
	phone   bool // digits only, enough for a phone number
}

// prepare makes what matching a folded needle needs.
func (n *needle) prepare() {
	if n.ready {
		return
	}
	n.ready = true
	n.folded = foldString(n.text)
	n.phone = digitsOnly(n.folded) && len(n.folded) >= minLeakDigits
	n.stem, n.inflect = stem(n.folded)
}

// minWord is the shortest word of a personal value that is looked for on
// its own in free text; shorter ones (initials, "Jr") match too much.
const minWord = 3

// words splits a value into its words of at least minWord letters or digits
// with at least one letter, and adds the words written together
// ("AnaPetrović") for a value of two to maxNameWords words.
func words(s string) []string {
	var out []string
	fields := strings.FieldsFunc(s, func(c rune) bool { return !unicode.IsLetter(c) && !unicode.IsDigit(c) })
	for _, w := range fields {
		if utf8.RuneCountInString(w) >= minWord && strings.IndexFunc(w, unicode.IsLetter) >= 0 {
			out = append(out, w)
		}
	}
	if len(fields) > 1 && len(fields) <= maxNameWords {
		if j := strings.Join(fields, ""); utf8.RuneCountInString(j) >= minLeak && strings.IndexFunc(j, unicode.IsLetter) >= 0 {
			out = append(out, j)
		}
	}
	return out
}

// textReplacer replaces the personal parts of free text: what the detector
// finds (e-mail addresses, phone numbers, JMBG, cards, IBANs) and the
// personal values of the same row, as a whole and word by word. Everything
// else stays as it is.
type textReplacer struct {
	r   *Replacer
	pii *pii.Detector

	// record makes freeText find the spans of a text, append them here and
	// replace nothing (engine.findAhead); replay are spans found so, used
	// in the same order by the texts they were found in.
	record *[]foundSpans
	replay []foundSpans
}

// foundSpans are the spans of one text, known by its length and hash
// rather than a copy of it: the texts can be large and many are found
// ahead.
type foundSpans struct {
	n     int
	sum   uint64
	spans []span
}

// spanSeed hashes the texts of foundSpans.
var spanSeed = maphash.MakeSeed()

type span struct {
	start, end int
	fn         replaceFn
	escaped    bool // the text has JSON escapes or HTML character references
}

func isWordRune(c rune) bool { return unicode.IsLetter(c) || unicode.IsDigit(c) }

// boundary reports whether s[start:end] is not part of a longer word.
func boundary(s string, start, end int) bool {
	if start > 0 {
		if c, _ := utf8.DecodeLastRuneInString(s[:start]); isWordRune(c) {
			return false
		}
	}
	if end < len(s) {
		if c, _ := utf8.DecodeRuneInString(s[end:]); isWordRune(c) {
			return false
		}
	}
	return true
}

// minStem is the shortest stem an inflected name is looked for by, and
// endings are the Serbian case endings it may carry in running text:
// "Petroviću" and "Petrovićem" for Petrović, "Jovanom" for Jovana. Other
// suffixes make other words ("Markovićeva ulica") and stay.
const minStem = minLeak - 1

var endings = []string{"a", "e", "i", "o", "u", "om", "em", "ima", "ama"}

func isEnding(s string) bool {
	for _, e := range endings {
		if s == e {
			return true
		}
	}
	return false
}

// stem returns the folded word without its final vowel when an inflected
// form drops it (Jovana, Jovanom), and whether it may be inflected at all:
// letters only, at least minLeak of them.
func stem(folded string) (string, bool) {
	if utf8.RuneCountInString(folded) < minLeak {
		return "", false
	}
	for _, c := range folded {
		if !unicode.IsLetter(c) {
			return "", false
		}
	}
	switch folded[len(folded)-1] {
	case 'a', 'e', 'i', 'o', 'u':
		return folded[:len(folded)-1], true
	}
	return "", true
}

// fchar is one character of a text as needles are matched against it: a
// JSON \u escape or an HTML character reference counts as the character it
// stands for, and every character is folded (see foldRune) into the bytes
// at b[at:at+n] of its foldedText.
type fchar struct {
	start, end int32 // in the text
	at         int32
	r          rune
	n          uint8
	escaped    bool
}

// stop is where the folded bytes of the character end.
func (c fchar) stop() int { return int(c.at) + int(c.n) }

// foldedText is a text folded for matching: needles are found regardless of
// case, diacritics and script (Serbian Cyrillic and Latin), with the offsets
// to replace them in the original. Combining marks are left out.
type foldedText struct {
	b     []byte
	chars []fchar
	of    []int32 // folded byte → its character
	s     string  // b as a string, made once
}

func foldText(s string) *foldedText {
	f := &foldedText{b: make([]byte, 0, len(s)), chars: make([]fchar, 0, len(s)), of: make([]int32, 0, len(s))}
	for i := 0; i < len(s); {
		r, size, esc := decodeChar(s, i)
		if unicode.Is(unicode.Mn, r) {
			i += size
			continue
		}
		at := len(f.b)
		f.b = foldRune(f.b, r)
		for range len(f.b) - at {
			f.of = append(f.of, int32(len(f.chars)))
		}
		f.chars = append(f.chars, fchar{start: int32(i), end: int32(i + size), r: r, at: int32(at), n: uint8(len(f.b) - at), escaped: esc})
		i += size
	}
	return f
}

// decodeChar reads the character at s[i]: a JSON \uXXXX escape (a
// surrogate pair too), an HTML character reference (&#263; &#x107;
// &cacute;) or a UTF-8 character.
func decodeChar(s string, i int) (r rune, size int, escaped bool) {
	switch s[i] {
	case '\\':
		if r, ok := hex4(s, i); ok {
			if utf16.IsSurrogate(r) {
				if r2, ok := hex4(s, i+6); ok {
					if c := utf16.DecodeRune(r, r2); c != utf8.RuneError {
						return c, 12, true
					}
				}
				break
			}
			return r, 6, true
		}
	case '&':
		if end := strings.IndexByte(s[i:min(len(s), i+33)], ';'); end > 1 {
			ref := s[i : i+end+1]
			if u := html.UnescapeString(ref); u != ref {
				if c, n := utf8.DecodeRuneInString(u); n == len(u) && c != utf8.RuneError {
					return c, len(ref), true
				}
			}
		}
	}
	c, n := utf8.DecodeRuneInString(s[i:])
	return c, n, false
}

func hex4(s string, i int) (rune, bool) {
	if i+6 > len(s) || s[i] != '\\' || s[i+1] != 'u' {
		return 0, false
	}
	var r rune
	for _, c := range []byte(s[i+2 : i+6]) {
		d := unhex(c)
		if d < 0 {
			return 0, false
		}
		r = r<<4 | rune(d)
	}
	return r, true
}

func foldString(s string) string {
	var b []byte
	for _, c := range s {
		if !unicode.Is(unicode.Mn, c) {
			b = foldRune(b, c)
		}
	}
	return string(b)
}

// find calls fn with every place the folded pattern matches whole
// characters of f, by the indexes of its first and last character.
func (f *foldedText) find(pattern string, fn func(first, last int)) {
	if pattern == "" {
		return
	}
	hay := f.hay()
	for from := 0; from < len(hay); {
		i := strings.Index(hay[from:], pattern)
		if i < 0 {
			return
		}
		start, end := from+i, from+i+len(pattern)
		from = start + 1
		first, last := int(f.of[start]), int(f.of[end-1])
		if int(f.chars[first].at) != start || f.chars[last].stop() != end {
			continue // inside a character that folds into more letters (đ is dj)
		}
		fn(first, last)
	}
}

func (f *foldedText) hay() string {
	if f.s == "" {
		f.s = string(f.b)
	}
	return f.s
}

func (f *foldedText) word(i int) bool { return i >= 0 && i < len(f.chars) && isWordRune(f.chars[i].r) }

// inflected extends a match that a word goes on after (at last+1) by a
// case ending; ok is false when the word goes on otherwise.
func (f *foldedText) inflected(last int) (int, bool) {
	end := last + 1
	for f.word(end + 1) {
		end++
	}
	if end-last > 3 || !isEnding(string(f.b[f.chars[last+1].at:f.chars[end].stop()])) {
		return last, false
	}
	return end, true
}

// phonePrefix extends a match of a phone number stored without its prefix
// (641234567) over a national or international prefix written before it
// in the text (0641234567, +381641234567, 00381 641234567).
func (f *foldedText) phonePrefix(first int) (int, bool) {
	i := first - 1
	if i >= 0 && f.chars[i].r == ' ' {
		i--
	}
	var digits []byte
	for j := i; f.word(j); j-- {
		digits = append([]byte{byte(f.chars[j].r)}, digits...)
		i = j
	}
	switch string(digits) {
	case "0", "381", "00381":
	default:
		return first, false
	}
	if i > 0 && f.chars[i-1].r == '+' && string(digits) == "381" {
		i--
	}
	if f.word(i - 1) {
		return first, false
	}
	return i, true
}

func digitsOnly(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// spans finds the parts of s to replace, without overlaps: earlier first,
// and of two at the same place the longer.
func (x *textReplacer) spans(s string, needles []needle) []span {
	var all []span
	for _, sp := range x.pii.Spans(s) {
		fn := x.r.Universal
		if sp.Kind == "email" {
			fn = x.r.Email
		}
		all = append(all, span{start: sp.Start, end: sp.End, fn: fn})
	}
	var ft *foldedText
	for i := range needles {
		n := &needles[i]
		if n.text == "" {
			continue
		}
		if !n.fold {
			for from := 0; from < len(s); {
				i := strings.Index(s[from:], n.text)
				if i < 0 {
					break
				}
				start, end := from+i, from+i+len(n.text)
				if boundary(s, start, end) {
					all = append(all, span{start: start, end: end, fn: n.fn})
				}
				from = end
			}
			continue
		}
		if ft == nil {
			ft = foldText(s)
		}
		n.prepare()
		text, phone, st, inflect := n.folded, n.phone, n.stem, n.inflect
		add := func(first, last int) {
			sp := span{start: int(ft.chars[first].start), end: int(ft.chars[last].end), fn: n.fn}
			for _, c := range ft.chars[first : last+1] {
				sp.escaped = sp.escaped || c.escaped
			}
			all = append(all, sp)
		}
		ft.find(text, func(first, last int) {
			if ft.word(first - 1) {
				var ok bool
				if first, ok = ft.phonePrefix(first); !phone || !ok {
					return
				}
			}
			if ft.word(last + 1) {
				var ok bool
				if last, ok = ft.inflected(last); !inflect || !ok {
					return
				}
			}
			add(first, last)
		})
		if st != "" {
			ft.find(st, func(first, last int) {
				if !ft.word(first-1) && ft.word(last+1) {
					if last, ok := ft.inflected(last); ok {
						add(first, last)
					}
				}
			})
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].start != all[j].start {
			return all[i].start < all[j].start
		}
		return all[i].end > all[j].end
	})
	var out []span
	last := 0
	for _, sp := range all {
		if sp.start >= last {
			out = append(out, sp)
			last = sp.end
		}
	}
	return out
}

// freeText replaces the personal parts of s and says how many there were.
func (x *textReplacer) freeText(s string, needles []needle) (string, int, error) {
	var sps []span
	if len(x.replay) > 0 && x.replay[0].n == len(s) && x.replay[0].sum == maphash.String(spanSeed, s) {
		sps, x.replay = x.replay[0].spans, x.replay[1:]
	} else {
		x.replay = nil
		sps = x.spans(s, needles)
	}
	if x.record != nil {
		*x.record = append(*x.record, foundSpans{n: len(s), sum: maphash.String(spanSeed, s), spans: sps})
		return s, len(sps), nil
	}
	if len(sps) == 0 {
		return s, 0, nil
	}
	var b strings.Builder
	b.Grow(len(s))
	last := 0
	for _, sp := range sps {
		b.WriteString(s[last:sp.start])
		out, err := replaceSpan(s[sp.start:sp.end], sp)
		if err != nil {
			return "", 0, err
		}
		b.WriteString(out)
		last = sp.end
	}
	b.WriteString(s[last:])
	return b.String(), len(sps), nil
}

// replaceSpan replaces one part of a text. A part written with JSON
// escapes or HTML character references is replaced as the text they stand
// for, and its stand-in is written back the same way (every character
// outside ASCII escaped), so the text stays valid JSON or HTML.
func replaceSpan(part string, sp span) (string, error) {
	if !sp.escaped {
		return sp.fn(part)
	}
	var plain strings.Builder
	json, ref := false, false
	for i := 0; i < len(part); {
		r, size, esc := decodeChar(part, i)
		if esc {
			json, ref = json || part[i] == '\\', ref || part[i] == '&'
		}
		plain.WriteRune(r)
		i += size
	}
	out, err := sp.fn(plain.String())
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, c := range out {
		switch {
		case json && (c >= utf8.RuneSelf || c == '"' || c == '\\' || c < 0x20):
			for _, u := range utf16.Encode([]rune{c}) {
				fmt.Fprintf(&b, "\\u%04x", u)
			}
		case ref && (c >= utf8.RuneSelf || c == '<' || c == '>' || c == '&' || c == '"' || c == '\''):
			fmt.Fprintf(&b, "&#%d;", c)
		default:
			b.WriteRune(c)
		}
	}
	return b.String(), nil
}

// html replaces personal parts in the text and in attribute values of an
// HTML fragment; tags, attribute names and the markup stay as they are.
func (x *textReplacer) html(s string, needles []needle) (string, int, error) {
	var b strings.Builder
	b.Grow(len(s))
	count := 0
	text := func(t string) error {
		out, n, err := x.freeText(t, needles)
		if err != nil {
			return err
		}
		b.WriteString(out)
		count += n
		return nil
	}
	i := 0
	for i < len(s) {
		lt := strings.IndexByte(s[i:], '<')
		if lt < 0 {
			if err := text(s[i:]); err != nil {
				return "", 0, err
			}
			break
		}
		if err := text(s[i : i+lt]); err != nil {
			return "", 0, err
		}
		i += lt
		if strings.HasPrefix(s[i:], "<!--") {
			end := strings.Index(s[i+4:], "-->")
			if end < 0 {
				b.WriteString("<!--")
				if err := text(s[i+4:]); err != nil {
					return "", 0, err
				}
				break
			}
			b.WriteString("<!--")
			if err := text(s[i+4 : i+4+end]); err != nil {
				return "", 0, err
			}
			b.WriteString("-->")
			i += 4 + end + 3
			continue
		}
		// A '<' that starts no tag ("a < b") is text.
		if i+1 >= len(s) || !(isASCIILetter(s[i+1]) || s[i+1] == '/' || s[i+1] == '!' || s[i+1] == '?') {
			b.WriteByte('<')
			i++
			continue
		}
		j, quote := i+1, byte(0)
		for ; j < len(s); j++ {
			c := s[j]
			if quote != 0 {
				if c == quote {
					quote = 0
				}
				continue
			}
			if c == '"' || c == '\'' {
				quote = c
			} else if c == '>' {
				break
			}
		}
		end := min(j+1, len(s))
		out, n, err := x.attributes(s[i:end], needles)
		if err != nil {
			return "", 0, err
		}
		b.WriteString(out)
		count += n
		i = end
	}
	return b.String(), count, nil
}

func isASCIILetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

// attributes replaces personal parts in the attribute values of one tag.
func (x *textReplacer) attributes(tag string, needles []needle) (string, int, error) {
	var b strings.Builder
	count := 0
	i := 0
	for i < len(tag) {
		eq := strings.IndexByte(tag[i:], '=')
		if eq < 0 {
			b.WriteString(tag[i:])
			break
		}
		j := i + eq + 1
		for j < len(tag) && (tag[j] == ' ' || tag[j] == '\t' || tag[j] == '\n' || tag[j] == '\r') {
			j++
		}
		b.WriteString(tag[i:j])
		if j >= len(tag) {
			break
		}
		var start, end int
		if q := tag[j]; q == '"' || q == '\'' {
			start = j + 1
			e := strings.IndexByte(tag[start:], q)
			if e < 0 {
				end = len(tag)
			} else {
				end = start + e
			}
			b.WriteByte(q)
		} else {
			start = j
			end = start
			for end < len(tag) && tag[end] != ' ' && tag[end] != '>' && tag[end] != '\t' && tag[end] != '\n' && !(tag[end] == '/' && end+1 < len(tag) && tag[end+1] == '>') {
				end++
			}
		}
		out, n, err := x.freeText(tag[start:end], needles)
		if err != nil {
			return "", 0, err
		}
		b.WriteString(out)
		count += n
		i = end
	}
	return b.String(), count, nil
}

// jsonRule applies per-path strategies to a JSON value: a path glob ($.phone,
// $.contacts[*].email, $..) picks a node, and every string and number under it
// gets the path's strategy. Strings under no path get the other strategy
// (free text by default, or keep). Key order, numbers and layout stay.
type jsonRule struct {
	paths []string // longest first, then by name: the most specific path decides
	strat map[string]string
	other string
}

func newJSONRule(c *ColumnRule) *jsonRule {
	j := &jsonRule{strat: c.Paths, other: c.Other}
	if j.other == "" {
		j.other = StrategyFreeText
	}
	for p := range c.Paths {
		j.paths = append(j.paths, p)
	}
	sort.Slice(j.paths, func(a, b int) bool {
		if len(j.paths[a]) != len(j.paths[b]) {
			return len(j.paths[a]) > len(j.paths[b])
		}
		return j.paths[a] < j.paths[b]
	})
	return j
}

func (j *jsonRule) match(path string) (string, bool) {
	for _, p := range j.paths {
		if glob.Match(p, path) {
			return j.strat[p], true
		}
	}
	return "", false
}

// jsonValue holds the replacement of one JSON document.
type jsonValue struct {
	x       *textReplacer
	rule    *jsonRule
	needles []needle
	parts   int
	changed bool
}

// apply replaces the personal parts of a JSON document; text that is not
// JSON is treated as free text (or kept, with other: keep).
func (x *textReplacer) json(raw string, rule *jsonRule, needles []needle) (out string, nulled bool, parts int, err error) {
	v, perr := jsonx.Parse([]byte(raw))
	if perr != nil {
		if rule.other == KindKeep {
			return raw, false, 0, nil
		}
		out, n, err := x.freeText(raw, needles)
		return out, false, n, err
	}
	jv := &jsonValue{x: x, rule: rule, needles: needles}
	root, err := jv.walk("$", v, "")
	if err != nil {
		return "", false, 0, err
	}
	if root == nil {
		return "", true, 1, nil
	}
	if !jv.changed {
		return raw, false, 0, nil
	}
	spaced := false
	for run := range jsonx.Layout([]byte(raw)) {
		if strings.Contains(run, " ") {
			spaced = true
		}
	}
	return string(jsonx.Marshal(root, spaced)), false, jv.parts, nil
}

// walk returns the node to keep in place of v, or nil for a JSON null that
// replaces it at the top. strat is the strategy of the nearest matched
// ancestor ("" for none). Under a matched path the keys of objects are
// replaced like the values (an object of e-mail addresses), numbers too
// unless the strategy is keep (empty makes them 0); a null or empty
// strategy on an object or an array replaces all of it.
func (jv *jsonValue) walk(path string, v *jsonx.Value, strat string) (*jsonx.Value, error) {
	if s, ok := jv.rule.match(path); ok {
		strat = s
	}
	switch v.Kind {
	case jsonx.Object, jsonx.Array:
		switch strat {
		case StrategyNull:
			jv.changed, jv.parts = true, jv.parts+1
			return &jsonx.Value{Kind: jsonx.Null}, nil
		case StrategyEmpty:
			if len(v.Members)+len(v.Items) > 0 {
				jv.changed, jv.parts = true, jv.parts+1
			}
			return &jsonx.Value{Kind: v.Kind}, nil
		}
	}
	switch v.Kind {
	case jsonx.Object:
		var from map[string]string // stand-in key → original key
		for i, m := range v.Members {
			k, n, err := jv.key(m.Key, strat)
			if err != nil {
				return nil, err
			}
			if n > 0 {
				v.Members[i].Key = k
				jv.parts += n
				jv.changed = true
			}
			if from == nil {
				from = map[string]string{}
			}
			if o, ok := from[k]; ok && o != m.Key {
				return nil, fmt.Errorf("two keys of %s have the same stand-in", path)
			}
			from[k] = m.Key
			nv, err := jv.walk(jsonx.KeyPath(path, m.Key), m.Value, strat)
			if err != nil {
				return nil, err
			}
			v.Members[i].Value = nv
		}
		return v, nil
	case jsonx.Array:
		for i, item := range v.Items {
			nv, err := jv.walk(path+"[*]", item, strat)
			if err != nil {
				return nil, err
			}
			v.Items[i] = nv
		}
		return v, nil
	case jsonx.String:
		return jv.leaf(v, strat)
	case jsonx.Int, jsonx.Float:
		switch {
		case strat == StrategyNull:
			jv.changed, jv.parts = true, jv.parts+1
			return &jsonx.Value{Kind: jsonx.Null}, nil
		case strat == StrategyEmpty:
			if v.Text != "0" {
				jv.changed, jv.parts = true, jv.parts+1
			}
			return &jsonx.Value{Kind: jsonx.Int, Text: "0"}, nil
		case strat == KindKeep, strat == "" && (jv.rule.other != StrategyFreeText || !personalNumber(path, v.Text)):
			return v, nil
		}
		out, err := jv.x.r.Number(v.Text)
		if err != nil {
			return nil, err
		}
		if out != v.Text {
			jv.changed, jv.parts = true, jv.parts+1
		}
		return &jsonx.Value{Kind: v.Kind, Text: out}, nil
	}
	return v, nil
}

// key replaces an object key by the strategy of its object: free text
// under no path, the path's strategy under one.
func (jv *jsonValue) key(k, strat string) (string, int, error) {
	var out string
	var err error
	switch strat {
	case "":
		if jv.rule.other != StrategyFreeText {
			return k, 0, nil
		}
		return jv.x.freeText(k, jv.needles)
	case KindKeep, StrategyNull, StrategyEmpty:
		return k, 0, nil
	case StrategyFreeText, StrategyHTML:
		return jv.x.freeText(k, jv.needles)
	case StrategyPassword:
		out, err = jv.x.r.Token(k)
	default:
		out, err = jv.x.r.byName(strat)(k)
	}
	if err != nil || out == k {
		return k, 0, err
	}
	return out, 1, nil
}

// personalNumber reports a JSON number that its key names as a personal
// identifier (phone, jmbg, card_number… as data classify reads column
// names) with at least minLeakDigits digits. Other numbers are amounts,
// counters, IDs and times; they stay unless a path names them.
func personalNumber(path, number string) bool {
	digits := 0
	for i := 0; i < len(number); i++ {
		if number[i] >= '0' && number[i] <= '9' {
			digits++
		}
	}
	if digits < minLeakDigits {
		return false
	}
	key := strings.TrimSuffix(path, "[*]")
	for strings.HasSuffix(key, "[*]") {
		key = strings.TrimSuffix(key, "[*]")
	}
	if i := strings.LastIndexAny(key, ".["); i >= 0 {
		key = key[i+1:]
	}
	key = strings.Trim(key, `"']`)
	return hint(key, phoneWords) || hint(key, idWords)
}

func (jv *jsonValue) leaf(v *jsonx.Value, strat string) (*jsonx.Value, error) {
	s := v.Text
	var out string
	var err error
	n := 1
	switch strat {
	case "":
		if jv.rule.other != StrategyFreeText {
			return v, nil
		}
		out, n, err = jv.x.freeText(s, jv.needles)
	case KindKeep:
		return v, nil
	case StrategyNull:
		jv.changed, jv.parts = true, jv.parts+1
		return &jsonx.Value{Kind: jsonx.Null}, nil
	case StrategyEmpty:
		out = ""
	case StrategyFreeText:
		out, n, err = jv.x.freeText(s, jv.needles)
	case StrategyHTML:
		out, n, err = jv.x.html(s, jv.needles)
	case StrategyPassword:
		out, _, err = jv.x.r.Password(s)
	default:
		out, err = jv.x.r.byName(strat)(s)
	}
	if err != nil {
		return nil, err
	}
	if out == s {
		return v, nil
	}
	jv.changed, jv.parts = true, jv.parts+n
	return &jsonx.Value{Kind: jsonx.String, Text: out}, nil
}
