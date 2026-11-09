package dbx

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// GaleraNode reports whether the session runs on a Galera cluster node
// with wsrep on: MariaDB Galera or Percona XtraDB Cluster.
func GaleraNode(ctx context.Context, q Querier) (bool, error) {
	var name, value string
	err := q.QueryRowContext(ctx, "SHOW SESSION VARIABLES LIKE 'wsrep_on'").Scan(&name, &value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil && strings.EqualFold(value, "ON"), err
}

// GaleraRefusedSequences names the sequences of db a Galera node refuses
// to create (1235), each with why: one with a cache and an increment other
// than 0, or one not stored by InnoDB. They are read in one query.
func GaleraRefusedSequences(ctx context.Context, q Querier, db string, seqs []string) ([]string, error) {
	if len(seqs) == 0 {
		return nil, nil
	}
	parts := make([]string, len(seqs))
	var args []any
	for i, s := range seqs {
		parts[i] = "SELECT ?, increment, cache_size, (SELECT ENGINE FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?) FROM " + QuoteMySQL(db) + "." + QuoteMySQL(s)
		args = append(args, s, db, s)
	}
	rows, err := q.QueryContext(ctx, strings.Join(parts, " UNION ALL "), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refused []string
	for rows.Next() {
		var s string
		var increment int64
		var cache uint64
		var engine sql.NullString
		if err := rows.Scan(&s, &increment, &cache, &engine); err != nil {
			return nil, err
		}
		switch {
		case !strings.EqualFold(engine.String, "InnoDB"):
			refused = append(refused, fmt.Sprintf("%s (ENGINE=%s)", s, engine.String))
		case cache != 0 && increment != 0:
			refused = append(refused, fmt.Sprintf("%s (CACHE %d, INCREMENT BY %d)", s, cache, increment))
		}
	}
	return refused, rows.Err()
}

// GaleraSequencesHint is what to do about a sequence GaleraRefusedSequences
// names.
const GaleraSequencesHint = "make them NOCACHE or INCREMENT BY 0 (ALTER SEQUENCE) and InnoDB (ALTER TABLE … ENGINE=InnoDB)"
