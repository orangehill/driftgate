package data

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orangehill/driftgate/internal/dbx"
)

// BenchmarkData measures anonymize, subset and verify on a generated source
// of DRIFTGATE_BENCH_DATA_USERS users (the seed of the anonymize test, with
// free text, HTML and JSON) and as many posts and orders, on the servers of
// DRIFTGATE_TEST_MYSQL and DRIFTGATE_TEST_POSTGRES:
//
//	DRIFTGATE_BENCH_DATA_USERS=1000000 go test -run '^$' -bench Data -benchtime 1x ./internal/data/
//
// The source is seeded once and kept (dgbench_src_<users>), so later runs
// measure only the copy; -bench Data/<host>/<mode> runs one mode. With
// DRIFTGATE_BENCH_DATA_OUT each run writes its dump, a digest of the target
// database's content and the verify report there, to compare two builds
// byte for byte.
func BenchmarkData(b *testing.B) {
	users, _ := strconv.Atoi(os.Getenv("DRIFTGATE_BENCH_DATA_USERS"))
	if users <= 0 {
		b.Skip("DRIFTGATE_BENCH_DATA_USERS not set")
	}
	var servers []*server
	if v := os.Getenv("DRIFTGATE_TEST_MYSQL"); v != "" {
		for _, dsn := range strings.Split(v, ",") {
			servers = append(servers, newServer(b, MySQL, dsn))
		}
	}
	if dsn := os.Getenv("DRIFTGATE_TEST_POSTGRES"); dsn != "" {
		servers = append(servers, newServer(b, Postgres, dsn))
	}
	for _, s := range servers {
		u, _ := url.Parse(s.dsn)
		b.Run(u.Host, func(b *testing.B) { benchData(b, s, users) })
	}
}

const benchRules = `  orders:
    columns: { id: keep, user_id: keep, total: keep, note: free-text, created_at: keep }
`

const benchSubset = `subset:
  tables:
    posts: { percent: 10 }
    orders: { percent: 10 }
    post_log: { follow: [posts] }
    tags: all
  references:
    - { from: "post_log(post_id)", to: "posts(id)" }
`

func benchData(b *testing.B, s *server, users int) {
	ctx := context.Background()
	src, dst := fmt.Sprintf("dgbench_src_%d", users), fmt.Sprintf("dgbench_dst_%d", users)
	benchSeed(b, s, src, users)
	b.Cleanup(func() { s.drop(dst) })
	rulesText := testRules + benchRules
	if s.driver == Postgres {
		rulesText = testRulesPostgres + benchRules
	}
	rules, err := ParseRules([]byte(rulesText))
	if err != nil {
		b.Fatal(err)
	}
	subRules, err := ParseRules([]byte(rulesText + benchSubset))
	if err != nil {
		b.Fatal(err)
	}
	u, _ := url.Parse(s.dsn)
	out := os.Getenv("DRIFTGATE_BENCH_DATA_OUT")
	save := func(name string, data []byte) {
		if out == "" {
			return
		}
		if err := os.WriteFile(filepath.Join(out, strings.ReplaceAll(u.Host, ":", "_")+"-"+name), data, 0o600); err != nil {
			b.Fatal(err)
		}
	}
	target := s.db(dst)
	// anonymize runs one copy with a dump to a file and reports rows/s and
	// the peak heap.
	anonymize := func(b *testing.B, name string, opt Options, verify func(context.Context, Options, string, *Database) error) {
		var rows int64
		var elapsed time.Duration
		var peak uint64
		for range b.N {
			b.StopTimer()
			dir := out
			if dir == "" {
				dir = b.TempDir()
			}
			f, err := os.Create(filepath.Join(dir, strings.ReplaceAll(u.Host, ":", "_")+"-"+name+".sql"))
			if err != nil {
				b.Fatal(err)
			}
			opt.Source, opt.Key, opt.Dump = s.db(src), testKey, f
			if testing.Verbose() {
				opt.Log = os.Stderr
			}
			if verify != nil {
				o := opt
				opt.Verify = func(ctx context.Context, staging *Database) error { return verify(ctx, o, f.Name(), staging) }
			}
			heap := watchHeap()
			start := time.Now()
			b.StartTimer()
			rep, err := Anonymize(ctx, opt)
			if err != nil {
				b.Fatal(err)
			}
			b.StopTimer()
			elapsed += time.Since(start)
			peak = max(peak, heap())
			rows += rep.Rows
			f.Close()
			if out == "" {
				os.Remove(f.Name())
			} else if opt.Target != nil {
				save(name+".content", []byte(contentDigest(b, target)))
			}
		}
		b.ReportMetric(float64(rows)/elapsed.Seconds(), "rows/s")
		b.ReportMetric(float64(peak)/(1<<20), "peak-heap-MiB")
	}
	b.Run("copy", func(b *testing.B) {
		anonymize(b, "copy", Options{Target: &target, Replace: true, Rules: rules}, nil)
	})
	b.Run("dump", func(b *testing.B) {
		anonymize(b, "dump", Options{Rules: rules}, nil)
	})
	b.Run("subset", func(b *testing.B) {
		anonymize(b, "subset", Options{Target: &target, Replace: true, Rules: subRules, Filter: &Subset{}}, nil)
	})
	// verified is the copy with data verify on its output before it is made,
	// as data anonymize runs it.
	b.Run("verified", func(b *testing.B) {
		var rep *VerifyReport
		anonymize(b, "verified", Options{Target: &target, Replace: true, Rules: rules, Index: &VerifyIndex{}}, func(ctx context.Context, opt Options, dump string, staging *Database) error {
			var err error
			rep, err = benchVerify(ctx, opt, dump, staging)
			return err
		})
		saveVerify(b, save, "verified.json", rep)
	})
	// verify is data verify on its own, over the target the copy wrote.
	b.Run("verify", func(b *testing.B) {
		b.StopTimer()
		if _, err := Anonymize(ctx, Options{Source: s.db(src), Target: &target, Replace: true, Rules: rules, Key: testKey}); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		var rep *VerifyReport
		peak := watchHeap()
		start := time.Now()
		for range b.N {
			var err error
			if rep, err = Verify(ctx, VerifyOptions{Source: s.db(src), Target: &target, Rules: rules, Key: testKey}); err != nil {
				b.Fatal(err)
			}
		}
		b.ReportMetric(float64(rep.Values)*float64(b.N)/time.Since(start).Seconds(), "values/s")
		b.ReportMetric(float64(peak())/(1<<20), "peak-heap-MiB")
		saveVerify(b, save, "verify.json", rep)
	})
	if rss := peakRSS(); rss > 0 {
		b.Logf("peak RSS of the process: %d MiB", rss>>20)
	}
}

func saveVerify(b *testing.B, save func(string, []byte), name string, rep *VerifyReport) {
	if rep == nil {
		return
	}
	r := *rep
	r.Seconds = 0
	j, err := json.MarshalIndent(r, "", " ")
	if err != nil {
		b.Fatal(err)
	}
	save(name, j)
}

// watchHeap samples the heap in use until the returned function reports
// the peak.
func watchHeap() func() uint64 {
	var mu sync.Mutex
	var peak uint64
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var m runtime.MemStats
		for {
			runtime.ReadMemStats(&m)
			mu.Lock()
			peak = max(peak, m.HeapInuse)
			mu.Unlock()
			select {
			case <-done:
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}()
	return func() uint64 {
		close(done)
		wg.Wait()
		return peak
	}
}

// benchSeed makes the source unless it is there from an earlier run.
func benchSeed(b *testing.B, s *server, src string, users int) {
	if d, err := open(s.db(src), src); err == nil {
		var n int
		err := d.QueryRow("SELECT COUNT(*) FROM orders").Scan(&n)
		d.Close()
		if err == nil && n == users {
			return
		}
	}
	start := time.Now()
	mariadb := false
	if s.driver == MySQL {
		var version string
		s.admin.QueryRow("SELECT VERSION()").Scan(&version)
		mariadb = dbx.IsMariaDB(version)
	}
	s.create(b, src)
	first := min(users, 2000)
	s.exec(b, src, seedStatements(s.driver, people(first), invoiceSequence(s, mariadb))...)
	for from := first + 1; from <= users; from += 2000 {
		var ps []person
		for i := from; i < min(from+2000, users+1); i++ {
			ps = append(ps, newPerson(i))
		}
		s.exec(b, src, seedRows(s.driver, ps)...)
	}
	// An order of each post, with a note that names the buyer's address.
	if s.driver == MySQL {
		s.exec(b, src, "CREATE TABLE orders (id INT PRIMARY KEY, user_id INT NOT NULL, total DECIMAL(10,2) NOT NULL, note VARCHAR(200), created_at DATETIME NOT NULL, FOREIGN KEY (user_id) REFERENCES users(id))",
			"INSERT INTO orders SELECT p.id, p.user_id, (p.id % 10000) / 100, CONCAT('Dostava na ', u.email, ' ili pozvati ', u.phone), '2025-06-01 00:00:00' FROM posts p JOIN users u ON u.id = p.user_id")
	} else {
		s.exec(b, src, fmt.Sprintf("SELECT setval('users_id_seq', %d)", users),
			"CREATE TABLE orders (id INT PRIMARY KEY, user_id INT NOT NULL REFERENCES users(id), total NUMERIC(10,2) NOT NULL, note VARCHAR(200), created_at TIMESTAMP NOT NULL)",
			"INSERT INTO orders SELECT p.id, p.user_id, (p.id % 10000) / 100.0, 'Dostava na ' || u.email || ' ili pozvati ' || u.phone, TIMESTAMP '2025-06-01' FROM posts p JOIN users u ON u.id = p.user_id",
			"ANALYZE")
	}
	b.Logf("seeded %s with %d users in %s", src, users, time.Since(start).Round(time.Second))
}

// contentDigest is a digest of every row of every table, in key order.
func contentDigest(b *testing.B, d Database) string {
	ctx := context.Background()
	dl, _ := dialectFor(d.Driver)
	db, err := open(d, d.Name)
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	c, err := db.Conn(ctx)
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	if err := dl.session(ctx, c); err != nil {
		b.Fatal(err)
	}
	s, err := dl.schema(ctx, c, d.Name)
	if err != nil {
		b.Fatal(err)
	}
	h := sha256.New()
	var report bytes.Buffer
	for _, t := range s.Tables {
		rows, err := c.QueryContext(ctx, dl.selectRows(t, t.Columns, ""))
		if err != nil {
			b.Fatal(err)
		}
		th := sha256.New()
		raw := make([]any, len(t.Columns))
		vals := make([]*[]byte, len(t.Columns))
		for i := range raw {
			vals[i] = new([]byte)
			raw[i] = vals[i]
		}
		n := 0
		for rows.Next() {
			if err := rows.Scan(raw...); err != nil {
				b.Fatal(err)
			}
			for _, v := range vals {
				if *v == nil {
					th.Write([]byte{0})
				} else {
					th.Write([]byte{1})
					fmt.Fprintf(th, "%d:", len(*v))
					th.Write(*v)
				}
			}
			n++
		}
		if err := rows.Err(); err != nil {
			b.Fatal(err)
		}
		rows.Close()
		fmt.Fprintf(&report, "%s %d %x\n", t.Name, n, th.Sum(nil))
	}
	io.Copy(h, bytes.NewReader(report.Bytes()))
	return report.String() + "all " + hex.EncodeToString(h.Sum(nil)) + "\n"
}

// benchVerify is data verify on an output before it is made, as the
// command runs it.
func benchVerify(ctx context.Context, opt Options, dump string, staging *Database) (*VerifyReport, error) {
	f, err := os.Open(dump)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rep, err := Verify(ctx, VerifyOptions{Source: opt.Source, Target: staging, Dump: f, Rules: opt.Rules, Key: opt.Key, Index: opt.Index})
	if err != nil {
		return nil, err
	}
	for i := range rep.Findings {
		if rep.Findings[i].Output == "database "+staging.Name {
			rep.Findings[i].Output = "database " + opt.Target.Name
		}
	}
	return rep, nil
}
