package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
)

// A table's row hash is kept when the pages its rows were read from are
// what they were. Each page is hashed on its own (sqlitePageHashes), and a
// table's digest is the hash of its pages' numbers and hashes in the order
// dbstat walked its b-tree (root, inner, leaf and overflow pages).
//
// Why the same pages mean the same rows: SQLite reads a table by walking
// its b-tree from the root page sqlite_schema names. What it finds on a
// page (its kind, its cells, the pages its cells point to: children,
// overflow chains, each chain page naming the next) is all in the page's
// bytes, read the way the file's header says (page size, reserved bytes,
// text encoding, schema format). So when the root page is the same one,
// the header says the same, and every page the walk visited last time
// holds the same bytes now, the walk visits exactly those pages again and
// finds the same rows: a page that joined the b-tree since must be pointed
// to from a page of it, which would then have changed. No other page is
// read for the rows: not the freelist (a page that left the b-tree, freed
// or given to another table, left a changed page behind), not the pointer
// map of an auto_vacuum file (it only says where a page is pointed from,
// for moving pages), not an index. The rest of what a table's item hashes
// (its CREATE statement, which also gives the default of a column added
// later, its indexes, its sqlite_sequence value) is compared apart.

// sqliteHeader is what of the file's header the pages are read by: the
// page size, the reserved bytes at the end of each page and the payload
// fractions, the schema format and the text encoding (bytes 16–17, 20–23,
// 44–47 and 56–59). Neither the change counter nor the read and write
// versions (which only a change of journal mode moves) are part of it.
func sqliteHeader(ctx context.Context, q queryer, schema string) (string, error) {
	var h string
	err := q.QueryRowContext(ctx, "SELECT hex(substr(data, 17, 2)) || hex(substr(data, 21, 4)) || hex(substr(data, 45, 4)) || hex(substr(data, 57, 4)) FROM sqlite_dbpage("+ql(schema)+") WHERE pgno = 1").Scan(&h)
	return h, err
}

// sqlitePageHashes are the hashes of pages read at one moment: of every
// page of the file (pgnos nil: sums[i] is page i+1's), or of the pages
// listed in pgnos (sorted, sums[i] is pgnos[i]'s).
type sqlitePageHashes struct {
	pgnos []uint32
	sums  [][32]byte
}

func (h *sqlitePageHashes) get(pgno uint32) ([32]byte, bool) {
	if h == nil {
		return [32]byte{}, false
	}
	if h.pgnos == nil {
		if pgno == 0 || int(pgno) > len(h.sums) {
			return [32]byte{}, false
		}
		return h.sums[pgno-1], true
	}
	i, ok := slices.BinarySearch(h.pgnos, pgno)
	if !ok {
		return [32]byte{}, false
	}
	return h.sums[i], true
}

// digest is the digest of a table whose b-tree consists of pages, from
// their hashes; ok is false when one of them was not read (it is no longer
// in the file).
func (h *sqlitePageHashes) digest(pages []uint32) ([32]byte, bool) {
	sums := make([][32]byte, len(pages))
	for i, p := range pages {
		var ok bool
		if sums[i], ok = h.get(p); !ok {
			return [32]byte{}, false
		}
	}
	return sqliteDigest(pages, sums), true
}

// sqliteDigest is the digest of a table whose b-tree consists of pages,
// whose hashes are sums.
func sqliteDigest(pages []uint32, sums [][32]byte) [32]byte {
	dh := sha256.New()
	var b [4 + 32]byte
	for i, p := range pages {
		binary.BigEndian.PutUint32(b[:4], p)
		copy(b[4:], sums[i][:])
		dh.Write(b[:])
	}
	var d [32]byte
	dh.Sum(d[:0])
	return d
}

// readPages hashes pages of the file at the moment conns share: every page
// (want nil), or the pages in want (sorted, without repeats). The pages are
// read in order, spread over conns by page number, each connection hashing
// what it reads. (SQLite reads a page at a time; the reads hardly overlap,
// the hashing does.)
func readPages(ctx context.Context, conns []*sql.Conn, schema string, want []uint32) (*sqlitePageHashes, error) {
	var count int64
	if err := conns[0].QueryRowContext(ctx, "PRAGMA "+qs(schema)+".page_count").Scan(&count); err != nil {
		return nil, err
	}
	n := int(count)
	if want != nil {
		// A page beyond the end of the file is not read: a table that
		// had it changed.
		end, _ := slices.BinarySearch(want, uint32(min(count, math.MaxUint32))+1)
		want = want[:end]
		n = len(want)
	}
	h := &sqlitePageHashes{pgnos: want, sums: make([][32]byte, n)}
	readers := min(len(conns), max(1, n/256))
	errs := make([]error, readers)
	var wg sync.WaitGroup
	for r := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[r] = h.read(ctx, conns[r], schema, n*r/readers, n*(r+1)/readers)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return h, nil
}

// read hashes the pages lo to hi-1 (of the file, or of pgnos) on c.
func (h *sqlitePageHashes) read(ctx context.Context, c *sql.Conn, schema string, lo, hi int) error {
	var rows *sql.Rows
	var err error
	if h.pgnos == nil {
		rows, err = c.QueryContext(ctx, "SELECT pgno, data FROM sqlite_dbpage("+ql(schema)+") WHERE pgno BETWEEN ? AND ?", lo+1, hi)
	} else {
		list, _ := json.Marshal(h.pgnos[lo:hi])
		rows, err = c.QueryContext(ctx, "SELECT p.pgno, p.data FROM json_each(?) AS j CROSS JOIN sqlite_dbpage("+ql(schema)+") AS p ON p.pgno = j.value", string(list))
	}
	if err != nil {
		return err
	}
	defer rows.Close()
	at := lo
	for rows.Next() {
		var pgno int64
		var data sql.RawBytes
		if err := rows.Scan(&pgno, &data); err != nil {
			return err
		}
		want := int64(at + 1)
		if h.pgnos != nil && at < hi {
			want = int64(h.pgnos[at])
		}
		if at >= hi || pgno != want {
			return fmt.Errorf("sqlite_dbpage: page %d where page %d was expected", pgno, want)
		}
		h.sums[at] = sha256.Sum256(data)
		at++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if at != hi {
		return fmt.Errorf("sqlite_dbpage: %d pages read, %d expected", at-lo, hi-lo)
	}
	return nil
}

// sqliteWalk lists the pages of a table's b-tree in the order dbstat walks
// them, with their digest: from hashes when they hold every page of the
// file, else read with the pages.
func sqliteWalk(ctx context.Context, c *sql.Conn, schema, table string, hashes *sqlitePageHashes) ([]uint32, [32]byte, error) {
	var pages []uint32
	var sums [][32]byte
	if hashes != nil && hashes.pgnos == nil {
		rows, err := c.QueryContext(ctx, "SELECT pageno FROM dbstat("+ql(schema)+") WHERE name = ?", table)
		if err != nil {
			return nil, [32]byte{}, err
		}
		defer rows.Close()
		for rows.Next() {
			var pgno int64
			if err := rows.Scan(&pgno); err != nil {
				return nil, [32]byte{}, err
			}
			pages = append(pages, uint32(pgno))
		}
		if err := rows.Err(); err != nil {
			return nil, [32]byte{}, err
		}
		d, ok := hashes.digest(pages)
		if !ok {
			return nil, [32]byte{}, fmt.Errorf("dbstat: a page of %s is beyond the end of the file", table)
		}
		return pages, d, nil
	}
	rows, err := c.QueryContext(ctx, "SELECT s.pageno, p.data FROM dbstat("+ql(schema)+") AS s JOIN sqlite_dbpage("+ql(schema)+") AS p ON p.pgno = s.pageno WHERE s.name = ?", table)
	if err != nil {
		return nil, [32]byte{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var pgno int64
		var data sql.RawBytes
		if err := rows.Scan(&pgno, &data); err != nil {
			return nil, [32]byte{}, err
		}
		pages = append(pages, uint32(pgno))
		sums = append(sums, sha256.Sum256(data))
	}
	if err := rows.Err(); err != nil {
		return nil, [32]byte{}, err
	}
	return pages, sqliteDigest(pages, sums), nil
}
