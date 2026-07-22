package main

import (
	"database/sql"

	_ "github.com/mattn/go-sqlite3"
)

// openIndexDB opens the disposable cache. Delete this file and it rebuilds on
// the next scan. It holds nothing that isn't derived from the archives.
func openIndexDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", "file:"+path+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		return nil, err
	}
	schema := `
CREATE TABLE IF NOT EXISTS archives (
    id          INTEGER PRIMARY KEY,
    path        TEXT UNIQUE NOT NULL,          -- relative to root; the stable identity
    title       TEXT NOT NULL DEFAULT '',
    size        INTEGER NOT NULL DEFAULT 0,
    mtime       INTEGER NOT NULL DEFAULT 0,    -- unix; cheap change-detection fingerprint
    page_count  INTEGER NOT NULL DEFAULT 0,
    meta        TEXT NOT NULL DEFAULT '',      -- raw .txt contents from inside the archive
    has_cover   INTEGER NOT NULL DEFAULT 0,
    status      TEXT NOT NULL DEFAULT 'pending', -- pending | indexed | error
    error       TEXT NOT NULL DEFAULT '',
    indexed_at  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_archives_status ON archives(status);
CREATE INDEX IF NOT EXISTS idx_archives_title  ON archives(title);`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// openUserDB opens the durable database. It is keyed by archive path (the
// stable identity), so it survives deleting and rebuilding the index cache.
// This is the place to add features that hold real user state.
func openUserDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", "file:"+path+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		return nil, err
	}
	schema := `
CREATE TABLE IF NOT EXISTS favorites (
    archive_path TEXT PRIMARY KEY,
    created_at   INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS read_progress (
    archive_path TEXT PRIMARY KEY,
    last_page    INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
