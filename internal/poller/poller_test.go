package poller

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oryanm/stalker/internal/events"
	"github.com/oryanm/stalker/internal/feed"
	"github.com/oryanm/stalker/internal/model"
	"github.com/oryanm/stalker/internal/store"
)

func TestInterval(t *testing.T) {
	tests := []struct {
		imp  model.Importance
		want time.Duration
	}{
		{model.Realtime, 10 * time.Minute},
		{model.Frequent, time.Hour},
		{model.Occasional, 4 * time.Hour},
		{model.Sometime, 12 * time.Hour},
		{model.Rarely, 24 * time.Hour},
		// values between tiers snap down like model.NormalizeImportance
		{-3, 10 * time.Minute},
		{3, time.Hour},
		{14, 4 * time.Hour},
		{90, 12 * time.Hour},
		{1000, 24 * time.Hour},
	}
	for _, tt := range tests {
		if got := Interval(tt.imp); got != tt.want {
			t.Errorf("Interval(%d) = %v, want %v", tt.imp, got, tt.want)
		}
	}
}

func TestNextFetch(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		imp        model.Importance
		errors     int
		retryAfter time.Duration
		jitter     float64
		want       time.Duration
	}{
		{"success", model.Frequent, 0, 0, 0, time.Hour},
		{"success half jitter", model.Frequent, 0, 0, 0.5, 75 * time.Minute},
		{"success full jitter", model.Realtime, 0, 0, 1, 15 * time.Minute},
		{"success rarely", model.Rarely, 0, 0, 0, 24 * time.Hour},
		{"negative error count is success", model.Frequent, -1, 0, 0, time.Hour},
		{"first error", model.Realtime, 1, 0, 0, 10 * time.Minute},
		{"second error doubles", model.Realtime, 2, 0, 0, 20 * time.Minute},
		{"third error", model.Realtime, 3, 0, 0, 40 * time.Minute},
		{"seventh error hits the exponent cap", model.Realtime, 7, 0, 0, 640 * time.Minute},
		{"exponent stays capped", model.Realtime, 50, 0, 0, 640 * time.Minute},
		{"huge error count", model.Realtime, math.MaxInt, 0, 0, 640 * time.Minute},
		{"frequent backoff", model.Frequent, 4, 0, 0, 8 * time.Hour},
		{"capped at a day", model.Frequent, 6, 0, 0, 24 * time.Hour},
		{"cap applies before jitter", model.Occasional, 7, 0, 0.5, 30 * time.Hour},
		{"rarely stays at its interval", model.Rarely, 3, 0, 0, 24 * time.Hour},
		{"error with jitter", model.Realtime, 2, 0, 0.25, 22*time.Minute + 30*time.Second},
		{"retry after wins", model.Realtime, 1, 2 * time.Hour, 0.5, 2 * time.Hour},
		{"backoff wins over short retry after", model.Frequent, 1, time.Minute, 0, time.Hour},
		{"retry after also bounds success", model.Realtime, 0, time.Hour, 0, time.Hour},
		{"negative retry after ignored", model.Realtime, 1, -time.Hour, 0, 10 * time.Minute},
		{"negative jitter clamped", model.Frequent, 0, 0, -3, time.Hour},
		{"jitter above one clamped", model.Frequent, 0, 0, 7, 90 * time.Minute},
		{"NaN jitter ignored", model.Frequent, 0, 0, math.NaN(), time.Hour},
		{"unknown importance normalized", 3, 2, 0, 0, 2 * time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NextFetch(now, tt.imp, tt.errors, tt.retryAfter, tt.jitter)
			if d := got.Sub(now); d != tt.want {
				t.Errorf("NextFetch(now, %d, %d, %v, %v) = now+%v, want now+%v", tt.imp, tt.errors, tt.retryAfter, tt.jitter, d, tt.want)
			}
		})
	}
}

func TestErrorMessage(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"not found", &feed.HTTPError{StatusCode: 404}, "HTTP 404 Not Found"},
		{"rate limited", &feed.HTTPError{StatusCode: 429, RetryAfter: time.Hour}, "HTTP 429 Too Many Requests"},
		{"unknown status", &feed.HTTPError{StatusCode: 599}, "HTTP 599"},
		{"wrapped status", fmt.Errorf("fetch: %w", &feed.HTTPError{StatusCode: 410}), "HTTP 410 Gone"},
		{"not a feed", feed.ErrNotAFeed, "not an RSS, Atom or JSON feed"},
		{"parser detail dropped", fmt.Errorf("%w: XML syntax error on line 3", feed.ErrNotAFeed), "not an RSS, Atom or JSON feed"},
		{"redirect target kept", &feed.RedirectError{URL: "https://old.reddit.com/login/", Err: fmt.Errorf("%w: EOF", feed.ErrNotAFeed)},
			"not an RSS, Atom or JSON feed (redirected to https://old.reddit.com/login/)"},
		{"transport message kept", errors.New("host not found: nope.example"), "host not found: nope.example"},
		{"whitespace collapsed", errors.New("  line one\n\tline two  "), "line one line two"},
		{"long message truncated", errors.New(strings.Repeat("é", 400)), strings.Repeat("é", maxErrorRunes-1) + "…"},
		{"empty message", errors.New(" "), "fetch failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := errorMessage(tt.err); got != tt.want {
				t.Errorf("errorMessage() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFetchResult(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	posts := []model.Post{{GUID: "a", Title: "A"}}
	full := &feed.Result{
		ETag: `"e"`, LastModified: "lm", Title: "T", Description: "D",
		SiteURL: "https://site.example/", ImageURL: "https://site.example/i.png", Posts: posts,
	}
	tests := []struct {
		name   string
		follow model.Follow
		res    *feed.Result
		err    error
		want   store.FetchResult
	}{
		{
			name:   "success",
			follow: model.Follow{ID: 1, FeedURL: "https://site.example/feed", Importance: model.Frequent, ErrorCount: 3},
			res:    full,
			want: store.FetchResult{
				FollowID: 1, FeedURL: "https://site.example/feed", Importance: model.Frequent, FetchedAt: now, NextFetchAt: now.Add(time.Hour),
				ETag: `"e"`, LastModified: "lm", FeedTitle: "T", Description: "D",
				SiteURL: "https://site.example/", PhotoURL: "https://site.example/i.png", Posts: posts,
			},
		},
		{
			name:   "not modified keeps only validators",
			follow: model.Follow{ID: 2, Importance: model.Realtime},
			res:    &feed.Result{NotModified: true, ETag: `"e2"`, LastModified: "lm2", Title: "ignored", Posts: posts},
			want: store.FetchResult{
				FollowID: 2, FetchedAt: now, NextFetchAt: now.Add(10 * time.Minute),
				NotModified: true, ETag: `"e2"`, LastModified: "lm2",
			},
		},
		{
			name:   "failure counts from the stored error count",
			follow: model.Follow{ID: 3, FeedURL: "https://site.example/feed", Importance: model.Realtime, ErrorCount: 2},
			err:    &feed.HTTPError{StatusCode: 500},
			want: store.FetchResult{
				FollowID: 3, FeedURL: "https://site.example/feed", FetchedAt: now, NextFetchAt: now.Add(40 * time.Minute), Err: "HTTP 500 Internal Server Error",
			},
		},
		{
			name:   "failure honours retry after",
			follow: model.Follow{ID: 4, Importance: model.Realtime},
			err:    &feed.HTTPError{StatusCode: 429, RetryAfter: 3 * time.Hour},
			want: store.FetchResult{
				FollowID: 4, FetchedAt: now, NextFetchAt: now.Add(3 * time.Hour), Err: "HTTP 429 Too Many Requests",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fetchResult(tt.follow, tt.res, tt.err, now, 0, 0)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("fetchResult() =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

func TestFetchNowStoresPosts(t *testing.T) {
	st := openStore(t)
	srv := httptest.NewServer(newSite())
	defer srv.Close()
	clk := newClock()
	p, ev := newPoller(st, nil, Options{Now: clk.Now})
	feedURL := srv.URL + "/news"
	f := addFollow(t, st, model.Follow{URL: feedURL, FeedURL: feedURL, Importance: model.Frequent})

	sub, cancel := ev.Subscribe()
	defer cancel()
	fetchingAtUpdate := make(chan bool, 1)
	go func() {
		for e := range sub {
			if e.Kind == events.FollowUpdated {
				fetchingAtUpdate <- p.IsFetching(e.FollowID)
				return
			}
		}
	}()

	if err := p.FetchNow(t.Context(), f.ID); err != nil {
		t.Fatalf("FetchNow: %v", err)
	}
	if <-fetchingAtUpdate {
		t.Error("IsFetching was still true when FollowUpdated was published")
	}

	got := getFollow(t, st, f.ID)
	if got.FeedTitle != "news" || got.Description != "About news" {
		t.Errorf("feed title/description = %q/%q, want news/About news", got.FeedTitle, got.Description)
	}
	if got.URL != "https://news.example/" {
		t.Errorf("URL = %q, want the feed's site link to replace the placeholder", got.URL)
	}
	if !got.LastFetchedAt.Equal(clk.Now()) {
		t.Errorf("LastFetchedAt = %v, want %v", got.LastFetchedAt, clk.Now())
	}
	if want := clk.Now().Add(time.Hour); !got.NextFetchAt.Equal(want) {
		t.Errorf("NextFetchAt = %v, want %v", got.NextFetchAt, want)
	}
	if got.LastError != "" || got.ErrorCount != 0 {
		t.Errorf("error state = %q/%d, want none", got.LastError, got.ErrorCount)
	}
	if n := postCount(t, st, f.ID); n != postsPerFeed {
		t.Errorf("stored %d posts, want %d", n, postsPerFeed)
	}
	if p.IsFetching(f.ID) {
		t.Error("IsFetching after FetchNow returned")
	}
}

func TestFetchNowPublishesEvents(t *testing.T) {
	st := openStore(t)
	srv := httptest.NewServer(newSite())
	defer srv.Close()
	p, ev := newPoller(st, nil, Options{})
	f := addFollow(t, st, model.Follow{FeedURL: srv.URL + "/a"})

	sub, cancel := ev.Subscribe()
	defer cancel()
	if err := p.FetchNow(t.Context(), f.ID); err != nil {
		t.Fatal(err)
	}
	want := []events.Event{{Kind: events.FollowFetching, FollowID: f.ID}, {Kind: events.FollowUpdated, FollowID: f.ID}}
	if got := drain(sub); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("events = %v, want %v", got, want)
	}
}

func TestFetchNowConditionalGet(t *testing.T) {
	const lastModified = "Tue, 22 Sep 2026 10:00:00 GMT"
	st := openStore(t)
	s := newSite()
	var mu sync.Mutex
	var sent []string
	s.handle = func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sent = append(sent, r.Header.Get("If-None-Match")+"|"+r.Header.Get("If-Modified-Since"))
		mu.Unlock()
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.Header().Set("ETag", `"v2"`)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Last-Modified", lastModified)
		writeFeed(w, r)
	}
	srv := httptest.NewServer(s)
	defer srv.Close()
	clk := newClock()
	p, _ := newPoller(st, nil, Options{Now: clk.Now})
	f := addFollow(t, st, model.Follow{FeedURL: srv.URL + "/a", Importance: model.Realtime})

	if err := p.FetchNow(t.Context(), f.ID); err != nil {
		t.Fatal(err)
	}
	first := getFollow(t, st, f.ID)
	if first.ETag != `"v1"` || first.LastModified != lastModified {
		t.Fatalf("validators = %q/%q after first fetch", first.ETag, first.LastModified)
	}

	clk.Add(time.Hour)
	if err := p.FetchNow(t.Context(), f.ID); err != nil {
		t.Fatal(err)
	}
	second := getFollow(t, st, f.ID)
	if want := []string{"|", `"v1"|` + lastModified}; fmt.Sprint(sent) != fmt.Sprint(want) {
		t.Errorf("conditional headers sent = %q, want %q", sent, want)
	}
	if second.ETag != `"v2"` || second.LastModified != lastModified {
		t.Errorf("validators = %q/%q after 304, want the new ETag and the old Last-Modified", second.ETag, second.LastModified)
	}
	if !second.LastFetchedAt.Equal(clk.Now()) || !second.NextFetchAt.Equal(clk.Now().Add(10*time.Minute)) {
		t.Errorf("fetch times = %v/%v, want %v and 10m later", second.LastFetchedAt, second.NextFetchAt, clk.Now())
	}
	if second.FeedTitle != "a" || postCount(t, st, f.ID) != postsPerFeed {
		t.Errorf("a 304 changed the feed data: title %q, %d posts", second.FeedTitle, postCount(t, st, f.ID))
	}
}

func TestFetchNowErrors(t *testing.T) {
	s := newSite()
	s.handle = func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/404":
			http.NotFound(w, r)
		case "/500":
			w.WriteHeader(http.StatusInternalServerError)
		case "/599":
			w.WriteHeader(599)
		case "/page":
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprint(w, "<!doctype html><html><head><title>Hi</title></head><body>hello</body></html>")
		case "/slow":
			<-r.Context().Done()
		default:
			writeFeed(w, r)
		}
	}
	srv := httptest.NewServer(s)
	defer srv.Close()
	down := httptest.NewServer(s)
	downURL := down.URL
	down.Close()

	tests := []struct {
		name       string
		feedURL    string
		want       string
		wantStatus int
	}{
		{"not found", srv.URL + "/404", "HTTP 404 Not Found", 404},
		{"server error", srv.URL + "/500", "HTTP 500 Internal Server Error", 500},
		{"unknown status", srv.URL + "/599", "HTTP 599", 599},
		{"html page", srv.URL + "/page", "not an RSS, Atom or JSON feed", 0},
		{"timeout", srv.URL + "/slow", "request timed out", 0},
		{"connection refused", downURL + "/feed", "connection refused by 127.0.0.1", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := openStore(t)
			clk := newClock()
			p, _ := newPoller(st, nil, Options{Now: clk.Now})
			if tt.name == "timeout" {
				p.fetchTimeout = 100 * time.Millisecond
			}
			f := addFollow(t, st, model.Follow{FeedURL: tt.feedURL, Importance: model.Frequent})

			err := p.FetchNow(t.Context(), f.ID)
			if err == nil || err.Error() != tt.want {
				t.Fatalf("FetchNow error = %v, want %q", err, tt.want)
			}
			if he, ok := errors.AsType[*feed.HTTPError](err); ok != (tt.wantStatus != 0) || ok && he.StatusCode != tt.wantStatus {
				t.Errorf("errors.As HTTPError = %v, %v; want status %d", he, ok, tt.wantStatus)
			}
			got := getFollow(t, st, f.ID)
			if got.LastError != tt.want || got.ErrorCount != 1 {
				t.Errorf("stored error = %q/%d, want %q/1", got.LastError, got.ErrorCount, tt.want)
			}
			if want := clk.Now().Add(time.Hour); !got.NextFetchAt.Equal(want) {
				t.Errorf("NextFetchAt = %v, want %v", got.NextFetchAt, want)
			}
		})
	}
}

func TestFetchNowBackoffAndRecovery(t *testing.T) {
	st := openStore(t)
	s := newSite()
	var mu sync.Mutex
	failing := true
	s.handle = func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if failing {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		writeFeed(w, r)
	}
	srv := httptest.NewServer(s)
	defer srv.Close()
	clk := newClock()
	p, _ := newPoller(st, nil, Options{Now: clk.Now})
	f := addFollow(t, st, model.Follow{FeedURL: srv.URL + "/a", Importance: model.Realtime})

	wantDelays := []time.Duration{10, 20, 40, 80, 160, 320, 640, 640}
	for i, want := range wantDelays {
		clk.Add(time.Minute)
		if err := p.FetchNow(t.Context(), f.ID); err == nil {
			t.Fatalf("fetch %d succeeded", i+1)
		}
		got := getFollow(t, st, f.ID)
		if got.ErrorCount != i+1 {
			t.Errorf("fetch %d: ErrorCount = %d", i+1, got.ErrorCount)
		}
		if d := got.NextFetchAt.Sub(got.LastFetchedAt); d != want*time.Minute {
			t.Errorf("fetch %d: next fetch in %v, want %v", i+1, d, want*time.Minute)
		}
	}

	mu.Lock()
	failing = false
	mu.Unlock()
	clk.Add(time.Minute)
	if err := p.FetchNow(t.Context(), f.ID); err != nil {
		t.Fatalf("recovery fetch: %v", err)
	}
	got := getFollow(t, st, f.ID)
	if got.ErrorCount != 0 || got.LastError != "" {
		t.Errorf("error state after recovery = %q/%d", got.LastError, got.ErrorCount)
	}
	if d := got.NextFetchAt.Sub(got.LastFetchedAt); d != 10*time.Minute {
		t.Errorf("next fetch after recovery in %v, want 10m", d)
	}
}

func TestFetchNowRetryAfter(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		retryAfter string
		imp        model.Importance
		want       time.Duration
	}{
		{"retry after beats the backoff", http.StatusTooManyRequests, "3600", model.Realtime, time.Hour},
		{"backoff beats a short retry after", http.StatusServiceUnavailable, "60", model.Realtime, 10 * time.Minute},
		{"no retry after", http.StatusTooManyRequests, "", model.Frequent, time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := openStore(t)
			s := newSite()
			s.handle = func(w http.ResponseWriter, _ *http.Request) {
				if tt.retryAfter != "" {
					w.Header().Set("Retry-After", tt.retryAfter)
				}
				w.WriteHeader(tt.status)
			}
			srv := httptest.NewServer(s)
			defer srv.Close()
			clk := newClock()
			p, _ := newPoller(st, nil, Options{Now: clk.Now})
			f := addFollow(t, st, model.Follow{FeedURL: srv.URL + "/a", Importance: tt.imp})

			if err := p.FetchNow(t.Context(), f.ID); err == nil {
				t.Fatal("FetchNow succeeded")
			}
			if got := getFollow(t, st, f.ID).NextFetchAt.Sub(clk.Now()); got != tt.want {
				t.Errorf("next fetch in %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFetchNowUnknownFollow(t *testing.T) {
	st := openStore(t)
	p, ev := newPoller(st, newSite(), Options{})
	sub, cancel := ev.Subscribe()
	defer cancel()

	if err := p.FetchNow(t.Context(), 42); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("FetchNow(unknown) = %v, want store.ErrNotFound", err)
	}
	if p.IsFetching(42) {
		t.Error("unknown follow left in flight")
	}
	if got := drain(sub); len(got) != 0 {
		t.Errorf("events for a fetch that never started: %v", got)
	}
}

func TestFetchNowFollowDeletedDuringFetch(t *testing.T) {
	st := openStore(t)
	s := newSite()
	var f model.Follow
	s.handle = func(w http.ResponseWriter, r *http.Request) {
		if err := st.DeleteFollow(context.Background(), f.ID); err != nil {
			t.Error(err)
		}
		writeFeed(w, r)
	}
	srv := httptest.NewServer(s)
	defer srv.Close()
	p, _ := newPoller(st, nil, Options{})
	f = addFollow(t, st, model.Follow{FeedURL: srv.URL + "/a"})

	if err := p.FetchNow(t.Context(), f.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("FetchNow = %v, want store.ErrNotFound", err)
	}
	if len(p.held) != 0 {
		t.Errorf("a deleted follow was held back: %v", p.held)
	}
}

func TestFetchNowFeedURLEditedDuringFetch(t *testing.T) {
	st := openStore(t)
	s := newSite()
	var f model.Follow
	var srvURL string
	s.handle = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			// the user corrects the feed URL while the old one is being fetched
			edited := f
			edited.FeedURL = srvURL + "/new"
			if err := st.UpdateFollowSettings(context.Background(), edited); err != nil {
				t.Error(err)
			}
			w.Header().Set("ETag", `"old-etag"`)
			w.Header().Set("Last-Modified", "Tue, 22 Sep 2026 10:00:00 GMT")
		}
		if got := r.Header.Get("If-None-Match") + r.Header.Get("If-Modified-Since"); got != "" {
			t.Errorf("%s was requested with the validators %q", r.URL.Path, got)
		}
		writeFeed(w, r)
	}
	srv := httptest.NewServer(s)
	defer srv.Close()
	srvURL = srv.URL
	p, _ := newPoller(st, nil, Options{})
	f = addFollow(t, st, model.Follow{FeedURL: srv.URL + "/old", Importance: model.Rarely})

	if err := p.FetchNow(t.Context(), f.ID); err != nil {
		t.Fatalf("FetchNow: %v", err)
	}
	assertHits(t, s, map[string]int{"/old": 1, "/new": 1})
	got := getFollow(t, st, f.ID)
	if got.FeedURL != srv.URL+"/new" || got.FeedTitle != "new" || got.ETag != "" || got.LastModified != "" {
		t.Errorf("follow = feed %q, title %q, validators %q/%q; want the new feed's, without the old validators",
			got.FeedURL, got.FeedTitle, got.ETag, got.LastModified)
	}
	posts, err := st.RecentPosts(t.Context(), f.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, post := range posts {
		if !strings.HasPrefix(post.GUID, "https://new.example/") {
			t.Errorf("stored post %q of the old feed", post.GUID)
		}
	}
	if len(posts) != postsPerFeed {
		t.Errorf("stored %d posts, want %d", len(posts), postsPerFeed)
	}
}

func TestFetchNowTierEditedDuringFetch(t *testing.T) {
	st := openStore(t)
	s := newSite()
	var f model.Follow
	s.handle = func(w http.ResponseWriter, r *http.Request) {
		if s.hitCount("/a") == 1 {
			// promoted from Rarely to Realtime while the Rarely schedule is being computed
			edited := f
			edited.Importance = model.Realtime
			if err := st.UpdateFollowSettings(context.Background(), edited); err != nil {
				t.Error(err)
			}
		}
		writeFeed(w, r)
	}
	srv := httptest.NewServer(s)
	defer srv.Close()
	clk := newClock()
	p, _ := newPoller(st, nil, Options{Now: clk.Now})
	f = addFollow(t, st, model.Follow{FeedURL: srv.URL + "/a", Importance: model.Rarely})

	if err := p.FetchNow(t.Context(), f.ID); err != nil {
		t.Fatalf("FetchNow: %v", err)
	}
	if want := clk.Now().Add(Interval(model.Realtime)); !getFollow(t, st, f.ID).NextFetchAt.Equal(want) {
		t.Errorf("NextFetchAt = %v, want the Realtime interval, %v", getFollow(t, st, f.ID).NextFetchAt, want)
	}
	assertHits(t, s, map[string]int{"/a": 2})
}

func TestCheckAll(t *testing.T) {
	st := openStore(t)
	s := newSite()
	s.handle = func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/missing":
			http.NotFound(w, r)
		case "/page":
			_, _ = fmt.Fprint(w, "<html><body>not a feed</body></html>")
		default:
			time.Sleep(20 * time.Millisecond) // long enough for fetches to overlap
			writeFeed(w, r)
		}
	}
	srv := httptest.NewServer(s)
	defer srv.Close()
	p, _ := newPoller(st, nil, Options{})
	for _, f := range []model.Follow{
		{FeedURL: srv.URL + "/Gamma"},
		{FeedURL: srv.URL + "/missing", Title: "Delta"},
		{FeedURL: srv.URL + "/alpha"},
		{FeedURL: srv.URL + "/page", Title: "epsilon"},
		{FeedURL: srv.URL + "/Beta"},
	} {
		addFollow(t, st, f)
	}

	results, err := p.CheckAll(t.Context(), 2)
	if err != nil {
		t.Fatalf("CheckAll: %v", err)
	}
	want := []struct {
		title string
		posts int
		err   string
	}{
		{"alpha", postsPerFeed, ""},
		{"Beta", postsPerFeed, ""},
		{"Delta", 0, "HTTP 404 Not Found"},
		{"epsilon", 0, "not an RSS, Atom or JSON feed"},
		{"Gamma", postsPerFeed, ""},
	}
	if len(results) != len(want) {
		t.Fatalf("got %d results, want %d", len(results), len(want))
	}
	for i, w := range want {
		r := results[i]
		errText := ""
		if r.Err != nil {
			errText = r.Err.Error()
		}
		if r.Follow.DisplayTitle() != w.title || r.Posts != w.posts || errText != w.err {
			t.Errorf("result %d = %q, %d posts, err %q; want %q, %d posts, err %q",
				i, r.Follow.DisplayTitle(), r.Posts, errText, w.title, w.posts, w.err)
		}
		if r.Duration <= 0 {
			t.Errorf("result %d: Duration = %v", i, r.Duration)
		}
		if r.Follow.LastFetchedAt.IsZero() || r.Follow.LastError != w.err {
			t.Errorf("result %d holds the follow as it was before the fetch: %+v", i, r.Follow)
		}
	}
	if _, maxActive := s.concurrency(); maxActive > 2 {
		t.Errorf("%d fetches ran at once, want at most 2", maxActive)
	}
}

func TestCheckAllCancelled(t *testing.T) {
	st := openStore(t)
	s := newSite()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.handle = func(_ http.ResponseWriter, r *http.Request) {
		cancel() // interrupted during the first fetch
		<-r.Context().Done()
	}
	srv := httptest.NewServer(s)
	defer srv.Close()
	p, _ := newPoller(st, nil, Options{})
	for _, name := range []string{"a", "b", "c"} {
		addFollow(t, st, model.Follow{FeedURL: srv.URL + "/" + name})
	}

	results, err := p.CheckAll(ctx, 1)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("CheckAll error = %v, want context.Canceled", err)
	}
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	for _, r := range results {
		if !errors.Is(r.Err, context.Canceled) || !r.Follow.LastFetchedAt.IsZero() || r.Follow.ErrorCount != 0 {
			t.Errorf("result %q: err %v, fetched at %v; want cancelled and unrecorded", r.Follow.FeedURL, r.Err, r.Follow.LastFetchedAt)
		}
	}
	if n := s.totalHits(); n != 1 {
		t.Errorf("%d requests, want only the interrupted one", n)
	}
}

func TestCheckAllEmpty(t *testing.T) {
	p, _ := newPoller(openStore(t), newSite(), Options{})
	results, err := p.CheckAll(t.Context(), 0)
	if err != nil || len(results) != 0 {
		t.Errorf("CheckAll on no follows = %v, %v", results, err)
	}
}
