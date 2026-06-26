package storage

import (
	"database/sql"
	"fmt"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib" // pure-Go Postgres driver, registered as "pgx"
)

// postgresKV is the optional shared backend. All namespaces live in a single
// table keyed by (ns, key); the schema is created on first open. It is only
// reached when [storage] backend = "postgres" — never by default.
type postgresKV struct {
	db *sql.DB
}

// OpenPostgres connects to dsn and ensures the kv table exists.
func OpenPostgres(dsn string) (KV, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: open postgres: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("storage: ping postgres: %w", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS lilmail_kv (
		ns  TEXT  NOT NULL,
		key TEXT  NOT NULL,
		val BYTEA NOT NULL,
		PRIMARY KEY (ns, key)
	)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("storage: init schema: %w", err)
	}
	return &postgresKV{db: db}, nil
}

func (p *postgresKV) Get(ns, key string) ([]byte, error) {
	var val []byte
	err := p.db.QueryRow(`SELECT val FROM lilmail_kv WHERE ns=$1 AND key=$2`, ns, key).Scan(&val)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return val, nil
}

func (p *postgresKV) Set(ns, key string, val []byte) error {
	_, err := p.db.Exec(
		`INSERT INTO lilmail_kv (ns, key, val) VALUES ($1, $2, $3)
		 ON CONFLICT (ns, key) DO UPDATE SET val = EXCLUDED.val`,
		ns, key, val,
	)
	return err
}

func (p *postgresKV) Delete(ns, key string) error {
	_, err := p.db.Exec(`DELETE FROM lilmail_kv WHERE ns=$1 AND key=$2`, ns, key)
	return err
}

func (p *postgresKV) List(ns, prefix string) (map[string][]byte, error) {
	out := make(map[string][]byte)

	var rows *sql.Rows
	var err error
	if prefix == "" {
		rows, err = p.db.Query(`SELECT key, val FROM lilmail_kv WHERE ns=$1`, ns)
	} else {
		// Escape LIKE wildcards so a literal prefix is matched verbatim.
		esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix)
		rows, err = p.db.Query(`SELECT key, val FROM lilmail_kv WHERE ns=$1 AND key LIKE $2 ESCAPE '\'`, ns, esc+"%")
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var k string
		var v []byte
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func (p *postgresKV) Close() error { return p.db.Close() }
