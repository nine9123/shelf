package main

import (
	"database/sql"
	"log"

	_ "github.com/mattn/go-sqlite3"
)

// openIndexDB opens the disposable cache. Delete this file and it rebuilds on
// the next scan. It holds nothing that isn't derived from the archives.
func openIndexDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", "file:"+path+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		return nil, err
	}
	// 1. Base tables. Fresh databases get every column; older ones are left as
	//    they are here and patched by the column migrations below.
	base := `
CREATE TABLE IF NOT EXISTS archives (
    id          INTEGER PRIMARY KEY,
    path        TEXT UNIQUE NOT NULL,          -- relative to root; where the file currently is
    identity    TEXT NOT NULL DEFAULT '',      -- durable identity ("#<id>" or "@<path>")
    title       TEXT NOT NULL DEFAULT '',
    size        INTEGER NOT NULL DEFAULT 0,
    mtime       INTEGER NOT NULL DEFAULT 0,    -- unix; cheap change-detection fingerprint
    page_count  INTEGER NOT NULL DEFAULT 0,
    meta        TEXT NOT NULL DEFAULT '',      -- raw .txt contents from inside the archive
    category    TEXT NOT NULL DEFAULT '',      -- parsed from info.txt "Category:"
    has_cover   INTEGER NOT NULL DEFAULT 0,
    status      TEXT NOT NULL DEFAULT 'pending', -- pending | indexed | error
    error       TEXT NOT NULL DEFAULT '',
    indexed_at  INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS tags (
    archive_id INTEGER NOT NULL,
    namespace  TEXT NOT NULL,
    tag        TEXT NOT NULL
);`
	if _, err := db.Exec(base); err != nil {
		db.Close()
		return nil, err
	}
	// 2. Add columns that older caches may lack. ALTER errors harmlessly when the
	//    column already exists; when it actually adds one, re-index to backfill.
	//    (Indexes on these columns come afterwards, so they never reference a
	//    column that doesn't exist yet.)
	for _, m := range []struct{ name, ddl string }{
		{"identity", `ALTER TABLE archives ADD COLUMN identity TEXT NOT NULL DEFAULT ''`},
		{"category", `ALTER TABLE archives ADD COLUMN category TEXT NOT NULL DEFAULT ''`},
	} {
		if _, err := db.Exec(m.ddl); err == nil {
			db.Exec(`UPDATE archives SET status='pending' WHERE status='indexed'`)
			log.Printf("index.db: added %s column; will re-index to backfill on next scan", m.name)
		}
	}
	// 3. Indexes last, once every column they reference is guaranteed to exist.
	idx := `
CREATE INDEX IF NOT EXISTS idx_archives_status   ON archives(status);
CREATE INDEX IF NOT EXISTS idx_archives_title    ON archives(title);
CREATE INDEX IF NOT EXISTS idx_archives_identity ON archives(identity);
CREATE INDEX IF NOT EXISTS idx_archives_category ON archives(category);
CREATE INDEX IF NOT EXISTS idx_tags_lookup       ON tags(namespace, tag);
CREATE INDEX IF NOT EXISTS idx_tags_archive      ON tags(archive_id);`
	if _, err := db.Exec(idx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// openUserDB opens the durable database. It is keyed by the archive's identity
// (see identityOf), so favorites and progress survive rebuilding the index
// cache and follow a file across moves, renames, and re-encoding. This is the
// place to add features that hold real user state.
func openUserDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", "file:"+path+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		return nil, err
	}
	// Rekey a pre-identity user.db: the old tables were keyed by path, which is
	// incompatible with identity keys, so rebuild them. Only favorites/progress
	// are affected and they're quick to redo.
	for _, t := range []string{"favorites", "read_progress"} {
		if userDBHasColumn(db, t, "archive_path") {
			db.Exec("DROP TABLE " + t)
			log.Printf("user.db: rekeying %s to numeric identity (previous entries cleared)", t)
		}
	}
	schema := `
CREATE TABLE IF NOT EXISTS favorites (
    identity   TEXT PRIMARY KEY,
    created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS read_progress (
    identity   TEXT PRIMARY KEY,
    last_page  INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func userDBHasColumn(db *sql.DB, table, col string) bool {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk) == nil && name == col {
			return true
		}
	}
	return false
}
