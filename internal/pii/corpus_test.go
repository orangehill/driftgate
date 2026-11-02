package pii

import (
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"
)

// corpus builds a JSON body of n records shaped like a typical API response:
// ids, amounts, times, slugs, free text and nested lists. With personal set,
// every tenth record also holds an e-mail address, a phone number and a JMBG.
func corpus(n int, personal bool) []byte {
	rnd := rand.New(rand.NewPCG(1, 2))
	words := strings.Fields("news announcement school city sport session board member Niš Čačak Novi Sad café naïve " +
		"program support companies contest deadline application year market price service contract tender project")
	text := func(k int) string {
		var b strings.Builder
		for i := 0; i < k; i++ {
			if i > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(words[rnd.IntN(len(words))])
			if rnd.IntN(12) == 0 {
				fmt.Fprintf(&b, " %d", rnd.IntN(2030))
			}
		}
		return b.String()
	}
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	var b strings.Builder
	b.WriteString(`{"data":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		at := t0.Add(time.Duration(rnd.IntN(3*365*24*3600)) * time.Second)
		fmt.Fprintf(&b, `{"id":%d,"uuid":"%08x-%04x-4%03x-a%03x-%012x","slug":"vest-%d-%s","title":%q,`,
			100000+i, rnd.Uint32(), rnd.IntN(1<<16), rnd.IntN(1<<12), rnd.IntN(1<<12), rnd.Uint64()&(1<<48-1),
			at.Year(), strings.ToLower(words[rnd.IntN(len(words))]), text(6))
		fmt.Fprintf(&b, `"price":"%d.%02d","amount":%d.%02d,"cents":%d,"created_at":"%s","ts":%d,"order":"ORD-%s",`,
			rnd.IntN(100000), rnd.IntN(100), rnd.IntN(1000000), rnd.IntN(100), rnd.IntN(100000000),
			at.Format("2006-01-02 15:04:05"), at.UnixMilli(), strconv.Itoa(1e9+rnd.IntN(1e9)))
		fmt.Fprintf(&b, `"body":%q,"tags":["%s","%s"],"stats":{"views":%d,"likes":%d,"ratio":%.4f},`,
			"<p>"+text(40)+"</p>", words[rnd.IntN(len(words))], words[rnd.IntN(len(words))],
			rnd.IntN(100000), rnd.IntN(1000), rnd.Float64())
		if personal && i%10 == 0 {
			fmt.Fprintf(&b, `"contact":{"email":"user%d@example.com","phone":"+381 64 %03d %04d","jmbg":"0101990710008"},`,
				i, rnd.IntN(1000), rnd.IntN(10000))
		}
		fmt.Fprintf(&b, `"active":%t}`, rnd.IntN(2) == 0)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

// page is an HTML page of about n paragraphs with entities, CSS and SVG
// numbers: a body that is not JSON.
func page(n int) []byte {
	rnd := rand.New(rand.NewPCG(3, 4))
	var b strings.Builder
	b.WriteString(`<html><head><style>:root{--a:oklch(98.4% .003 247.858)}</style></head><body>`)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, `<p class="lead">News &amp; announcements from Niš, issue %d &ndash; price %d,%02d RSD</p>`, i, rnd.IntN(100000), rnd.IntN(100))
		fmt.Fprintf(&b, `<svg><path d="M%d.%d %d %d"/></svg>`, rnd.IntN(400), rnd.IntN(1000), rnd.IntN(400), rnd.IntN(400))
	}
	b.WriteString(`</body></html>`)
	return []byte(b.String())
}

// stamps builds a JSON body of n records that are mostly UUIDs and ISO times,
// as a Laravel or Rails API returns them: the texts the phone pattern used to
// spend most of its time on.
func stamps(n int) []byte {
	rnd := rand.New(rand.NewPCG(9, 10))
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	var b strings.Builder
	b.WriteString(`{"data":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		at := t0.Add(time.Duration(rnd.Int64N(3 * 365 * 24 * 3600 * 1e9)))
		fmt.Fprintf(&b, `{"id":"%08x-%04x-4%03x-a%03x-%012x","user_id":"%08x-%04x-4%03x-b%03x-%012x",`,
			rnd.Uint32(), rnd.IntN(1<<16), rnd.IntN(1<<12), rnd.IntN(1<<12), rnd.Uint64()&(1<<48-1),
			rnd.Uint32(), rnd.IntN(1<<16), rnd.IntN(1<<12), rnd.IntN(1<<12), rnd.Uint64()&(1<<48-1))
		fmt.Fprintf(&b, `"status":"active","created_at":"%s","updated_at":"%s","published_at":"%s","date":"%s"}`,
			at.Format("2006-01-02T15:04:05.000000Z"), at.Add(time.Hour).Format(time.RFC3339),
			at.In(time.FixedZone("", 3600)).Format(time.RFC3339Nano), at.Format("2006-01-02 15:04:05"))
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}
