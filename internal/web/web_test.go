package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oryanm/stalker/internal/discover"
	"github.com/oryanm/stalker/internal/events"
	"github.com/oryanm/stalker/internal/model"
	"github.com/oryanm/stalker/internal/opml"
	"github.com/oryanm/stalker/internal/store"
)

const (
	testUser = "me"
	testPass = "s3cret"
)

type fakeFetcher struct {
	mu       sync.Mutex
	fetched  []int64
	kicks    int
	fetching map[int64]bool
	fetch    func(ctx context.Context, id int64) error // optional behaviour of FetchNow
	limit    time.Duration
	limitErr error
}

func (f *fakeFetcher) MinInterval(context.Context) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.limit
}

func (f *fakeFetcher) SetMinInterval(_ context.Context, d time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.limitErr != nil {
		return f.limitErr
	}
	f.limit = d
	return nil
}

func (f *fakeFetcher) FetchNow(ctx context.Context, id int64) error {
	f.mu.Lock()
	f.fetched = append(f.fetched, id)
	fn := f.fetch
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, id)
	}
	return nil
}

func (f *fakeFetcher) Kick() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kicks++
}

func (f *fakeFetcher) IsFetching(id int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetching[id]
}

func (f *fakeFetcher) setFetching(id int64, v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fetching == nil {
		f.fetching = map[int64]bool{}
	}
	f.fetching[id] = v
}

func (f *fakeFetcher) calls() (fetched []int64, kicks int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.fetched), f.kicks
}

type fakeDiscoverer struct {
	mu      sync.Mutex
	results map[string][]discover.Candidate
	errs    map[string]error
	inputs  []string
}

func (d *fakeDiscoverer) Discover(_ context.Context, input string) ([]discover.Candidate, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.inputs = append(d.inputs, input)
	if err, ok := d.errs[input]; ok {
		return nil, err
	}
	if cs, ok := d.results[input]; ok {
		return cs, nil
	}
	return nil, discover.ErrNoFeed
}

type env struct {
	t       *testing.T
	st      *store.Store
	srv     *Server
	h       http.Handler
	fetcher *fakeFetcher
	disc    *fakeDiscoverer
	ev      *events.Broker
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "stalker.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{
		t:       t,
		st:      st,
		fetcher: &fakeFetcher{},
		disc:    &fakeDiscoverer{results: map[string][]discover.Candidate{}, errs: map[string]error{}},
		ev:      events.NewBroker(),
	}
	e.srv, err = New(st, e.disc, e.fetcher, e.ev, Config{Username: testUser, Password: testPass})
	if err != nil {
		t.Fatal(err)
	}
	e.srv.now = func() time.Time { return now }
	e.srv.log = slog.New(slog.DiscardHandler)
	e.srv.fetchWait = 2 * time.Second
	e.h = e.srv.Handler()
	return e
}

// do sends an authenticated same-origin request; extra headers come in pairs.
func (e *env) do(method, target string, body io.Reader, contentType string, headers ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, target, body)
	req.SetBasicAuth(testUser, testPass)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if method == http.MethodPost {
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	return w
}

func (e *env) get(target string, headers ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do(http.MethodGet, target, nil, "", headers...)
}

func (e *env) post(target string, form url.Values, headers ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do(http.MethodPost, target, strings.NewReader(form.Encode()), "application/x-www-form-urlencoded", headers...)
}

var htmxFragment = []string{"HX-Request", "true"}

func (e *env) createFollow(f model.Follow) model.Follow {
	e.t.Helper()
	if err := e.st.CreateFollow(context.Background(), &f); err != nil {
		e.t.Fatal(err)
	}
	return f
}

func (e *env) recordPosts(id int64, posts ...model.Post) {
	e.t.Helper()
	err := e.st.RecordFetch(context.Background(), store.FetchResult{
		FollowID: id, FetchedAt: now, NextFetchAt: now.Add(time.Hour), Posts: posts,
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) importFraidycat() {
	e.t.Helper()
	f, err := os.Open("../../testdata/fraidycat-sample.opml")
	if err != nil {
		e.t.Fatal(err)
	}
	defer f.Close()
	entries, err := opml.Parse(f)
	if err != nil {
		e.t.Fatal(err)
	}
	follows := make([]model.Follow, len(entries))
	for i, en := range entries {
		follows[i] = en.ToFollow(now)
	}
	if added, _, err := e.st.ImportFollows(context.Background(), follows); err != nil || added != 14 {
		e.t.Fatalf("import: added %d, err %v", added, err)
	}
}

func wantStatus(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d; body:\n%s", w.Code, status, w.Body.String())
	}
}

func wantContains(t *testing.T, body string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q", want)
		}
	}
}

func wantNotContains(t *testing.T, body string, unwanted ...string) {
	t.Helper()
	for _, u := range unwanted {
		if strings.Contains(body, u) {
			t.Errorf("body contains %q", u)
		}
	}
}

var rowRE = regexp.MustCompile(`<li id="follow-(\d+)"`)

func rowIDs(body string) []int64 {
	var ids []int64
	for _, m := range rowRE.FindAllStringSubmatch(body, -1) {
		id, _ := strconv.ParseInt(m[1], 10, 64)
		ids = append(ids, id)
	}
	return ids
}

func TestNewValidatesConfig(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	disc, f := &fakeDiscoverer{}, &fakeFetcher{}
	tests := []struct {
		name    string
		st      *store.Store
		disc    Discoverer
		f       Fetcher
		cfg     Config
		wantErr bool
	}{
		{"password", st, disc, f, Config{Password: "x"}, false},
		{"no auth", st, disc, f, Config{NoAuth: true}, false},
		{"missing password", st, disc, f, Config{Username: "u"}, true},
		{"nil store", nil, disc, f, Config{NoAuth: true}, true},
		{"nil discoverer", st, nil, f, Config{NoAuth: true}, true},
		{"nil fetcher", st, disc, nil, Config{NoAuth: true}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := New(tt.st, tt.disc, tt.f, nil, tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && s.Handler() == nil {
				t.Fatal("nil handler")
			}
		})
	}
}

func TestBasicAuth(t *testing.T) {
	e := newEnv(t)
	tests := []struct {
		name       string
		user, pass string
		auth       bool
		want       int
	}{
		{"no credentials", "", "", false, http.StatusUnauthorized},
		{"wrong password", testUser, "nope", true, http.StatusUnauthorized},
		{"wrong user", "stalker", testPass, true, http.StatusUnauthorized},
		{"password prefix", testUser, testPass[:3], true, http.StatusUnauthorized},
		{"correct", testUser, testPass, true, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, path := range []string{"/", "/settings", "/events?x", "/export.opml"} {
				if path == "/events?x" && tt.want == http.StatusOK {
					continue // streams forever; covered by the SSE tests
				}
				req := httptest.NewRequest(http.MethodGet, path, nil)
				if tt.auth {
					req.SetBasicAuth(tt.user, tt.pass)
				}
				w := httptest.NewRecorder()
				e.h.ServeHTTP(w, req)
				if w.Code != tt.want {
					t.Errorf("%s: status %d, want %d", path, w.Code, tt.want)
				}
				if tt.want == http.StatusUnauthorized && w.Header().Get("WWW-Authenticate") != `Basic realm="stalker"` {
					t.Errorf("%s: WWW-Authenticate = %q", path, w.Header().Get("WWW-Authenticate"))
				}
			}
		})
	}

	t.Run("no auth mode", func(t *testing.T) {
		s, err := New(e.st, e.disc, e.fetcher, e.ev, Config{NoAuth: true, AllowedHosts: []string{" NAS.lan "}})
		if err != nil {
			t.Fatal(err)
		}
		for host, want := range map[string]int{
			"localhost:8080":             http.StatusOK,
			"LOCALHOST":                  http.StatusOK,
			"stalker.localhost:80":       http.StatusOK,
			"127.0.0.1:8080":             http.StatusOK,
			"192.168.1.5:8080":           http.StatusOK,
			"[::1]:8080":                 http.StatusOK,
			"[::1]":                      http.StatusOK,
			"nas.lan:8080":               http.StatusOK,
			"nas.lan.":                   http.StatusOK,
			"attacker.example:8080":      http.StatusForbidden, // DNS rebinding
			"example.com":                http.StatusForbidden,
			"localhost.attacker.example": http.StatusForbidden,
			"":                           http.StatusForbidden,
		} {
			for _, path := range []string{"/", "/export.opml"} {
				req := httptest.NewRequest(http.MethodGet, path, nil)
				req.Host = host
				w := httptest.NewRecorder()
				s.Handler().ServeHTTP(w, req)
				if w.Code != want {
					t.Errorf("GET %s with Host %q = %d, want %d", path, host, w.Code, want)
				}
			}
		}
		// a rebinding page is same-origin, so cross-origin protection alone lets it write
		req := httptest.NewRequest(http.MethodPost, "/follows/1/delete", nil)
		req.Host = "attacker.example:8080"
		req.Header.Set("Origin", "http://attacker.example:8080")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		wantStatus(t, w, http.StatusForbidden)

		// with a password the Host is not checked
		req = httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = "stalker.example.com"
		req.SetBasicAuth(testUser, testPass)
		w = httptest.NewRecorder()
		e.h.ServeHTTP(w, req)
		wantStatus(t, w, http.StatusOK)
	})
}

func TestPublicRoutes(t *testing.T) {
	e := newEnv(t)
	tests := []struct {
		path        string
		want        int
		contentType string
		cache       string
	}{
		{"/healthz", http.StatusOK, "text/plain; charset=utf-8", "no-store"},
		{"/static/htmx.min.js", http.StatusOK, "text/javascript; charset=utf-8", "no-cache"},
		{"/static/htmx-sse.js", http.StatusOK, "text/javascript; charset=utf-8", "no-cache"},
		{"/static/app.js", http.StatusOK, "text/javascript; charset=utf-8", "no-cache"},
		{e.srv.staticURL("themes/receiver.css"), http.StatusOK, "text/css; charset=utf-8", "public, max-age=31536000, immutable"},
		{"/static/themes/classic.css?v=stale", http.StatusOK, "text/css; charset=utf-8", "no-cache"},
		{"/static/", http.StatusNotFound, "", ""},
		{"/static/missing.js", http.StatusNotFound, "", ""},
		{"/static/static/app.js", http.StatusNotFound, "", ""},
	}
	for _, tt := range tests {
		// no credentials
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tt.path, nil))
		if w.Code != tt.want {
			t.Errorf("%s: status %d, want %d", tt.path, w.Code, tt.want)
			continue
		}
		if tt.contentType != "" && w.Header().Get("Content-Type") != tt.contentType {
			t.Errorf("%s: Content-Type = %q, want %q", tt.path, w.Header().Get("Content-Type"), tt.contentType)
		}
		if tt.cache != "" && w.Header().Get("Cache-Control") != tt.cache {
			t.Errorf("%s: Cache-Control = %q, want %q", tt.path, w.Header().Get("Cache-Control"), tt.cache)
		}
	}
	if w := e.get("/healthz"); w.Body.String() != "ok" {
		t.Errorf("healthz body = %q", w.Body.String())
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t)
	want := map[string]string{
		"Content-Security-Policy": "default-src 'self'; img-src 'self' https: data:; style-src 'self'; script-src 'self'; frame-ancestors 'none'",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "no-referrer",
	}
	unauthenticated := httptest.NewRecorder()
	e.h.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, "/", nil))
	for name, w := range map[string]*httptest.ResponseRecorder{
		"home":      e.get("/"),
		"healthz":   e.get("/healthz"),
		"static":    e.get("/static/app.js"),
		"not found": e.get("/nope"),
		"401":       unauthenticated,
	} {
		for h, v := range want {
			if got := w.Header().Get(h); got != v {
				t.Errorf("%s: %s = %q, want %q", name, h, got, v)
			}
		}
	}
}

func TestCrossOriginProtection(t *testing.T) {
	e := newEnv(t)
	form := strings.NewReader(url.Values{"sort": {sortTitle}}.Encode())
	tests := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"cross-site fetch metadata", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"same-site but other origin", map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"foreign Origin without metadata", map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		{"same-origin", map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusSeeOther},
		{"matching Origin", map[string]string{"Origin": "http://example.com"}, http.StatusSeeOther},
		{"no browser headers", nil, http.StatusSeeOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			form.Seek(0, io.SeekStart)
			req := httptest.NewRequest(http.MethodPost, "/settings", form)
			req.SetBasicAuth(testUser, testPass)
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			e.h.ServeHTTP(w, req)
			wantStatus(t, w, tt.want)
		})
	}
	// safe methods pass regardless
	w := e.get("/", "Sec-Fetch-Site", "cross-site")
	wantStatus(t, w, http.StatusOK)
}

func TestHomeGroupsFraidycatExport(t *testing.T) {
	e := newEnv(t)
	e.importFraidycat()

	type tier struct {
		imp   model.Importance
		count int
	}
	tests := []struct {
		name       string
		target     string
		activeTag  string
		tiers      []tier
		activeTier model.Importance
		rows       int
	}{
		{"default is home", "/", model.HomeTag, []tier{{1, 4}}, model.Frequent, 4},
		{"videos", "/?tag=" + url.QueryEscape("📹"), "📹",
			[]tier{{1, 2}, {7, 2}, {30, 1}, {365, 2}}, model.Frequent, 2},
		{"videos rarely", "/?tier=365&tag=" + url.QueryEscape("📹"), "📹",
			[]tier{{1, 2}, {7, 2}, {30, 1}, {365, 2}}, model.Rarely, 2},
		{"dev", "/?tag=" + url.QueryEscape(devTag), devTag, []tier{{7, 4}}, model.Occasional, 4},
		{"empty realtime", "/?tier=0&tag=" + url.QueryEscape(devTag), devTag, []tier{{0, 0}, {7, 4}}, model.Realtime, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := e.get(tt.target)
			wantStatus(t, w, http.StatusOK)
			body := w.Body.String()

			// tabs: 🏠 first, then sorted
			var positions []int
			for _, tag := range []string{model.HomeTag, devTag, "📹"} {
				i := strings.Index(body, `data-tag="`+tag+`"`)
				if i < 0 {
					t.Fatalf("no tab for %s", tag)
				}
				positions = append(positions, i)
			}
			if !slices.IsSorted(positions) {
				t.Errorf("tab order wrong: %v", positions)
			}
			if got := strings.Count(body, `data-tag="`); got != 3 {
				t.Errorf("%d tag tabs, want 3", got)
			}
			wantContains(t, body, `active" data-tag="`+tt.activeTag+`"`)

			if got := strings.Count(body, `<li class="tier`); got != len(tt.tiers) {
				t.Errorf("%d tier tabs, want %d", got, len(tt.tiers))
			}
			for _, tr := range tt.tiers {
				active := ""
				if tr.imp == tt.activeTier {
					active = " active"
				}
				wantContains(t, body, `<li class="tier`+active+`" data-tier="`+strconv.Itoa(int(tr.imp))+`" data-count="`+strconv.Itoa(tr.count)+`">`)
			}
			if got := len(rowIDs(body)); got != tt.rows {
				t.Errorf("%d rows, want %d", got, tt.rows)
			}
			if tt.rows == 0 {
				wantContains(t, body, `class="empty"`, "Nothing in 🚄 Realtime under "+devTag)
			}
		})
	}

	t.Run("bad tier", func(t *testing.T) {
		wantStatus(t, e.get("/?tier=2"), http.StatusBadRequest)
		wantStatus(t, e.get("/?tier=x"), http.StatusBadRequest)
	})
	t.Run("unknown tag", func(t *testing.T) {
		w := e.get("/?tag=nothing")
		wantStatus(t, w, http.StatusOK)
		wantContains(t, w.Body.String(), "Nothing in 🚄 Realtime under nothing yet.")
	})
}

func TestHomeWelcome(t *testing.T) {
	e := newEnv(t)
	w := e.get("/")
	wantStatus(t, w, http.StatusOK)
	body := w.Body.String()
	wantContains(t, body, "Ready?", `href="/add"`, `href="/settings#import"`,
		`<body hx-boost="true" hx-ext="sse" sse-connect="/events">`)
	wantNotContains(t, body, `class="tags"`, `class="tiers"`)
}

func TestHomeRows(t *testing.T) {
	e := newEnv(t)
	fresh := e.createFollow(model.Follow{
		URL: "https://fresh.example/", FeedURL: "https://fresh.example/feed", FeedTitle: "Fresh Blog",
		Importance: model.Realtime, CreatedAt: now.Add(-100 * day),
	})
	e.recordPosts(fresh.ID,
		model.Post{GUID: "1", URL: "https://fresh.example/1", Title: "Newest post", PublishedAt: now.Add(-2 * time.Hour)},
		model.Post{GUID: "2", URL: "https://fresh.example/2", Title: "Older post", PublishedAt: now.Add(-10 * day)},
		model.Post{GUID: "3", URL: "https://fresh.example/3", Title: "Oldest post", PublishedAt: now.Add(-40 * day)},
		model.Post{GUID: "4", URL: "https://fresh.example/4", Title: "Not shown inline", PublishedAt: now.Add(-50 * day)},
	)
	stale := e.createFollow(model.Follow{
		URL: "https://stale.example/", FeedURL: "https://stale.example/feed", Title: "A stale one",
		Importance: model.Realtime, CreatedAt: now.Add(-1 * day),
	})
	e.recordPosts(stale.ID, model.Post{GUID: "x", URL: "https://stale.example/x", Title: "Ancient", PublishedAt: now.Add(-100 * day)})
	broken := e.createFollow(model.Follow{
		FeedURL: "https://broken.example/feed", FeedTitle: "Broken", Importance: model.Realtime, CreatedAt: now.Add(-50 * day),
	})
	if err := e.st.RecordFetch(context.Background(), store.FetchResult{
		FollowID: broken.ID, FetchedAt: now, NextFetchAt: now, Err: `HTTP 404 <b>"gone"</b>`,
	}); err != nil {
		t.Fatal(err)
	}
	e.fetcher.setFetching(broken.ID, true)

	w := e.get("/?tag=" + url.QueryEscape(model.HomeTag))
	wantStatus(t, w, http.StatusOK)
	body := w.Body.String()

	if got := rowIDs(body); !slices.Equal(got, []int64{fresh.ID, stale.ID, broken.ID}) {
		t.Errorf("row order = %v", got)
	}
	wantContains(t, body,
		`<li id="follow-1" class="follow age-h">`,
		`<div class="summary" sse-swap="follow-1" hx-swap="innerHTML">`,
		`<div class="head age-h">`,
		`<a class="title" href="https://fresh.example/" target="_blank" rel="noopener noreferrer" dir="auto">Fresh Blog</a>`,
		`<span class="age" title="Latest post">2h</span>`,
		`<svg class="spark spark-recent"`,
		`<li class="post age-h"><a href="https://fresh.example/1" target="_blank" rel="noopener noreferrer" dir="auto">Newest post</a> <span class="age">2h</span></li>`,
		`<li class="post age-d"><a href="https://fresh.example/2"`,
		`<li class="post age-M"><a href="https://fresh.example/3"`,
		`hx-post="/follows/1/refresh" hx-target="closest .summary"`,
		`<a class="toggle" href="/follows/1/posts" hx-get="/follows/1/posts" hx-target="#posts-1"`,
		`aria-controls="posts-1" aria-expanded="false"`,
		`hx-get="/follows/1/edit" hx-target="#posts-1"`,
		`<div class="posts" id="posts-1"></div>`,
		`<li id="follow-2" class="follow age-M">`,
		`<svg class="spark spark-old"`,
		`<li id="follow-3" class="follow">`,
		`<span class="error" tabindex="0" title="Last fetch failed: HTTP 404 &lt;b&gt;&#34;gone&#34;&lt;/b&gt;">`,
		`<span class="fetching"`,
		// the tuning dial has a station for every follow with posts
		`<line class="station age-h" data-follow="1" `,
		`<line class="station age-M" data-follow="2" `,
		// the realtime tab takes the colour of its newest post
		`<li class="tag age-h active" data-tag="🏠">`,
	)
	wantNotContains(t, body, "Not shown inline", `<b>"gone"</b>`, `class="expand"`)
	if strings.Count(body, `class="fetching"`) != 1 {
		t.Error("only the fetching follow shows the indicator")
	}

	// sort settings reorder rows
	for sortBy, want := range map[string][]int64{
		sortFollowed: {stale.ID, broken.ID, fresh.ID},
		sortTitle:    {stale.ID, broken.ID, fresh.ID},
		sortRecent:   {fresh.ID, stale.ID, broken.ID},
	} {
		wantStatus(t, e.post("/settings", url.Values{"sort": {sortBy}}), http.StatusSeeOther)
		if got := rowIDs(e.get("/").Body.String()); !slices.Equal(got, want) {
			t.Errorf("sort %s: rows %v, want %v", sortBy, got, want)
		}
	}
}

func TestXSSIsNeutralized(t *testing.T) {
	e := newEnv(t)
	const script = "<script>alert(1)</script>"
	f := e.createFollow(model.Follow{
		URL: "javascript:alert(2)", FeedURL: "https://evil.example/feed", FeedTitle: script,
		Importance: model.Frequent, Tags: []string{`"><script>alert(3)</script>`},
	})
	e.recordPosts(f.ID,
		model.Post{GUID: "a", URL: "javascript:alert(4)", Title: script, PublishedAt: now.Add(-time.Hour)},
		model.Post{GUID: "b", URL: "data:text/html,<script>alert(5)</script>", Title: "data", PublishedAt: now.Add(-2 * time.Hour)},
	)
	if err := e.st.RecordFetch(context.Background(), store.FetchResult{
		FollowID: f.ID, FetchedAt: now, NextFetchAt: now, Err: script,
	}); err != nil {
		t.Fatal(err)
	}

	for _, target := range []string{
		"/?tag=" + url.QueryEscape(`"><script>alert(3)</script>`),
		"/follows/1/posts",
		"/follows/1/edit",
		"/settings",
	} {
		w := e.get(target)
		wantStatus(t, w, http.StatusOK)
		body := w.Body.String()
		// the edit form may show the stored URL as an input value, never as a link
		wantNotContains(t, body, "<script>alert", `href="javascript:`, `href="data:`, "ZgotmplZ")
		if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
			t.Errorf("%s: escaped title missing", target)
		}
	}
	// the site link falls back to the feed; the javascript: post is plain text
	body := e.get("/?tag=" + url.QueryEscape(`"><script>alert(3)</script>`)).Body.String()
	wantContains(t, body, `<a class="title" href="https://evil.example/feed"`,
		`<li class="post age-h"><span class="text" dir="auto">&lt;script&gt;alert(1)&lt;/script&gt;</span>`)
}

func TestAddForm(t *testing.T) {
	e := newEnv(t)
	w := e.get("/add?url=https%3A%2F%2Fex.example&tag=news&tier=7")
	wantStatus(t, w, http.StatusOK)
	wantContains(t, w.Body.String(),
		`name="url" value="https://ex.example"`, `name="tags" value="news"`,
		`<option value="7" selected>🐇 Occasional: For when I have free time.</option>`)

	// the home tag is implicit, so it is not prefilled
	w = e.get("/add?tag=" + url.QueryEscape(model.HomeTag) + "&tier=bogus")
	wantContains(t, w.Body.String(), `name="tags" value=""`, `<option value="1" selected>`)
}

func TestAddSingleCandidate(t *testing.T) {
	e := newEnv(t)
	e.disc.results["blog.example"] = []discover.Candidate{
		{FeedURL: "https://blog.example/feed", Title: "Blog", SiteURL: "https://blog.example/"},
	}
	e.fetcher.fetch = func(ctx context.Context, id int64) error {
		return errors.New("HTTP 500")
	}
	var got []events.Event
	sub, cancel := e.ev.Subscribe()
	defer cancel()

	w := e.post("/follows", url.Values{"url": {"blog.example"}, "title": {"My blog"}, "tags": {"news, " + devTag}, "tier": {"0"}})
	wantStatus(t, w, http.StatusSeeOther)
	follows, _ := e.st.ListFollows(context.Background())
	if len(follows) != 1 {
		t.Fatalf("%d follows", len(follows))
	}
	f := follows[0]
	if f.FeedURL != "https://blog.example/feed" || f.URL != "https://blog.example/" || f.Title != "My blog" ||
		f.FeedTitle != "Blog" || f.Importance != model.Realtime || !slices.Equal(f.Tags, []string{"news", devTag}) {
		t.Errorf("created %+v", f)
	}
	if loc := w.Header().Get("Location"); loc != "/?tag=news&tier=0#follow-1" {
		t.Errorf("Location = %q", loc)
	}
	// the failed fetch does not undo the follow
	if fetched, _ := e.fetcher.calls(); !slices.Equal(fetched, []int64{f.ID}) {
		t.Errorf("fetched %v", fetched)
	}
	select {
	case ev := <-sub:
		got = append(got, ev)
	default:
	}
	if len(got) != 1 || got[0].Kind != events.FollowsChanged {
		t.Errorf("events = %v", got)
	}

	// adding it again explains and links to the existing follow
	w = e.post("/follows", url.Values{"url": {"blog.example"}, "tier": {"1"}})
	wantStatus(t, w, http.StatusUnprocessableEntity)
	wantContains(t, w.Body.String(), "You already follow this feed.", `href="/?tag=news&amp;tier=0#follow-1"`)
}

func TestAddChannelFollowedByItsPage(t *testing.T) {
	e := newEnv(t)
	// Fraidycat exports store YouTube channel pages as feed URLs; discovery returns the feed itself
	existing := e.createFollow(model.Follow{
		FeedURL: "https://www.youtube.com/channel/UCY1kMZp36IQSyNx_9h4mpCg", Tags: []string{"📹"}, Importance: model.Frequent,
	})
	feedURL := "https://www.youtube.com/feeds/videos.xml?channel_id=UCY1kMZp36IQSyNx_9h4mpCg"
	e.disc.results["https://www.youtube.com/@MarkRober"] = []discover.Candidate{{FeedURL: feedURL, Title: "Mark Rober"}}

	w := e.post("/follows", url.Values{"url": {"https://www.youtube.com/@MarkRober"}, "tier": {"1"}})
	wantStatus(t, w, http.StatusUnprocessableEntity)
	wantContains(t, w.Body.String(), "You already follow this feed.", `href="`+followLocationEscaped(existing)+`"`)

	// the chooser skips it the same way and shows the existing follow
	w = e.post("/follows/choose", url.Values{
		"candidate": {feedURL}, "candidate_site": {""}, "candidate_title": {"Mark Rober"}, "feed": {feedURL}, "tier": {"1"},
	})
	wantStatus(t, w, http.StatusSeeOther)
	if loc := w.Header().Get("Location"); loc != followLocation(existing) {
		t.Errorf("Location = %q, want %q", loc, followLocation(existing))
	}
	if follows, _ := e.st.ListFollows(context.Background()); len(follows) != 1 {
		t.Errorf("%d follows, want the existing one only", len(follows))
	}
}

// followLocationEscaped is followLocation as html/template writes it into an href.
func followLocationEscaped(f model.Follow) string {
	return strings.ReplaceAll(followLocation(f), "&", "&amp;")
}

func TestAddBoundedWait(t *testing.T) {
	e := newEnv(t)
	e.srv.fetchWait = 20 * time.Millisecond
	e.disc.results["slow.example"] = []discover.Candidate{{FeedURL: "https://slow.example/feed"}}
	release := make(chan struct{})
	finished := make(chan error, 1)
	e.fetcher.fetch = func(ctx context.Context, id int64) error {
		<-release
		finished <- ctx.Err()
		return nil
	}
	w := e.post("/follows", url.Values{"url": {"slow.example"}, "tier": {"1"}})
	wantStatus(t, w, http.StatusSeeOther)
	// the response did not wait for the fetch, which keeps running with a live context
	close(release)
	if err := <-finished; err != nil {
		t.Errorf("fetch context ended early: %v", err)
	}
}

func TestAddErrors(t *testing.T) {
	e := newEnv(t)
	e.disc.errs["bad"] = discover.ErrInvalidURL
	e.disc.errs["dns.example"] = errors.Join(discover.ErrNoFeed, errors.New("host not found: dns.example"))
	e.disc.results["js.example"] = []discover.Candidate{{FeedURL: "javascript:alert(1)"}}
	tests := []struct {
		name string
		form url.Values
		want string
	}{
		{"no feed", url.Values{"url": {"nothing.example"}, "tier": {"1"}}, "No RSS, Atom or JSON feed found at this URL."},
		{"invalid", url.Values{"url": {"bad"}, "tier": {"1"}}, "That does not look like a web address."},
		{"network", url.Values{"url": {"dns.example"}, "tier": {"1"}}, "host not found: dns.example"},
		{"unsafe candidates dropped", url.Values{"url": {"js.example"}, "tier": {"1"}}, "No RSS, Atom or JSON feed found"},
		{"empty", url.Values{"url": {" "}, "tier": {"1"}}, "Enter the address"},
		{"bad tier", url.Values{"url": {"x.example"}, "tier": {"3"}}, "Pick an importance tier."},
		{"long title", url.Values{"url": {"x.example"}, "tier": {"1"}, "title": {strings.Repeat("é", 301)}}, "Titles are limited"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := e.post("/follows", tt.form)
			wantStatus(t, w, http.StatusUnprocessableEntity)
			if w.Header().Get("HX-Push-Url") != "false" {
				t.Error("a re-rendered form must not be pushed into the history")
			}
			body := w.Body.String()
			wantContains(t, body, tt.want, `<form class="add-form"`)
			// the input survives the round trip
			wantContains(t, body, `name="url" value="`+strings.TrimSpace(tt.form.Get("url"))+`"`)
		})
	}
	if follows, _ := e.st.ListFollows(context.Background()); len(follows) != 0 {
		t.Errorf("created %d follows", len(follows))
	}
}

func TestAddMultipleCandidates(t *testing.T) {
	e := newEnv(t)
	e.disc.results["https://site.example"] = []discover.Candidate{
		{FeedURL: "https://site.example/posts.xml", Title: "Posts", SiteURL: "https://site.example/"},
		{FeedURL: "https://site.example/comments.xml", Title: "Comments", SiteURL: "https://site.example/"},
		{FeedURL: "https://site.example/links.xml", Title: "<i>Links</i>", SiteURL: "javascript:x"},
	}
	w := e.post("/follows", url.Values{"url": {"https://site.example"}, "title": {"Mine"}, "tags": {"web"}, "tier": {"7"}})
	wantStatus(t, w, http.StatusOK)
	body := w.Body.String()
	wantContains(t, body,
		`<form class="choose-form" method="post" action="/follows/choose"`,
		`name="feed" value="https://site.example/posts.xml" checked>`,
		`name="feed" value="https://site.example/comments.xml">`,
		`name="candidate_title" value="&lt;i&gt;Links&lt;/i&gt;"`,
		`name="tags" value="web"`, `<option value="7" selected>`, `name="title" value="Mine"`,
	)
	if n := strings.Count(body, `name="candidate"`); n != 3 {
		t.Errorf("%d candidates", n)
	}
	wantNotContains(t, body, "javascript:")
	if follows, _ := e.st.ListFollows(context.Background()); len(follows) != 0 {
		t.Fatal("the chooser must not create follows")
	}

	chosen := url.Values{
		"url":             {"https://site.example"},
		"title":           {"Mine"},
		"tags":            {"web"},
		"tier":            {"7"},
		"candidate":       {"https://site.example/posts.xml", "https://site.example/comments.xml", "https://site.example/links.xml"},
		"candidate_site":  {"https://site.example/", "https://site.example/", ""},
		"candidate_title": {"Posts", "Comments", "<i>Links</i>"},
	}

	t.Run("nothing picked", func(t *testing.T) {
		w := e.post("/follows/choose", chosen)
		wantStatus(t, w, http.StatusUnprocessableEntity)
		wantContains(t, w.Body.String(), "Pick at least one feed.")
	})

	t.Run("feeds outside the list are ignored", func(t *testing.T) {
		form := url.Values{}
		for k, v := range chosen {
			form[k] = v
		}
		form["feed"] = []string{"https://other.example/feed"}
		wantStatus(t, e.post("/follows/choose", form), http.StatusUnprocessableEntity)
	})

	t.Run("mismatched hidden fields", func(t *testing.T) {
		form := url.Values{"tier": {"1"}, "candidate": {"https://a.example/f"}, "feed": {"https://a.example/f"}}
		wantStatus(t, e.post("/follows/choose", form), http.StatusBadRequest)
	})

	t.Run("two picked", func(t *testing.T) {
		form := url.Values{}
		for k, v := range chosen {
			form[k] = v
		}
		form["feed"] = []string{"https://site.example/posts.xml", "https://site.example/links.xml"}
		w := e.post("/follows/choose", form)
		wantStatus(t, w, http.StatusSeeOther)
		follows, _ := e.st.ListFollows(context.Background())
		if len(follows) != 2 {
			t.Fatalf("%d follows", len(follows))
		}
		for _, f := range follows {
			// one title cannot name two feeds
			if f.Title != "" || f.Importance != model.Occasional || !slices.Equal(f.Tags, []string{"web"}) {
				t.Errorf("created %+v", f)
			}
		}
		if follows[1].FeedTitle != "<i>Links</i>" || follows[1].URL != "" {
			t.Errorf("links follow = %+v", follows[1])
		}
		if fetched, _ := e.fetcher.calls(); len(fetched) != 2 {
			t.Errorf("fetched %v", fetched)
		}
		if loc := w.Header().Get("Location"); loc != "/?tag=web&tier=7#follow-1" {
			t.Errorf("Location = %q", loc)
		}
	})

	t.Run("already followed", func(t *testing.T) {
		form := url.Values{}
		for k, v := range chosen {
			form[k] = v
		}
		form["feed"] = []string{"https://site.example/posts.xml"}
		w := e.post("/follows/choose", form)
		wantStatus(t, w, http.StatusSeeOther)
		if loc := w.Header().Get("Location"); loc != "/?tag=web&tier=7#follow-1" {
			t.Errorf("Location = %q", loc)
		}
		if follows, _ := e.st.ListFollows(context.Background()); len(follows) != 2 {
			t.Errorf("%d follows", len(follows))
		}
	})

	t.Run("single pick keeps the title", func(t *testing.T) {
		form := url.Values{}
		for k, v := range chosen {
			form[k] = v
		}
		form["feed"] = []string{"https://site.example/comments.xml"}
		wantStatus(t, e.post("/follows/choose", form), http.StatusSeeOther)
		f, err := e.st.GetFollowByFeedURL(context.Background(), "https://site.example/comments.xml")
		if err != nil || f.Title != "Mine" {
			t.Errorf("got %+v, %v", f, err)
		}
	})
}

func TestEdit(t *testing.T) {
	e := newEnv(t)
	f := e.createFollow(model.Follow{
		URL: "https://a.example/", FeedURL: "https://a.example/feed", FeedTitle: "Feed title",
		Importance: model.Frequent, Tags: []string{"x", "y"},
	})
	e.createFollow(model.Follow{FeedURL: "https://taken.example/feed"})

	t.Run("form page", func(t *testing.T) {
		w := e.get("/follows/1/edit")
		wantStatus(t, w, http.StatusOK)
		wantContains(t, w.Body.String(), "<html", `name="title" value="" placeholder="Feed title"`,
			`name="url" type="url" value="https://a.example/"`, `name="feed_url" type="url" value="https://a.example/feed"`,
			`name="tags" value="x y"`, `<option value="1" selected>🌄 Frequent: Keep just out of view. Nevertheless: beloved.</option>`,
			`<details class="delete">`, `action="/follows/1/delete"`, `<a href="/?tag=x&amp;tier=1#follow-1">Cancel</a>`)
	})

	t.Run("form fragment", func(t *testing.T) {
		w := e.get("/follows/1/edit", htmxFragment...)
		wantStatus(t, w, http.StatusOK)
		body := w.Body.String()
		wantNotContains(t, body, "<html")
		wantContains(t, body, `<form class="edit-form"`, `data-collapse>Cancel</button>`)
		if !strings.Contains(w.Header().Get("Vary"), "HX-Request") {
			t.Error("fragment responses must vary on HX-Request")
		}
	})

	t.Run("boosted request gets the page", func(t *testing.T) {
		w := e.get("/follows/1/edit", "HX-Request", "true", "HX-Boosted", "true")
		wantContains(t, w.Body.String(), "<html")
	})

	invalid := []struct {
		name string
		form url.Values
		want string
	}{
		{"javascript site", url.Values{"url": {"javascript:alert(1)"}, "feed_url": {"https://a.example/feed"}, "tier": {"1"}}, "Site URL:"},
		{"missing feed", url.Values{"feed_url": {""}, "tier": {"1"}}, "Feed URL: Enter a URL."},
		{"ftp feed", url.Values{"feed_url": {"ftp://a.example/feed"}, "tier": {"1"}}, "Feed URL: Enter a full http:// or https:// URL."},
		{"unknown tier", url.Values{"feed_url": {"https://a.example/feed"}, "tier": {"5"}}, "Pick an importance tier."},
		{"duplicate feed", url.Values{"feed_url": {"https://taken.example/feed"}, "tier": {"1"}}, "Another follow already uses this feed URL."},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			w := e.post("/follows/1", tt.form)
			wantStatus(t, w, http.StatusUnprocessableEntity)
			wantContains(t, w.Body.String(), tt.want)
		})
	}
	if got, _ := e.st.GetFollow(context.Background(), f.ID); got.FeedURL != f.FeedURL || got.Importance != f.Importance {
		t.Fatalf("invalid edits changed the follow: %+v", got)
	}

	t.Run("save", func(t *testing.T) {
		w := e.post("/follows/1", url.Values{
			"title": {"  Custom  "}, "url": {"https://site.example/"}, "feed_url": {"https://moved.example/feed"},
			"tags": {"b, " + devTag + " a"}, "tier": {"365"},
		})
		wantStatus(t, w, http.StatusSeeOther)
		got, err := e.st.GetFollow(context.Background(), f.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Title != "Custom" || got.URL != "https://site.example/" || got.FeedURL != "https://moved.example/feed" ||
			got.Importance != model.Rarely || !slices.Equal(got.Tags, []string{"a", "b", devTag}) {
			t.Errorf("saved %+v", got)
		}
		if loc := w.Header().Get("Location"); loc != "/?tag=a&tier=365#follow-1" {
			t.Errorf("Location = %q", loc)
		}
		if _, kicks := e.fetcher.calls(); kicks != 1 {
			t.Errorf("a moved feed should kick the poller, kicks = %d", kicks)
		}
	})

	t.Run("empty title and tags", func(t *testing.T) {
		w := e.post("/follows/1", url.Values{"feed_url": {"https://moved.example/feed"}, "tier": {"0"}})
		wantStatus(t, w, http.StatusSeeOther)
		got, _ := e.st.GetFollow(context.Background(), f.ID)
		if got.Title != "" || got.URL != "" || got.Tags != nil || got.DisplayTitle() != "Feed title" {
			t.Errorf("saved %+v", got)
		}
		if loc := w.Header().Get("Location"); loc != "/?tag=%F0%9F%8F%A0&tier=0#follow-1" {
			t.Errorf("Location = %q", loc)
		}
		if _, kicks := e.fetcher.calls(); kicks != 2 {
			t.Errorf("a more important tier should kick the poller, kicks = %d", kicks)
		}
	})

	t.Run("unchanged feed and a less important tier", func(t *testing.T) {
		w := e.post("/follows/1", url.Values{"feed_url": {"https://moved.example/feed"}, "tier": {"7"}})
		wantStatus(t, w, http.StatusSeeOther)
		if _, kicks := e.fetcher.calls(); kicks != 2 {
			t.Errorf("an unchanged feed must not kick, kicks = %d", kicks)
		}
	})

	for _, tt := range []struct {
		method, target string
		want           int
	}{
		{http.MethodGet, "/follows/99/edit", http.StatusNotFound},
		{http.MethodGet, "/follows/abc/edit", http.StatusBadRequest},
		{http.MethodGet, "/follows/0/edit", http.StatusBadRequest},
		{http.MethodPost, "/follows/99", http.StatusNotFound},
		{http.MethodGet, "/follows/99/posts", http.StatusNotFound},
		{http.MethodPost, "/follows/99/refresh", http.StatusNotFound},
	} {
		w := e.do(tt.method, tt.target, strings.NewReader("feed_url=https://x.example/&tier=1"), "application/x-www-form-urlencoded")
		if w.Code != tt.want {
			t.Errorf("%s %s = %d, want %d", tt.method, tt.target, w.Code, tt.want)
		}
	}
}

func TestDelete(t *testing.T) {
	e := newEnv(t)
	f := e.createFollow(model.Follow{FeedURL: "https://a.example/feed", Importance: model.Occasional, Tags: []string{"t"}})
	e.recordPosts(f.ID, model.Post{GUID: "1", Title: "p", PublishedAt: now})
	e.createFollow(model.Follow{FeedURL: "https://b.example/feed", Importance: model.Occasional, Tags: []string{"t"}})
	e.createFollow(model.Follow{FeedURL: "https://c.example/feed", Importance: model.Rarely, Tags: []string{"t"}})
	e.createFollow(model.Follow{FeedURL: "https://d.example/feed", Importance: model.Rarely, Tags: []string{"u"}})

	// each redirect lands on the narrowest list that still has follows
	for _, tt := range []struct {
		id   int64
		want string
	}{
		{1, "/?tag=t&tier=7"},
		{2, "/?tag=t"},
		{3, "/"},
	} {
		w := e.post(fmt.Sprintf("/follows/%d/delete", tt.id), nil)
		wantStatus(t, w, http.StatusSeeOther)
		if loc := w.Header().Get("Location"); loc != tt.want {
			t.Errorf("delete %d: Location = %q, want %q", tt.id, loc, tt.want)
		}
	}
	if _, err := e.st.GetFollow(context.Background(), f.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("follow still there: %v", err)
	}
	// a repeated submit is harmless
	w := e.post("/follows/1/delete", nil)
	wantStatus(t, w, http.StatusSeeOther)
	if loc := w.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q", loc)
	}
	wantStatus(t, e.post("/follows/x/delete", nil), http.StatusBadRequest)
}

func TestRefresh(t *testing.T) {
	e := newEnv(t)
	f := e.createFollow(model.Follow{FeedURL: "https://a.example/feed", FeedTitle: "Before"})
	e.fetcher.fetch = func(ctx context.Context, id int64) error {
		return e.st.RecordFetch(ctx, store.FetchResult{
			FollowID: id, FetchedAt: now, NextFetchAt: now.Add(time.Hour), FeedTitle: "After",
			Posts: []model.Post{{GUID: "n", URL: "https://a.example/n", Title: "New post", PublishedAt: now.Add(-5 * time.Minute)}},
		})
	}

	w := e.post("/follows/1/refresh", nil, htmxFragment...)
	wantStatus(t, w, http.StatusOK)
	body := w.Body.String()
	wantNotContains(t, body, "<html", `<li id="follow-`)
	wantContains(t, body, `<div class="head age-h">`, ">After</a>", `<span class="age" title="Latest post">5m</span>`, "New post")
	if fetched, _ := e.fetcher.calls(); !slices.Equal(fetched, []int64{f.ID}) {
		t.Errorf("fetched %v", fetched)
	}

	// without htmx the form posts back and lands on the list
	w = e.post("/follows/1/refresh", nil)
	wantStatus(t, w, http.StatusSeeOther)
	if loc := w.Header().Get("Location"); loc != "/?tag=%F0%9F%8F%A0&tier=0#follow-1" {
		t.Errorf("Location = %q", loc)
	}
}

func TestPosts(t *testing.T) {
	e := newEnv(t)
	f := e.createFollow(model.Follow{FeedURL: "https://a.example/feed", FeedTitle: "A"})
	var posts []model.Post
	for i := range 25 {
		posts = append(posts, model.Post{
			GUID: strconv.Itoa(i), URL: "https://a.example/" + strconv.Itoa(i), Title: "Post " + strconv.Itoa(i),
			PublishedAt: now.Add(-time.Duration(i) * day),
		})
	}
	e.recordPosts(f.ID, posts...)

	w := e.get("/follows/1/posts", htmxFragment...)
	wantStatus(t, w, http.StatusOK)
	body := w.Body.String()
	if n := strings.Count(body, `<li class="post`); n != 20 {
		t.Errorf("%d posts, want 20", n)
	}
	wantContains(t, body, `>Post 0</a> <span class="age">1m</span>`, `>Post 19</a>`)
	wantNotContains(t, body, "collapse", "Back to the list")
	wantNotContains(t, body, "<html", ">Post 20<")

	w = e.get("/follows/1/posts")
	wantContains(t, w.Body.String(), "<html", `<h1 dir="auto">A</h1>`, `<a href="/?tag=%F0%9F%8F%A0&amp;tier=0#follow-1">Back to the list</a>`)

	empty := e.createFollow(model.Follow{FeedURL: "https://b.example/feed"})
	w = e.get("/follows/"+strconv.FormatInt(empty.ID, 10)+"/posts", htmxFragment...)
	wantContains(t, w.Body.String(), "No posts yet.")
}

func multipartBody(t *testing.T, field, filename string, content []byte) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("other", "ignored"); err != nil {
		t.Fatal(err)
	}
	fw, err := mw.CreateFormFile(field, filename)
	if err != nil {
		t.Fatal(err)
	}
	fw.Write(content)
	mw.Close()
	return &buf, mw.FormDataContentType()
}

func TestImportExportRoundTrip(t *testing.T) {
	e := newEnv(t)
	data, err := os.ReadFile("../../testdata/fraidycat-sample.opml")
	if err != nil {
		t.Fatal(err)
	}
	body, ct := multipartBody(t, "file", "fraidycat-sample.opml", data)
	w := e.do(http.MethodPost, "/import", body, ct)
	wantStatus(t, w, http.StatusSeeOther)
	if loc := w.Header().Get("Location"); loc != "/settings?imported=14&skipped=0" {
		t.Fatalf("Location = %q", loc)
	}
	if _, kicks := e.fetcher.calls(); kicks != 1 {
		t.Errorf("kicks = %d", kicks)
	}
	w = e.get("/settings?imported=14&skipped=0")
	wantContains(t, w.Body.String(), "Imported 14, skipped 0.", "Follows: 14 · Tags: 3")

	w = e.get("/export.opml")
	wantStatus(t, w, http.StatusOK)
	if ct := w.Header().Get("Content-Type"); ct != "text/x-opml; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); cd != `attachment; filename="stalker.opml"` {
		t.Errorf("Content-Disposition = %q", cd)
	}
	exported := w.Body.Bytes()
	original, _ := opml.Parse(bytes.NewReader(data))
	roundTrip, err := opml.Parse(bytes.NewReader(exported))
	if err != nil {
		t.Fatal(err)
	}
	key := func(es []opml.Entry) []string {
		var out []string
		for _, en := range es {
			out = append(out, en.FeedURL+"|"+en.SiteURL+"|"+en.Title+"|"+strconv.Itoa(int(en.Importance))+"|"+strings.Join(en.Tags, ","))
		}
		slices.Sort(out)
		return out
	}
	if !slices.Equal(key(original), key(roundTrip)) {
		t.Error("export does not round-trip the import")
	}

	// importing the export again skips everything
	body, ct = multipartBody(t, "file", "stalker.opml", exported)
	w = e.do(http.MethodPost, "/import", body, ct)
	if loc := w.Header().Get("Location"); loc != "/settings?imported=0&skipped=14" {
		t.Errorf("Location = %q", loc)
	}
}

func TestImportErrors(t *testing.T) {
	e := newEnv(t)
	t.Run("not OPML", func(t *testing.T) {
		body, ct := multipartBody(t, "file", "x.opml", []byte("<html>nope</html>"))
		w := e.do(http.MethodPost, "/import", body, ct)
		wantStatus(t, w, http.StatusUnprocessableEntity)
		wantContains(t, w.Body.String(), "That file could not be imported: opml:", `<form class="import-form"`)
	})
	t.Run("no file", func(t *testing.T) {
		body, ct := multipartBody(t, "wrong", "x.opml", []byte("<opml/>"))
		wantStatus(t, e.do(http.MethodPost, "/import", body, ct), http.StatusBadRequest)
	})
	t.Run("not multipart", func(t *testing.T) {
		wantStatus(t, e.post("/import", url.Values{"file": {"x"}}), http.StatusBadRequest)
	})
	t.Run("too large", func(t *testing.T) {
		big := bytes.Repeat([]byte(" "), maxUploadBytes+1)
		body, ct := multipartBody(t, "file", "big.opml", big)
		wantStatus(t, e.do(http.MethodPost, "/import", body, ct), http.StatusRequestEntityTooLarge)
	})
	t.Run("far too large", func(t *testing.T) {
		big := bytes.Repeat([]byte(" "), maxUploadBytes+1<<20)
		body, ct := multipartBody(t, "file", "big.opml", big)
		wantStatus(t, e.do(http.MethodPost, "/import", body, ct), http.StatusRequestEntityTooLarge)
	})
	if follows, _ := e.st.ListFollows(context.Background()); len(follows) != 0 {
		t.Errorf("%d follows imported", len(follows))
	}
}

func TestSettings(t *testing.T) {
	e := newEnv(t)
	ok := e.createFollow(model.Follow{FeedURL: "https://ok.example/feed", FeedTitle: "Fine"})
	bad := e.createFollow(model.Follow{FeedURL: "https://bad.example/feed", FeedTitle: "Failing one", Importance: model.Rarely})
	for range 2 {
		if err := e.st.RecordFetch(context.Background(), store.FetchResult{
			FollowID: bad.ID, FetchedAt: now.Add(-3 * time.Hour), NextFetchAt: now, Err: "HTTP 503",
		}); err != nil {
			t.Fatal(err)
		}
	}
	_ = ok

	w := e.get("/settings")
	wantStatus(t, w, http.StatusOK)
	body := w.Body.String()
	wantContains(t, body,
		"Follows: 2 · Tags: 1",
		`<li data-tier="0"><span aria-hidden="true">🚄</span> Realtime: 1</li>`,
		`<li data-tier="365"><span aria-hidden="true">☂</span> Rarely: 1</li>`,
		`<a href="/follows/2/edit" dir="auto">Failing one</a> <span class="error-inline">Last 2 fetches failed: HTTP 503</span> <span class="age" title="Failing since">since 3h</span></li>`,
		`<input type="radio" name="sort" value="recent" checked>`,
		`action="/import" enctype="multipart/form-data"`,
		`<a href="/export.opml" hx-boost="false"`,
	)
	wantNotContains(t, body, `>Fine</a>`)

	w = e.post("/settings", url.Values{"sort": {sortTitle}})
	wantStatus(t, w, http.StatusSeeOther)
	if loc := w.Header().Get("Location"); loc != "/settings?saved=1" {
		t.Errorf("Location = %q", loc)
	}
	wantContains(t, e.get("/settings?saved=1").Body.String(), "Saved.", `value="title" checked>`)

	w = e.post("/settings", url.Values{"sort": {sortFollowed}, "next": {"/?tag=x&tier=1"}})
	if loc := w.Header().Get("Location"); loc != "/?tag=x&tier=1" {
		t.Errorf("Location = %q", loc)
	}
	for _, next := range []string{
		"//evil.example/", `/\evil.example`, `/./\evil.example`, `/.././\evil.example`, "/?tag=a\\b",
		"https://evil.example/", "/\t/evil.example", "/\x00", "evil.example", "",
	} {
		w = e.post("/settings", url.Values{"sort": {sortFollowed}, "next": {next}})
		if loc := w.Header().Get("Location"); loc != "/settings?saved=1" {
			t.Errorf("next=%q: open redirect: Location = %q", next, loc)
		}
	}
	wantStatus(t, e.post("/settings", url.Values{"sort": {"random"}}), http.StatusBadRequest)
}

func TestRecentFailuresStayQuiet(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	fail := func(f model.Follow, at time.Time) {
		t.Helper()
		if err := e.st.RecordFetch(ctx, store.FetchResult{FollowID: f.ID, FetchedAt: at, NextFetchAt: now, Err: "HTTP 404 Not Found"}); err != nil {
			t.Fatal(err)
		}
	}
	withPosts := func(url string) model.Follow {
		t.Helper()
		f := e.createFollow(model.Follow{FeedURL: url, FeedTitle: url, Importance: model.Frequent})
		e.recordPosts(f.ID, model.Post{GUID: "1", URL: url + "/1", Title: "Post", PublishedAt: now.Add(-48 * time.Hour)})
		return f
	}
	blip := withPosts("https://blip.example/feed")
	fail(blip, now.Add(-6*time.Hour))
	fail(blip, now.Add(-time.Hour))
	down := withPosts("https://down.example/feed")
	fail(down, now.Add(-25*time.Hour))
	fail(down, now.Add(-2*time.Hour))
	broken := e.createFollow(model.Follow{FeedURL: "https://broken.example/feed", FeedTitle: "broken", Importance: model.Frequent})
	fail(broken, now.Add(-time.Minute))

	body := e.get("/?tier=1").Body.String()
	row := func(id int64) string {
		start := strings.Index(body, fmt.Sprintf(`<li id="follow-%d"`, id))
		if start < 0 {
			t.Fatalf("no row for follow %d", id)
		}
		end := strings.Index(body[start:], `<div class="posts"`)
		return body[start : start+end]
	}
	if strings.Contains(row(blip.ID), `class="error"`) {
		t.Error("a follow failing for 6h shows a warning")
	}
	wantContains(t, row(down.ID), `<span class="error" tabindex="0" title="Last 2 fetches failed: HTTP 404 Not Found">`)
	wantContains(t, row(broken.ID), `<span class="error" tabindex="0" title="Last fetch failed: HTTP 404 Not Found">`)

	// Settings still lists every failing follow, saying which ones are not flagged yet
	body = e.get("/settings").Body.String()
	wantContains(t, body,
		`>https://blip.example/feed</a> <span class="error-inline">Last 2 fetches failed: HTTP 404 Not Found</span> <span class="age" title="Failing since">since 6h</span> <span class="hint">(retrying, not shown on its row yet)</span>`,
		`>https://down.example/feed</a> <span class="error-inline">Last 2 fetches failed: HTTP 404 Not Found</span> <span class="age" title="Failing since">since 1d</span></li>`,
	)
}

func TestTheme(t *testing.T) {
	e := newEnv(t)

	// the receiver is the default
	w := e.get("/")
	wantContains(t, w.Body.String(),
		`<html lang="en" data-theme="receiver">`,
		`<meta name="color-scheme" content="dark">`,
		`<link rel="stylesheet" href="`+e.srv.staticURL("themes/receiver.css")+`">`,
	)
	w = e.get("/settings")
	wantContains(t, w.Body.String(),
		`<form class="theme-form" method="post" action="/settings/theme" hx-boost="false">`,
		`<input type="radio" name="theme" value="receiver" checked>`,
		`<input type="radio" name="theme" value="classic">`,
	)

	w = e.post("/settings/theme", url.Values{"theme": {"classic"}})
	wantStatus(t, w, http.StatusSeeOther)
	if loc := w.Header().Get("Location"); loc != "/settings?saved=1#theme" {
		t.Errorf("Location = %q", loc)
	}
	w = e.get("/")
	wantContains(t, w.Body.String(),
		`<html lang="en" data-theme="classic">`,
		`<meta name="color-scheme" content="light dark">`,
		`<link rel="stylesheet" href="`+e.srv.staticURL("themes/classic.css")+`">`,
	)
	wantContains(t, e.get("/settings").Body.String(), `<input type="radio" name="theme" value="classic" checked>`)

	// the choice survives a restart
	srv, err := New(e.st, e.disc, e.fetcher, nil, Config{NoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := srv.currentTheme().ID; got != "classic" {
		t.Errorf("theme after restart = %q, want classic", got)
	}

	wantStatus(t, e.post("/settings/theme", url.Values{"theme": {"neon"}}), http.StatusBadRequest)
	wantContains(t, e.get("/").Body.String(), `data-theme="classic"`)

	// an unknown stored theme falls back to the default
	if err := e.st.SetSetting(context.Background(), settingTheme, "gone"); err != nil {
		t.Fatal(err)
	}
	srv, err = New(e.st, e.disc, e.fetcher, nil, Config{NoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := srv.currentTheme().ID; got != "receiver" {
		t.Errorf("theme with an unknown setting = %q, want receiver", got)
	}
}

func TestEveryThemeHasAStylesheet(t *testing.T) {
	for _, th := range themes {
		if _, err := fs.Stat(staticFS, "static/"+th.CSS()); err != nil {
			t.Errorf("theme %q: %v", th.ID, err)
		}
	}
}

func TestUpdateLimit(t *testing.T) {
	e := newEnv(t)

	w := e.get("/settings")
	wantStatus(t, w, http.StatusOK)
	wantContains(t, w.Body.String(),
		`<form class="interval-form" method="post" action="/settings/interval">`,
		`<input id="min-interval" name="interval" value="" placeholder="no limit"`,
		`Checked every: <span aria-hidden="true">🚄</span> Realtime 10m · <span aria-hidden="true">🌄</span> Frequent 1h ·`,
	)

	w = e.post("/settings/interval", url.Values{"interval": {" 3H "}})
	wantStatus(t, w, http.StatusSeeOther)
	if loc := w.Header().Get("Location"); loc != "/settings?saved=1#interval" {
		t.Errorf("Location = %q", loc)
	}
	if got := e.fetcher.MinInterval(context.Background()); got != 3*time.Hour {
		t.Errorf("limit = %v, want 3h", got)
	}
	w = e.get("/settings?saved=1")
	wantContains(t, w.Body.String(),
		`name="interval" value="3h"`,
		`Realtime 3h · <span aria-hidden="true">🌄</span> Frequent 3h · <span aria-hidden="true">🐇</span> Occasional 4h`,
		`Rarely 1d.`,
	)

	// what was typed stays in the box next to the error, and the limit is untouched
	for _, bad := range []string{"3", "soon", "10s", "45d"} {
		w = e.post("/settings/interval", url.Values{"interval": {bad}})
		wantStatus(t, w, http.StatusUnprocessableEntity)
		wantContains(t, w.Body.String(), `<p class="flash error" role="alert">`, `name="interval" value="`+bad+`"`)
	}
	if got := e.fetcher.MinInterval(context.Background()); got != 3*time.Hour {
		t.Errorf("limit after bad input = %v, want 3h", got)
	}

	w = e.post("/settings/interval", url.Values{"interval": {""}})
	wantStatus(t, w, http.StatusSeeOther)
	if got := e.fetcher.MinInterval(context.Background()); got != 0 {
		t.Errorf("limit after clearing = %v, want none", got)
	}

	e.fetcher.limitErr = errors.New("disk full")
	wantStatus(t, e.post("/settings/interval", url.Values{"interval": {"1h"}}), http.StatusInternalServerError)
}

func TestErrorPages(t *testing.T) {
	e := newEnv(t)
	w := e.get("/no/such/page")
	wantStatus(t, w, http.StatusNotFound)
	wantContains(t, w.Body.String(), "<html", "404 Not Found")

	// htmx fragments get a one-line error instead of a page
	w = e.get("/follows/42/posts", htmxFragment...)
	wantStatus(t, w, http.StatusNotFound)
	body := w.Body.String()
	wantNotContains(t, body, "<html")
	wantContains(t, body, `<p class="flash error" role="alert">That follow does not exist`)
}

func TestPanicRecovery(t *testing.T) {
	e := newEnv(t)
	h := e.srv.logAndRecover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	wantStatus(t, w, http.StatusInternalServerError)
	wantContains(t, w.Body.String(), "500 Internal Server Error", "Something went wrong.")
}

func TestStoreFailureIs500(t *testing.T) {
	e := newEnv(t)
	e.st.Close()
	w := e.get("/")
	wantStatus(t, w, http.StatusInternalServerError)
	wantContains(t, w.Body.String(), "Something went wrong.")
}
