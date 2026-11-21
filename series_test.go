package main

import (
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestSeriesOf(t *testing.T) {
	cases := []struct {
		title, key string
		vol        int
		ok         bool
	}{
		{"0042 Foo Vol. 3", "foo", 3, true},
		{"Foo v12", "foo", 12, true},
		{"Foo Volume 2", "foo", 2, true},
		{"Foo #7", "foo", 7, true},
		{"Foo - 01", "foo", 1, true},
		{"Foo 2 [English]", "foo [english]", 2, true},
		{"Foo  Vol 2 (2019)", "foo (2019)", 2, true},
		{`Foo "Bar" 4`, "foo bar", 4, true},
		{"01 Some Series Name", "some series name", 1, true}, // the leading number is the volume
		{"12 Some Series Name", "some series name", 12, true},
		{"0042 01 Some Series Name", "some series name", 1, true}, // ID, then volume
		{"0042 02 - Some Series Name", "some series name", 2, true},
		{"0042 Vol. 3 Some Series Name", "some series name", 3, true},
		{"01 Foo Vol. 2", "foo", 2, true}, // an explicit trailing volume wins
		{"-7 Foo", "", 0, false},          // a negative ID isn't a volume
		{"-7", "", 0, false},
		{"Foo [1080p]", "", 0, false}, // numbers in brackets aren't volumes
		{"12", "", 0, false},          // nothing but a number
		{"Plain Title", "", 0, false}, // no number at all
	}
	for _, c := range cases {
		key, _, vol, ok := seriesOf(c.title)
		if key != c.key || vol != c.vol || ok != c.ok {
			t.Errorf("seriesOf(%q) = %q %d %v, want %q %d %v", c.title, key, vol, ok, c.key, c.vol, c.ok)
		}
	}
	if _, name, _, _ := seriesOf("0042 Foo Vol. 3"); name != "Foo" {
		t.Errorf("name = %q, want Foo", name)
	}
	if _, name, _, _ := seriesOf("01 Some  Series Name"); name != "Some Series Name" {
		t.Errorf("name = %q, want Some Series Name", name)
	}
}

func TestListGroupsVolumes(t *testing.T) {
	dir := t.TempDir()
	idx, err := openIndexDB(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	user, err := openUserDB(filepath.Join(dir, "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer user.Close()
	a := &App{cfg: Config{CoversDir: dir}, index: idx, user: user}
	for _, rel := range []string{"1 Foo Vol. 1.cbz", "2 Foo Vol. 10.cbz", "3 Foo Vol. 2.cbz", "4 Bar.cbz", "5 Baz 1.cbz"} {
		a.writeResult(result{rel: rel, title: titleFromRel(rel), pageCount: 1})
	}

	list := func(q url.Values) []archiveItem {
		t.Helper()
		rec := httptest.NewRecorder()
		a.handleList(rec, httptest.NewRequest("GET", "/api/archives?"+q.Encode(), nil))
		var out struct{ Items []archiveItem }
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: %v", rec.Body.String(), err)
		}
		return out.Items
	}

	got := list(url.Values{"group": {"1"}})
	if len(got) != 3 {
		t.Fatalf("grouped list has %d items, want 3: %+v", len(got), got)
	}
	var foo *archiveItem
	for i := range got {
		if got[i].SeriesKey == "foo" {
			foo = &got[i]
		} else if got[i].Volumes != 0 {
			t.Errorf("%q should not be a group: %+v", got[i].Title, got[i])
		}
	}
	if foo == nil || foo.Volumes != 3 || foo.Title != "2 Foo Vol. 10" || foo.SeriesName != "Foo" {
		t.Fatalf("group = %+v, want 3 volumes represented by Vol. 10", foo)
	}

	if got := list(url.Values{}); len(got) != 5 {
		t.Fatalf("ungrouped list has %d items, want 5", len(got))
	}

	// Opening the group lists its volumes in volume order, ungrouped.
	got = list(url.Values{"group": {"1"}, "q": {`volumes:"foo"`}})
	if len(got) != 3 || got[0].Title != "1 Foo Vol. 1" || got[1].Title != "3 Foo Vol. 2" || got[2].Title != "2 Foo Vol. 10" {
		t.Fatalf("series list = %+v", got)
	}
}

func TestListGroupsLeadingVolumeNumbers(t *testing.T) {
	dir := t.TempDir()
	idx, err := openIndexDB(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	user, err := openUserDB(filepath.Join(dir, "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer user.Close()
	a := &App{cfg: Config{CoversDir: dir}, index: idx, user: user}
	for _, rel := range []string{"Series/01 Some Series Name.cbz", "Series/02 Some Series Name.cbz", "Series/10 Some Series Name.cbz", "Other/03 Other Name.cbz"} {
		a.writeResult(result{rel: rel, title: titleFromRel(rel), pageCount: 1})
	}
	rec := httptest.NewRecorder()
	a.handleList(rec, httptest.NewRequest("GET", "/api/archives?group=1", nil))
	var out struct{ Items []archiveItem }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s: %v", rec.Body.String(), err)
	}
	if len(out.Items) != 2 {
		t.Fatalf("grouped list has %d items, want 2: %+v", len(out.Items), out.Items)
	}
	var grp *archiveItem
	for i := range out.Items {
		if out.Items[i].Volumes > 0 {
			grp = &out.Items[i]
		}
	}
	if grp == nil || grp.Volumes != 3 || grp.Title != "10 Some Series Name" || grp.SeriesName != "Some Series Name" {
		t.Fatalf("group = %+v, want 3 volumes represented by 10", grp)
	}
}

func TestOpenIndexDBBackfillsSeries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	// A cache from before volume grouping: no series columns.
	old, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE archives (id INTEGER PRIMARY KEY, path TEXT UNIQUE NOT NULL,
		identity TEXT NOT NULL DEFAULT '', title TEXT NOT NULL DEFAULT '', size INTEGER NOT NULL DEFAULT 0,
		mtime INTEGER NOT NULL DEFAULT 0, page_count INTEGER NOT NULL DEFAULT 0, meta TEXT NOT NULL DEFAULT '',
		category TEXT NOT NULL DEFAULT '', has_cover INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL DEFAULT 'pending', error TEXT NOT NULL DEFAULT '', indexed_at INTEGER NOT NULL DEFAULT 0);
		INSERT INTO archives (path, title, status) VALUES ('7 Foo v4.cbz', '7 Foo v4', 'indexed');`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	idx, err := openIndexDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	var key, status string
	var vol int
	idx.QueryRow(`SELECT series_key, volume, status FROM archives`).Scan(&key, &vol, &status)
	if key != "foo" || vol != 4 || status != "indexed" {
		t.Fatalf("backfill = %q %d %q, want foo 4 indexed (no re-index)", key, vol, status)
	}
}

func TestOpenIndexDBRecomputesStaleSeries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	idx, err := openIndexDB(path)
	if err != nil {
		t.Fatal(err)
	}
	// A row written by older rules, in a cache that predates the rules version.
	if _, err := idx.Exec(`INSERT INTO archives (path, title, status, series_key, volume) VALUES ('01 Foo.cbz', '01 Foo', 'indexed', '', 0);
		PRAGMA user_version = 0`); err != nil {
		t.Fatal(err)
	}
	idx.Close()

	idx, err = openIndexDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	var key string
	var vol int
	idx.QueryRow(`SELECT series_key, volume FROM archives`).Scan(&key, &vol)
	if key != "foo" || vol != 1 {
		t.Fatalf("series = %q %d, want foo 1", key, vol)
	}
}
