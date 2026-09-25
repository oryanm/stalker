package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oryanm/stalker/internal/events"
	"github.com/oryanm/stalker/internal/feed"
	"github.com/oryanm/stalker/internal/model"
	"github.com/oryanm/stalker/internal/opml"
	"github.com/oryanm/stalker/internal/store"
	"github.com/oryanm/stalker/internal/web"
)

const fraidycatOPML = "../../testdata/fraidycat-sample.opml"

// lockedBuffer is written by server goroutines and read by the test.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type testApp struct {
	*app
	stdout, stderr *lockedBuffer
}

func newApp(t *testing.T, env map[string]string) testApp {
	t.Helper()
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	stdout, stderr := &lockedBuffer{}, &lockedBuffer{}
	return testApp{
		app: &app{
			stdin:       strings.NewReader(""),
			stdout:      stdout,
			stderr:      stderr,
			getenv:      func(k string) string { return env[k] },
			feedOptions: feed.Options{HostSpacing: -1, AllowPrivateNetworks: true},
		},
		stdout: stdout,
		stderr: stderr,
	}
}

func (a testApp) run(t *testing.T, args ...string) int {
	t.Helper()
	return a.app.run(t.Context(), args)
}

func TestParse(t *testing.T) {
	fullEnv := map[string]string{
		"STALKER_ADDR":          ":9000",
		"STALKER_DB":            "/data/env.db",
		"STALKER_USERNAME":      "me",
		"STALKER_PASSWORD":      "secret",
		"STALKER_NO_AUTH":       "1",
		"STALKER_ALLOWED_HOSTS": "nas.lan,stalker.home",
		"STALKER_ALLOW_PRIVATE": "true",
		"STALKER_LOG_LEVEL":     "debug",
	}
	defaults := settings{
		Addr: defaultAddr, DB: defaultDB, Username: defaultUsername,
		Concurrency: defaultConcurrency, LogLevel: slog.LevelError,
	}
	with := func(f func(*settings)) settings {
		s := defaults
		f(&s)
		return s
	}
	tests := []struct {
		name    string
		cmd     string
		args    []string
		env     map[string]string
		want    settings
		wantErr error  // matched with errors.Is
		errText string // matched as a substring when wantErr is nil
	}{
		{name: "serve defaults", cmd: "serve", want: with(func(s *settings) { s.LogLevel = slog.LevelInfo })},
		{
			name: "serve from environment", cmd: "serve", env: fullEnv,
			want: settings{
				Addr: ":9000", DB: "/data/env.db", Username: "me", Password: "secret", NoAuth: true,
				AllowedHosts: []string{"nas.lan", "stalker.home"}, AllowPrivate: true,
				LogLevel: slog.LevelDebug, Concurrency: defaultConcurrency,
			},
		},
		{
			name: "flags beat environment", cmd: "serve", args: []string{"-addr", "127.0.0.1:7000", "-db", "flag.db"}, env: fullEnv,
			want: settings{
				Addr: "127.0.0.1:7000", DB: "flag.db", Username: "me", Password: "secret", NoAuth: true,
				AllowedHosts: []string{"nas.lan", "stalker.home"}, AllowPrivate: true,
				LogLevel: slog.LevelDebug, Concurrency: defaultConcurrency,
			},
		},
		{
			name: "empty variables fall back to defaults", cmd: "serve",
			env:  map[string]string{"STALKER_ADDR": "", "STALKER_USERNAME": "", "STALKER_NO_AUTH": ""},
			want: with(func(s *settings) { s.LogLevel = slog.LevelInfo }),
		},
		{
			name: "no auth accepts booleans", cmd: "serve", env: map[string]string{"STALKER_NO_AUTH": "true"},
			want: with(func(s *settings) { s.NoAuth, s.LogLevel = true, slog.LevelInfo }),
		},
		{
			name: "no auth off", cmd: "serve", env: map[string]string{"STALKER_NO_AUTH": "0"},
			want: with(func(s *settings) { s.LogLevel = slog.LevelInfo }),
		},
		{name: "bad no auth", cmd: "serve", env: map[string]string{"STALKER_NO_AUTH": "yes please"}, errText: "STALKER_NO_AUTH"},
		{name: "bad allow private", cmd: "check", env: map[string]string{"STALKER_ALLOW_PRIVATE": "sure"}, errText: "STALKER_ALLOW_PRIVATE"},
		{
			name: "log level is case-insensitive", cmd: "export", env: map[string]string{"STALKER_LOG_LEVEL": "WARN"},
			want: with(func(s *settings) { s.LogLevel = slog.LevelWarn }),
		},
		{name: "bad log level", cmd: "export", env: map[string]string{"STALKER_LOG_LEVEL": "loud"}, errText: "STALKER_LOG_LEVEL"},
		{
			name: "import takes a file", cmd: "import", args: []string{"-db", "x.db", "follows.opml"},
			want: with(func(s *settings) { s.DB, s.Args = "x.db", []string{"follows.opml"} }),
		},
		{
			name: "import from stdin", cmd: "import", args: []string{"-"},
			want: with(func(s *settings) { s.Args = []string{"-"} }),
		},
		{name: "import without file", cmd: "import", wantErr: errUsage},
		{name: "import with two files", cmd: "import", args: []string{"a.opml", "b.opml"}, wantErr: errUsage},
		{name: "flag after argument", cmd: "import", args: []string{"a.opml", "-db", "x.db"}, wantErr: errUsage},
		{name: "export", cmd: "export", env: map[string]string{"STALKER_DB": "/d.db"}, want: with(func(s *settings) { s.DB = "/d.db" })},
		{name: "export takes no argument", cmd: "export", args: []string{"out.opml"}, wantErr: errUsage},
		{name: "check concurrency", cmd: "check", args: []string{"-concurrency", "3"}, want: with(func(s *settings) { s.Concurrency = 3 })},
		{name: "check zero concurrency", cmd: "check", args: []string{"-concurrency", "0"}, wantErr: errUsage},
		{name: "check bad concurrency", cmd: "check", args: []string{"-concurrency", "many"}, wantErr: errUsage},
		{
			name: "discover", cmd: "discover", args: []string{"example.com"},
			want: with(func(s *settings) { s.Args = []string{"example.com"} }),
		},
		{name: "discover has no db flag", cmd: "discover", args: []string{"-db", "x.db", "example.com"}, wantErr: errUsage},
		{name: "unknown flag", cmd: "serve", args: []string{"-port", "80"}, wantErr: errUsage},
		{name: "help flag", cmd: "check", args: []string{"-h"}, wantErr: flag.ErrHelp},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newApp(t, tt.env)
			got, err := a.parse(tt.cmd, tt.args)
			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("parse error = %v, want %v", err, tt.wantErr)
				}
				return
			case tt.errText != "":
				if err == nil || !strings.Contains(err.Error(), tt.errText) {
					t.Fatalf("parse error = %v, want one mentioning %s", err, tt.errText)
				}
				return
			case err != nil:
				t.Fatalf("parse: %v", err)
			}
			if len(got.Args) == 0 {
				got.Args = nil
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parse =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

func TestRunExitCodes(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		env        map[string]string
		want       int
		wantStdout string
		wantStderr string
	}{
		{name: "no command", want: exitUsage, wantStderr: "Usage:"},
		{name: "help", args: []string{"help"}, want: exitOK, wantStdout: "STALKER_PASSWORD"},
		{name: "-h", args: []string{"-h"}, want: exitOK, wantStdout: "Usage:"},
		{name: "--help", args: []string{"--help"}, want: exitOK, wantStdout: "Usage:"},
		{name: "help for a command", args: []string{"help", "check"}, want: exitOK, wantStderr: "Usage: stalker check [-db path] [-concurrency n]"},
		{name: "help for an unknown command", args: []string{"help", "nope"}, want: exitOK, wantStdout: "Usage:"},
		{name: "command -h", args: []string{"serve", "-h"}, want: exitOK, wantStderr: "listen address (env STALKER_ADDR)"},
		{name: "unknown command", args: []string{"frobnicate"}, want: exitUsage, wantStderr: `unknown command "frobnicate"`},
		{name: "missing argument", args: []string{"discover"}, want: exitUsage, wantStderr: "stalker discover: missing argument"},
		{name: "unexpected argument", args: []string{"export", "x"}, want: exitUsage, wantStderr: `unexpected argument "x"`},
		{name: "bad flag", args: []string{"serve", "-bogus"}, want: exitUsage, wantStderr: "flag provided but not defined: -bogus"},
		{name: "bad environment", args: []string{"export"}, env: map[string]string{"STALKER_LOG_LEVEL": "loud"}, want: exitFail, wantStderr: "stalker export: STALKER_LOG_LEVEL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newApp(t, tt.env)
			if got := a.run(t, tt.args...); got != tt.want {
				t.Errorf("exit code = %d, want %d\nstderr: %s", got, tt.want, a.stderr)
			}
			if !strings.Contains(a.stdout.String(), tt.wantStdout) {
				t.Errorf("stdout = %q, want it to contain %q", a.stdout, tt.wantStdout)
			}
			if !strings.Contains(a.stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", a.stderr, tt.wantStderr)
			}
		})
	}
}

func TestServeRequiresPassword(t *testing.T) {
	db := filepath.Join(t.TempDir(), "stalker.db")
	a := newApp(t, map[string]string{"STALKER_DB": db, "STALKER_NO_AUTH": "0"})
	a.newHandler = func(*store.Store, web.Discoverer, web.Fetcher, *events.Broker, web.Config) (http.Handler, error) {
		t.Error("serve built a handler without a password")
		return http.NotFoundHandler(), nil
	}
	if code := a.run(t, "serve"); code != exitFail {
		t.Errorf("exit code = %d, want %d", code, exitFail)
	}
	if !strings.Contains(a.stderr.String(), "STALKER_PASSWORD is not set") {
		t.Errorf("stderr = %q", a.stderr)
	}
	if _, err := os.Stat(db); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a database was created: %v", err)
	}
}

func TestServe(t *testing.T) {
	feedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, rss("Served", 2))
	}))
	defer feedSrv.Close()

	db := filepath.Join(t.TempDir(), "stalker.db")
	followID := seed(t, db, feedSrv.URL+"/feed")

	a := newApp(t, map[string]string{
		"STALKER_DB": db, "STALKER_ADDR": "127.0.0.1:0", "STALKER_PASSWORD": "pw", "STALKER_USERNAME": "me",
		"STALKER_ALLOWED_HOSTS": " nas.lan, ,stalker.home ",
	})
	var cfg web.Config
	var updates <-chan events.Event
	a.newHandler = func(_ *store.Store, _ web.Discoverer, _ web.Fetcher, ev *events.Broker, c web.Config) (http.Handler, error) {
		cfg = c
		updates, _ = ev.Subscribe()
		mux := http.NewServeMux()
		mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") })
		mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		})
		return mux, nil
	}
	addrs := make(chan net.Addr, 1)
	a.listening = func(addr net.Addr) { addrs <- addr }

	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	exited := make(chan int, 1)
	go func() { exited <- a.app.run(ctx, []string{"serve"}) }()

	var base string
	select {
	case addr := <-addrs:
		base = "http://" + addr.String()
	case code := <-exited:
		t.Fatalf("serve exited with %d: %s", code, a.stderr)
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not start listening")
	}
	if want := (web.Config{Username: "me", Password: "pw", AllowedHosts: []string{"nas.lan", "stalker.home"}}); !reflect.DeepEqual(cfg, want) {
		t.Errorf("web config = %+v, want %+v", cfg, want)
	}

	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Errorf("GET /healthz = %d %q", resp.StatusCode, body)
	}

	// an open SSE stream must not hold up shutdown
	stream, err := http.Get(base + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()

	timeout := time.After(10 * time.Second)
	for fetched := false; !fetched; {
		select {
		case e := <-updates:
			fetched = e.Kind == events.FollowUpdated && e.FollowID == followID
		case <-timeout:
			t.Fatal("the poller never fetched the follow")
		}
	}

	stop()
	select {
	case code := <-exited:
		if code != exitOK {
			t.Errorf("exit code = %d, want 0\nstderr: %s", code, a.stderr)
		}
	case <-time.After(shutdownTimeout - time.Second):
		t.Fatal("serve did not shut down in time")
	}
	if _, err := io.ReadAll(stream.Body); err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Logf("SSE stream ended with %v", err)
	}
	for _, want := range []string{"stalker listening", "shutting down"} {
		if !strings.Contains(a.stderr.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, a.stderr)
		}
	}

	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if f, err := st.GetFollow(context.Background(), followID); err != nil || f.FeedTitle != "Served" {
		t.Errorf("follow after serve = %+v, %v; want the fetch recorded", f, err)
	}
}

func TestServeListenError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	a := newApp(t, map[string]string{
		"STALKER_DB": filepath.Join(t.TempDir(), "stalker.db"), "STALKER_ADDR": ln.Addr().String(), "STALKER_NO_AUTH": "1",
	})
	a.newHandler = func(*store.Store, web.Discoverer, web.Fetcher, *events.Broker, web.Config) (http.Handler, error) {
		return http.NotFoundHandler(), nil
	}
	if code := a.run(t, "serve"); code != exitFail {
		t.Errorf("exit code = %d, want %d", code, exitFail)
	}
	if !strings.Contains(a.stderr.String(), "listen") {
		t.Errorf("stderr = %q, want the listen error", a.stderr)
	}
}

func TestServeHandlerError(t *testing.T) {
	a := newApp(t, map[string]string{"STALKER_DB": filepath.Join(t.TempDir(), "stalker.db"), "STALKER_PASSWORD": "pw"})
	a.newHandler = func(*store.Store, web.Discoverer, web.Fetcher, *events.Broker, web.Config) (http.Handler, error) {
		return nil, errors.New("templates broken")
	}
	if code := a.run(t, "serve"); code != exitFail || !strings.Contains(a.stderr.String(), "templates broken") {
		t.Errorf("exit code %d, stderr %q", code, a.stderr)
	}
}

func TestImportAndExport(t *testing.T) {
	db := filepath.Join(t.TempDir(), "stalker.db")
	env := map[string]string{"STALKER_DB": db}

	for _, want := range []string{"added 14, skipped 0\n", "added 0, skipped 14\n"} {
		a := newApp(t, env)
		if code := a.run(t, "import", fraidycatOPML); code != exitOK {
			t.Fatalf("import exit code %d: %s", code, a.stderr)
		}
		if got := a.stdout.String(); got != want {
			t.Errorf("import printed %q, want %q", got, want)
		}
	}

	a := newApp(t, env)
	a.stdin = strings.NewReader(`<opml version="2.0"><body><outline text="New" xmlUrl="https://new.example/feed" category="importance/0,news"/></body></opml>`)
	if code := a.run(t, "import", "-"); code != exitOK || a.stdout.String() != "added 1, skipped 0\n" {
		t.Fatalf("import from stdin: exit %d, stdout %q, stderr %q", code, a.stdout, a.stderr)
	}

	a = newApp(t, env)
	if code := a.run(t, "export"); code != exitOK {
		t.Fatalf("export exit code %d: %s", code, a.stderr)
	}
	out := a.stdout.String()
	if !strings.Contains(out, "<title>Stalker Follows</title>") {
		t.Errorf("export lacks the title:\n%.300s", out)
	}
	entries, err := opml.Parse(strings.NewReader(out))
	if err != nil {
		t.Fatalf("export is not valid OPML: %v", err)
	}
	if len(entries) != 15 {
		t.Errorf("export has %d follows, want 15", len(entries))
	}
	var found bool
	for _, e := range entries {
		if e.FeedURL == "https://new.example/feed" {
			found = true
			if e.Importance != model.Realtime || !reflect.DeepEqual(e.Tags, []string{"news"}) {
				t.Errorf("round-tripped follow = %+v", e)
			}
		}
	}
	if !found {
		t.Error("the follow imported from stdin is missing from the export")
	}
}

func TestImportErrors(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.opml")
	if err := os.WriteFile(bad, []byte("this is not OPML"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, file, wantStderr string
	}{
		{"not OPML", bad, "bad.opml: opml:"},
		{"missing file", filepath.Join(dir, "missing.opml"), "missing.opml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := filepath.Join(dir, "stalker.db")
			a := newApp(t, map[string]string{"STALKER_DB": db})
			if code := a.run(t, "import", tt.file); code != exitFail {
				t.Errorf("exit code = %d, want %d", code, exitFail)
			}
			if !strings.Contains(a.stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", a.stderr, tt.wantStderr)
			}
			if _, err := os.Stat(db); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a failed import created the database: %v", err)
			}
		})
	}
}

func TestCommandsNeedExistingDatabase(t *testing.T) {
	for _, cmd := range []string{"export", "check"} {
		t.Run(cmd, func(t *testing.T) {
			db := filepath.Join(t.TempDir(), "missing.db")
			a := newApp(t, map[string]string{"STALKER_DB": db})
			if code := a.run(t, cmd); code != exitFail {
				t.Errorf("exit code = %d, want %d", code, exitFail)
			}
			if !strings.Contains(a.stderr.String(), "no database at "+db) {
				t.Errorf("stderr = %q", a.stderr)
			}
			if _, err := os.Stat(db); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s created the database: %v", cmd, err)
			}
		})
	}
}

func TestCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gone":
			w.WriteHeader(http.StatusGone)
		default:
			fmt.Fprint(w, rss(strings.Trim(r.URL.Path, "/"), 2))
		}
	}))
	defer srv.Close()

	tests := []struct {
		name     string
		paths    []string
		want     int
		wantRows []string // status and title of each row, in order
		summary  string
	}{
		{"all fine", []string{"/beta", "/alpha"}, exitOK, []string{"OK alpha", "OK beta"}, "2 follows: 2 OK, 0 failed"},
		{"one failure", []string{"/alpha", "/gone"}, exitFail, []string{"FAIL 127.0.0.1", "OK alpha"}, "2 follows: 1 OK, 1 failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := filepath.Join(t.TempDir(), "stalker.db")
			for _, p := range tt.paths {
				seed(t, db, srv.URL+p)
			}
			a := newApp(t, map[string]string{"STALKER_DB": db})
			if code := a.run(t, "check", "-concurrency", "2"); code != tt.want {
				t.Errorf("exit code = %d, want %d\nstderr: %s", code, tt.want, a.stderr)
			}
			lines := strings.Split(strings.TrimSpace(a.stdout.String()), "\n")
			if len(lines) != len(tt.wantRows)+3 {
				t.Fatalf("output has %d lines:\n%s", len(lines), a.stdout)
			}
			header := lines[0]
			for _, col := range []string{"STATUS", "TITLE", "POSTS", "TIME", "ERROR"} {
				if !strings.Contains(header, col) {
					t.Errorf("header %q lacks %s", header, col)
				}
			}
			for i, want := range tt.wantRows {
				row := lines[i+1]
				status, title, _ := strings.Cut(want, " ")
				if !strings.HasPrefix(row, status+" ") || strings.Index(row, title) != strings.Index(header, "TITLE") {
					t.Errorf("row %d = %q, want %s with the title aligned under TITLE", i, row, want)
				}
				if status == "FAIL" && !strings.HasSuffix(row, "HTTP 410 Gone") {
					t.Errorf("row %d = %q, want the error at the end", i, row)
				}
				if status == "OK" && strings.Index(row, "2") != strings.Index(header, "POSTS") {
					t.Errorf("row %d = %q, want 2 posts under POSTS", i, row)
				}
			}
			if last := lines[len(lines)-1]; !strings.HasPrefix(last, tt.summary) {
				t.Errorf("summary = %q, want it to start with %q", last, tt.summary)
			}
		})
	}
}

func TestCheckRefusesPrivateAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, rss("local", 2))
	}))
	defer srv.Close()
	db := filepath.Join(t.TempDir(), "stalker.db")
	seed(t, db, srv.URL+"/local")

	for _, tt := range []struct {
		env  string
		want int
	}{{"", exitFail}, {"1", exitOK}} {
		a := newApp(t, map[string]string{"STALKER_DB": db, "STALKER_ALLOW_PRIVATE": tt.env})
		a.feedOptions.AllowPrivateNetworks = false
		if code := a.run(t, "check"); code != tt.want {
			t.Errorf("STALKER_ALLOW_PRIVATE=%q: exit code = %d, want %d\n%s", tt.env, code, tt.want, a.stdout)
		}
		if tt.want == exitFail && !strings.Contains(a.stdout.String(), "is a private network address") {
			t.Errorf("STALKER_ALLOW_PRIVATE=%q: output lacks the reason:\n%s", tt.env, a.stdout)
		}
	}
}

func TestDiscover(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><head><link rel="alternate" type="application/rss+xml" title="Blog" href="/blog.rss"></head></html>`)
	})
	// not at a well-known path, so the empty page cannot find it by probing
	mux.HandleFunc("GET /blog.rss", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, rss("A Blog", 1)) })
	mux.HandleFunc("GET /empty/{$}", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "<html><body>nothing here</body></html>")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Run("found", func(t *testing.T) {
		a := newApp(t, nil)
		if code := a.run(t, "discover", srv.URL+"/"); code != exitOK {
			t.Fatalf("exit code %d: %s", code, a.stderr)
		}
		lines := strings.Split(strings.TrimSpace(a.stdout.String()), "\n")
		if len(lines) != 2 || !strings.HasPrefix(lines[0], "FEED") ||
			!strings.HasPrefix(lines[1], srv.URL+"/blog.rss") || !strings.Contains(lines[1], "A Blog") {
			t.Errorf("output:\n%s", a.stdout)
		}
	})
	t.Run("none", func(t *testing.T) {
		a := newApp(t, nil)
		if code := a.run(t, "discover", srv.URL+"/empty/"); code != exitFail {
			t.Errorf("exit code = %d, want %d", code, exitFail)
		}
		if !strings.Contains(a.stderr.String(), "no RSS, Atom or JSON feed") {
			t.Errorf("stderr = %q", a.stderr)
		}
	})
}

func TestCell(t *testing.T) {
	tests := []struct {
		in    string
		width int
		want  string
	}{
		{"plain", 10, "plain"},
		{"tab\tand\nnewline", 0, "tab and newline"},
		{"exactly ten", 11, "exactly ten"},
		{"much too long", 8, "much to…"},
		{"שלום עולם", 5, "שלום…"},
	}
	for _, tt := range tests {
		if got := cell(tt.in, tt.width); got != tt.want {
			t.Errorf("cell(%q, %d) = %q, want %q", tt.in, tt.width, got, tt.want)
		}
	}
}

// seed creates the database at path if needed and adds a due follow for feedURL.
func seed(t *testing.T, path, feedURL string) int64 {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	f := model.Follow{URL: feedURL, FeedURL: feedURL, Importance: model.Frequent}
	if err := st.CreateFollow(context.Background(), &f); err != nil {
		t.Fatal(err)
	}
	return f.ID
}

// rss renders a feed titled title with n posts from the past days.
func rss(title string, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<?xml version="1.0"?><rss version="2.0"><channel><title>%s</title><link>https://example.com/</link>`, title)
	for i := range n {
		fmt.Fprintf(&b, `<item><guid>%s-%d</guid><title>Post %d</title><link>https://example.com/%d</link><pubDate>%s</pubDate></item>`,
			title, i, i, i, time.Now().Add(-time.Duration(i+1)*24*time.Hour).Format(time.RFC1123Z))
	}
	b.WriteString(`</channel></rss>`)
	return b.String()
}
