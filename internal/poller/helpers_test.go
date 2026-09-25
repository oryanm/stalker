package poller

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oryanm/stalker/internal/events"
	"github.com/oryanm/stalker/internal/feed"
	"github.com/oryanm/stalker/internal/model"
	"github.com/oryanm/stalker/internal/store"
)

func TestMain(m *testing.M) {
	// fetch failures are expected in these tests and would only clutter the output
	slog.SetDefault(slog.New(slog.DiscardHandler))
	os.Exit(m.Run())
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "stalker.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	return st
}

// newPoller returns a poller whose client goes through rt (the default
// transport, allowed to reach httptest servers, when nil) without per-host
// throttling.
func newPoller(st *store.Store, rt http.RoundTripper, opts Options) (*Poller, *events.Broker) {
	if opts.Jitter == nil {
		opts.Jitter = func() float64 { return 0 }
	}
	fc := feed.NewClient(feed.Options{Transport: rt, HostSpacing: -1, HostParallel: 64, AllowPrivateNetworks: true})
	ev := events.NewBroker()
	return New(st, fc, ev, opts), ev
}

func addFollow(t *testing.T, st *store.Store, f model.Follow) model.Follow {
	t.Helper()
	if err := st.CreateFollow(context.Background(), &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func getFollow(t *testing.T, st *store.Store, id int64) model.Follow {
	t.Helper()
	f, err := st.GetFollow(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func postCount(t *testing.T, st *store.Store, id int64) int {
	t.Helper()
	posts, err := st.RecentPosts(context.Background(), id, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return len(posts)
}

// startRun runs p until the returned stop func (also run on cleanup) is called.
func startRun(t *testing.T, p *Poller) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("Run: %v", err)
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock {
	return &clock{t: time.Now().UTC().Truncate(time.Millisecond)}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// feedXML renders an RSS feed named name with n posts an hour apart, the
// newest an hour before now.
func feedXML(name string, now time.Time, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>%[1]s</title><link>https://%[1]s.example/</link><description>About %[1]s</description>`, name)
	for i := range n {
		fmt.Fprintf(&b, `<item><guid>https://%[1]s.example/%[2]d</guid><title>%[1]s post %[2]d</title>`+
			`<link>https://%[1]s.example/%[2]d</link><pubDate>%[3]s</pubDate></item>`,
			name, i, now.Add(-time.Duration(i+1)*time.Hour).Format(time.RFC1123Z))
	}
	b.WriteString(`</channel></rss>`)
	return b.String()
}

const postsPerFeed = 3

// writeFeed serves the default feed for a request: named after its path.
func writeFeed(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/rss+xml")
	_, _ = io.WriteString(w, feedXML(strings.Trim(r.URL.Path, "/"), time.Now(), postsPerFeed))
}

// site is a fake feed host. It works as an http.Handler for httptest servers
// and as an in-process http.RoundTripper, which synctest bubbles need because
// goroutines blocked on real sockets never count as idle.
type site struct {
	handle       http.HandlerFunc // nil serves writeFeed
	ignoreCancel bool             // RoundTrip delivers responses even after the request context ended

	mu        sync.Mutex
	hits      map[string]int
	gates     map[string]chan struct{}
	active    int
	maxActive int
}

func newSite() *site {
	return &site{hits: map[string]int{}, gates: map[string]chan struct{}{}}
}

// gate holds requests for path until the returned channel is closed.
func (s *site) gate(path string) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := make(chan struct{})
	s.gates[path] = g
	return g
}

func (s *site) hitCount(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[path]
}

func (s *site) totalHits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, h := range s.hits {
		n += h
	}
	return n
}

func (s *site) concurrency() (active, maxActive int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active, s.maxActive
}

func (s *site) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.hits[r.URL.Path]++
	s.active++
	s.maxActive = max(s.maxActive, s.active)
	g := s.gates[r.URL.Path]
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.active--
		s.mu.Unlock()
	}()

	if g != nil {
		if s.ignoreCancel {
			<-g
		} else {
			select {
			case <-g:
			case <-r.Context().Done():
				return
			}
		}
	}
	if s.handle != nil {
		s.handle(w, r)
		return
	}
	writeFeed(w, r)
}

func (s *site) RoundTrip(req *http.Request) (*http.Response, error) {
	done := make(chan *http.Response, 1)
	go func() {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		done <- rec.Result()
	}()
	var resp *http.Response
	if s.ignoreCancel {
		resp = <-done
	} else {
		select {
		case resp = <-done:
			// a handler that gave up on a cancelled request must not look like a response
			if err := req.Context().Err(); err != nil {
				return nil, err
			}
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	resp.Request = req
	return resp, nil
}

// drain returns the events already buffered on ch.
func drain(ch <-chan events.Event) []events.Event {
	var out []events.Event
	for {
		select {
		case e := <-ch:
			out = append(out, e)
		default:
			return out
		}
	}
}
