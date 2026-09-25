package feed

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// recorder tracks request starts and concurrency per host inside a synctest bubble.
type recorder struct {
	mu       sync.Mutex
	starts   map[string][]time.Duration
	inFlight map[string]int
	peak     map[string]int
	epoch    time.Time
}

func newRecorder() *recorder {
	return &recorder{
		starts:   map[string][]time.Duration{},
		inFlight: map[string]int{},
		peak:     map[string]int{},
		epoch:    time.Now(),
	}
}

func (r *recorder) begin(host string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts[host] = append(r.starts[host], time.Since(r.epoch))
	r.inFlight[host]++
	r.peak[host] = max(r.peak[host], r.inFlight[host])
}

func (r *recorder) end(host string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inFlight[host]--
}

func (r *recorder) check(t *testing.T, host string, n, parallel int, spacing time.Duration) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	starts := slices.Sorted(slices.Values(r.starts[host]))
	if len(starts) != n {
		t.Fatalf("%s: %d requests started, want %d", host, len(starts), n)
	}
	if r.peak[host] > parallel {
		t.Errorf("%s: peak concurrency %d, want <= %d", host, r.peak[host], parallel)
	}
	for i := 1; i < len(starts); i++ {
		if gap := starts[i] - starts[i-1]; gap < spacing {
			t.Errorf("%s: starts %v and %v are %v apart, want >= %v", host, starts[i-1], starts[i], gap, spacing)
		}
	}
}

func TestThrottleLimits(t *testing.T) {
	tests := []struct {
		name     string
		parallel int
		spacing  time.Duration
		hold     time.Duration
		n        int
		// wantStarts are exact because the synctest clock is deterministic
		wantStarts []time.Duration
	}{
		{
			name: "spacing dominates short requests", parallel: 2, spacing: 500 * time.Millisecond,
			hold: 100 * time.Millisecond, n: 4,
			wantStarts: []time.Duration{0, 500 * time.Millisecond, time.Second, 1500 * time.Millisecond},
		},
		{
			name: "parallelism dominates long requests", parallel: 2, spacing: 500 * time.Millisecond,
			hold: 3 * time.Second, n: 4,
			wantStarts: []time.Duration{0, 500 * time.Millisecond, 3 * time.Second, 3500 * time.Millisecond},
		},
		{
			name: "no spacing", parallel: 3, spacing: 0, hold: time.Second, n: 5,
			wantStarts: []time.Duration{0, 0, 0, time.Second, time.Second},
		},
		{
			name: "serial", parallel: 1, spacing: 200 * time.Millisecond, hold: time.Second, n: 3,
			wantStarts: []time.Duration{0, time.Second, 2 * time.Second},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				th := newThrottle(tt.parallel, tt.spacing)
				rec := newRecorder()
				var wg sync.WaitGroup
				for range tt.n {
					wg.Go(func() {
						release, err := th.acquire(t.Context(), "example.com")
						if err != nil {
							t.Error(err)
							return
						}
						rec.begin("example.com")
						time.Sleep(tt.hold)
						rec.end("example.com")
						release()
					})
				}
				wg.Wait()
				rec.check(t, "example.com", tt.n, tt.parallel, tt.spacing)
				got := slices.Sorted(slices.Values(rec.starts["example.com"]))
				if !slices.Equal(got, tt.wantStarts) {
					t.Errorf("starts = %v, want %v", got, tt.wantStarts)
				}
				assertIdle(t, th)
			})
		})
	}
}

// assertIdle checks that no slot is held or awaited; an entry may remain while its spacing is pending.
func assertIdle(t *testing.T, th *throttle) {
	t.Helper()
	th.mu.Lock()
	defer th.mu.Unlock()
	for host, h := range th.hosts {
		if h.users != 0 || len(h.sem) != 0 {
			t.Errorf("%s: users=%d held=%d after all requests finished", host, h.users, len(h.sem))
		}
	}
}

func TestThrottleHostsAreIndependent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		th := newThrottle(1, time.Second)
		start := time.Now()
		var wg sync.WaitGroup
		for _, host := range []string{"a.example", "b.example", "c.example"} {
			wg.Go(func() {
				release, err := th.acquire(t.Context(), host)
				if err != nil {
					t.Error(err)
					return
				}
				defer release()
				if d := time.Since(start); d != 0 {
					t.Errorf("%s waited %v behind other hosts", host, d)
				}
			})
		}
		wg.Wait()
	})
}

func TestThrottleCancelWhileWaitingForSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		th := newThrottle(1, 0)
		release, err := th.acquire(t.Context(), "example.com")
		if err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error)
		go func() {
			_, err := th.acquire(ctx, "example.com")
			done <- err
		}()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}

		release()
		release() // releasing twice must not free a second slot
		if len(th.hosts) != 0 {
			t.Errorf("host entry leaked after cancel: %d", len(th.hosts))
		}
		assertIdle(t, th)
	})
}

func TestThrottleCancelWhileWaitingForSpacing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const spacing = 10 * time.Second
		th := newThrottle(2, spacing)
		start := time.Now()
		first, err := th.acquire(t.Context(), "example.com")
		if err != nil {
			t.Fatal(err)
		}
		first()

		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if _, err := th.acquire(ctx, "example.com"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", err)
		}
		if d := time.Since(start); d != time.Second {
			t.Errorf("cancelled wait took %v, want 1s", d)
		}

		// the abandoned reservation is handed back, so the next request starts at the original slot
		third, err := th.acquire(t.Context(), "example.com")
		if err != nil {
			t.Fatal(err)
		}
		third()
		if d := time.Since(start); d != spacing {
			t.Errorf("next request started at %v, want %v", d, spacing)
		}
	})
}

func TestThrottleContextAlreadyDone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		th := newThrottle(1, time.Second)
		hold, err := th.acquire(t.Context(), "example.com")
		if err != nil {
			t.Fatal(err)
		}
		defer hold()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := th.acquire(ctx, "example.com"); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
}

func TestThrottleSweepsIdleHosts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		th := newThrottle(1, time.Second)
		for i := range sweepThreshold + 10 {
			release, err := th.acquire(t.Context(), strings.Repeat("h", i+1))
			if err != nil {
				t.Fatal(err)
			}
			release()
		}
		// entries stay while their spacing is pending, then the next acquire prunes them
		time.Sleep(2 * time.Second)
		release, err := th.acquire(t.Context(), "fresh.example")
		if err != nil {
			t.Fatal(err)
		}
		release()
		if n := len(th.hosts); n > 1 {
			t.Errorf("%d host entries after sweep, want <= 1", n)
		}
	})
}

func TestClientThrottlesPerHost(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			host := r.URL.Hostname()
			rec.begin(host)
			defer rec.end(host)
			time.Sleep(50 * time.Millisecond)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader("ok")),
				Request:    r,
			}, nil
		})
		c := NewClient(Options{Transport: rt})

		urls := []string{
			"https://www.youtube.com/feeds/videos.xml?channel_id=1",
			"https://WWW.YOUTUBE.COM/feeds/videos.xml?channel_id=2",
			"https://www.youtube.com/feeds/videos.xml?channel_id=3",
			"https://www.YouTube.com/feeds/videos.xml?channel_id=4",
			"https://www.youtube.com/feeds/videos.xml?channel_id=5",
			"https://example.com/feed",
			"https://example.com/rss",
		}
		var wg sync.WaitGroup
		for _, u := range urls {
			wg.Go(func() {
				if _, err := c.Get(t.Context(), u, nil); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		rec.check(t, "www.youtube.com", 5, defaultHostParallel, defaultHostSpacing)
		rec.check(t, "example.com", 2, defaultHostParallel, defaultHostSpacing)
		if first := slices.Min(rec.starts["example.com"]); first != 0 {
			t.Errorf("example.com waited %v behind youtube", first)
		}
	})
}

func TestClientThrottleRespectsContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		block := make(chan struct{})
		rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			select {
			case <-block:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		})
		c := NewClient(Options{Transport: rt, HostParallel: 1, Timeout: time.Hour})

		var wg sync.WaitGroup
		wg.Go(func() {
			if _, err := c.Get(t.Context(), "https://example.com/a", nil); err != nil {
				t.Error(err)
			}
		})
		synctest.Wait()

		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		_, err := c.Get(ctx, "https://example.com/b", nil)
		if !errors.Is(err, context.DeadlineExceeded) || err.Error() != "request timed out" {
			t.Errorf("err = %v, want timeout while waiting for the host slot", err)
		}
		close(block)
		wg.Wait()
	})
}
