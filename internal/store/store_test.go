package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oryanm/stalker/internal/model"
)

// t0 is the fixed "now" of every test store.
var t0 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func openAt(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	s.now = func() time.Time { return t0 }
	return s
}

func openStore(t *testing.T) *Store {
	t.Helper()
	return openAt(t, filepath.Join(t.TempDir(), "data", "stalker.db"))
}

func mustCreate(t *testing.T, s *Store, f model.Follow) model.Follow {
	t.Helper()
	if err := s.CreateFollow(t.Context(), &f); err != nil {
		t.Fatalf("CreateFollow(%q): %v", f.FeedURL, err)
	}
	return f
}

func mustGet(t *testing.T, s *Store, id int64) model.Follow {
	t.Helper()
	f, err := s.GetFollow(t.Context(), id)
	if err != nil {
		t.Fatalf("GetFollow(%d): %v", id, err)
	}
	return f
}

func pragma(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var v string
	if err := db.QueryRowContext(t.Context(), "PRAGMA "+name).Scan(&v); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return v
}

func TestOpenAppliesPragmas(t *testing.T) {
	s := openStore(t)
	want := map[string]string{"journal_mode": "wal", "foreign_keys": "1", "busy_timeout": "5000", "synchronous": "1"}
	for name, db := range map[string]*sql.DB{"writer": s.w, "reader": s.r} {
		for p, v := range want {
			if got := pragma(t, db, p); got != v {
				t.Errorf("%s: PRAGMA %s = %q, want %q", name, p, got, v)
			}
		}
	}
	if got := pragma(t, s.r, "query_only"); got != "1" {
		t.Errorf("reader query_only = %q, want 1", got)
	}
	if got := pragma(t, s.w, "query_only"); got != "0" {
		t.Errorf("writer query_only = %q, want 0", got)
	}
	if _, err := s.r.ExecContext(t.Context(), `INSERT INTO settings (key, value) VALUES ('k', 'v')`); err == nil {
		t.Error("write through the reader pool succeeded, want an error")
	}
}

func TestOpenMigrationsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "stalker.db")
	ms, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	wantVersion := fmt.Sprint(len(ms))

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return t0 }
	f := mustCreate(t, s, model.Follow{FeedURL: "https://example.com/feed", Tags: []string{"a"}})
	if got := pragma(t, s.w, "user_version"); got != wantVersion {
		t.Errorf("user_version = %s, want %s", got, wantVersion)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		s := openAt(t, path)
		if got := pragma(t, s.w, "user_version"); got != wantVersion {
			t.Errorf("user_version after reopen = %s, want %s", got, wantVersion)
		}
		if got := mustGet(t, s, f.ID); !reflect.DeepEqual(got, f) {
			t.Errorf("follow after reopen = %+v, want %+v", got, f)
		}
	}
}

func TestOpenConcurrentFirstOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stalker.db")
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			s, err := Open(path)
			if err != nil {
				t.Errorf("Open: %v", err)
				return
			}
			if err := s.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		})
	}
	wg.Wait()
	s := openAt(t, path)
	if _, err := s.ListFollows(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRejectsNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stalker.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.w.ExecContext(t.Context(), "PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("Open succeeded on a newer schema, want an error")
	} else if !strings.Contains(err.Error(), "newer") {
		t.Errorf("Open error = %v, want it to mention the newer schema", err)
	}
}

func TestOpenPathWithURICharacters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "odd dir?x=1#frag%20", "stalker.db")
	s := openAt(t, path)
	mustCreate(t, s, model.Follow{FeedURL: "https://example.com/feed"})
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database not created at the literal path: %v", err)
	}
}

func TestOpenEmptyPath(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("Open(\"\") succeeded, want an error")
	}
}

func TestInTxPanicReleasesWriter(t *testing.T) {
	s := openStore(t)
	func() {
		defer func() { _ = recover() }()
		_ = s.inTx(t.Context(), func(tx *sql.Tx) error {
			if _, err := tx.Exec(`INSERT INTO settings (key, value) VALUES ('k', 'panicked')`); err != nil {
				t.Fatal(err)
			}
			panic("boom")
		})
	}()

	// a leaked transaction would hold the only writer connection and block forever
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := s.SetSetting(ctx, "other", "v"); err != nil {
		t.Fatalf("SetSetting after panic: %v", err)
	}
	if got, err := s.GetSetting(ctx, "k", "default"); err != nil || got != "default" {
		t.Errorf("GetSetting(k) = %q, %v; want the panicking write rolled back", got, err)
	}
}

func TestSettings(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	// steps run in order against one store
	steps := []struct {
		desc       string
		set        bool
		key, value string
		def, want  string
	}{
		{desc: "missing returns default", key: "sort", def: "recent", want: "recent"},
		{desc: "set", set: true, key: "sort", value: "az", def: "recent", want: "az"},
		{desc: "overwrite", set: true, key: "sort", value: "followed", def: "recent", want: "followed"},
		{desc: "empty value is not the default", set: true, key: "empty", value: "", def: "x", want: ""},
		{desc: "other key unaffected", key: "unset", def: "d", want: "d"},
	}
	for _, st := range steps {
		if st.set {
			if err := s.SetSetting(ctx, st.key, st.value); err != nil {
				t.Fatalf("%s: SetSetting: %v", st.desc, err)
			}
		}
		got, err := s.GetSetting(ctx, st.key, st.def)
		if err != nil {
			t.Fatalf("%s: GetSetting: %v", st.desc, err)
		}
		if got != st.want {
			t.Errorf("%s: GetSetting(%q) = %q, want %q", st.desc, st.key, got, st.want)
		}
	}
}

// TestConcurrentAccess mimics the poller writing while the UI reads; any
// SQLITE_BUSY or race surfaces as an error.
func TestConcurrentAccess(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	const follows, fetches, reads = 6, 15, 30
	ids := make([]int64, follows)
	for i := range follows {
		ids[i] = mustCreate(t, s, model.Follow{FeedURL: fmt.Sprintf("https://example.com/%d.xml", i)}).ID
	}

	var wg sync.WaitGroup
	for w, id := range ids {
		wg.Go(func() {
			for n := range fetches {
				at := t0.Add(time.Duration(n) * time.Minute)
				r := FetchResult{FollowID: id, FetchedAt: at, NextFetchAt: at.Add(time.Hour), ETag: fmt.Sprint(n)}
				if n%5 == 4 {
					r.Err = "HTTP 500"
				}
				for p := range 20 {
					r.Posts = append(r.Posts, model.Post{
						GUID: fmt.Sprintf("%d-%d", w, n+p), Title: "post", PublishedAt: at.Add(-time.Duration(p) * time.Hour),
					})
				}
				if err := s.RecordFetch(ctx, r); err != nil {
					t.Errorf("RecordFetch: %v", err)
					return
				}
			}
		})
	}
	wg.Go(func() {
		for n := range fetches {
			f, err := s.GetFollow(ctx, ids[0])
			if err != nil {
				t.Errorf("GetFollow: %v", err)
				return
			}
			f.Tags = []string{fmt.Sprint("tag", n%3)}
			if err := s.UpdateFollowSettings(ctx, f); err != nil {
				t.Errorf("UpdateFollowSettings: %v", err)
			}
			if err := s.SetSetting(ctx, "sort", fmt.Sprint(n)); err != nil {
				t.Errorf("SetSetting: %v", err)
			}
		}
	})
	for range 4 {
		wg.Go(func() {
			for n := range reads {
				var err error
				switch n % 4 {
				case 0:
					_, err = s.ListFollows(ctx)
				case 1:
					_, err = s.LatestPosts(ctx, 3)
				case 2:
					_, err = s.Activity(ctx, t0.Add(-60*24*time.Hour), nil)
				case 3:
					_, err = s.DueFollows(ctx, t0.Add(24*time.Hour), 10)
				}
				if err != nil {
					t.Errorf("read %d: %v", n%4, err)
					return
				}
			}
		})
	}
	wg.Wait()

	latest, err := s.LatestPosts(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if len(latest[id]) != 3 {
			t.Errorf("follow %d has %d latest posts, want 3", id, len(latest[id]))
		}
	}
}
