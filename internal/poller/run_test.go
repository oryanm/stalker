package poller

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oryanm/stalker/internal/events"
	"github.com/oryanm/stalker/internal/model"
	"github.com/oryanm/stalker/internal/store"
)

// These tests run in synctest bubbles: the clock only moves when every
// goroutine is blocked, so tick-driven behaviour is exact. Feeds are served
// in-process by site.RoundTrip on the host feeds.test.

const feeds = "http://feeds.test"

func TestRunFetchesDueFollows(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := openStore(t)
		s := newSite()
		p, ev := newPoller(st, s, Options{Tick: time.Minute})
		sub, cancel := ev.Subscribe()
		defer cancel()
		now := time.Now()
		a := addFollow(t, st, model.Follow{FeedURL: feeds + "/a", Importance: model.Frequent, NextFetchAt: now.Add(-time.Minute)})
		b := addFollow(t, st, model.Follow{FeedURL: feeds + "/b", Importance: model.Frequent, NextFetchAt: now})
		c := addFollow(t, st, model.Follow{FeedURL: feeds + "/c", Importance: model.Frequent, NextFetchAt: now.Add(30 * time.Minute)})

		startRun(t, p)
		synctest.Wait()
		assertHits(t, s, map[string]int{"/a": 1, "/b": 1, "/c": 0})
		kinds := map[int64][]events.Kind{}
		for _, e := range drain(sub) {
			kinds[e.FollowID] = append(kinds[e.FollowID], e.Kind)
		}
		for _, id := range []int64{a.ID, b.ID} {
			if want := []events.Kind{events.FollowFetching, events.FollowUpdated}; !slices.Equal(kinds[id], want) {
				t.Errorf("events for follow %d = %v, want %v", id, kinds[id], want)
			}
		}
		if len(kinds[c.ID]) != 0 {
			t.Errorf("events for a follow that is not due: %v", kinds[c.ID])
		}
		if got := getFollow(t, st, a.ID); got.FeedTitle != "a" || !got.NextFetchAt.Equal(now.Add(time.Hour)) {
			t.Errorf("follow a after its fetch: title %q, next fetch %v", got.FeedTitle, got.NextFetchAt)
		}

		time.Sleep(30*time.Minute + time.Second)
		synctest.Wait()
		assertHits(t, s, map[string]int{"/a": 1, "/b": 1, "/c": 1})

		// a and b were fetched at the start, so without jitter they are due an hour later
		time.Sleep(30 * time.Minute)
		synctest.Wait()
		assertHits(t, s, map[string]int{"/a": 2, "/b": 2, "/c": 1})
	})
}

func TestRunWorkerLimitAndRefill(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := openStore(t)
		s := newSite()
		// an hour-long tick means every fetch after the first round comes from refilling freed slots
		p, _ := newPoller(st, s, Options{Workers: 2, Tick: time.Hour})
		var gates []chan struct{}
		for _, name := range []string{"a", "b", "c", "d", "e"} {
			gates = append(gates, s.gate("/"+name))
			addFollow(t, st, model.Follow{FeedURL: feeds + "/" + name})
		}

		startRun(t, p)
		synctest.Wait()
		if active, _ := s.concurrency(); active != 2 || s.totalHits() != 2 {
			t.Errorf("%d fetches active, %d started; want 2 and 2", active, s.totalHits())
		}
		for _, g := range gates {
			close(g)
		}
		synctest.Wait()
		assertHits(t, s, map[string]int{"/a": 1, "/b": 1, "/c": 1, "/d": 1, "/e": 1})
		if _, maxActive := s.concurrency(); maxActive != 2 {
			t.Errorf("up to %d fetches ran at once, want 2", maxActive)
		}
	})
}

func TestRunSkipsFollowsInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := openStore(t)
		s := newSite()
		p, _ := newPoller(st, s, Options{Workers: 2, Tick: 5 * time.Second})
		now := time.Now()
		gateA, gateB := s.gate("/a"), s.gate("/b")
		a := addFollow(t, st, model.Follow{FeedURL: feeds + "/a", NextFetchAt: now.Add(-2 * time.Minute)})
		addFollow(t, st, model.Follow{FeedURL: feeds + "/b", NextFetchAt: now.Add(-time.Minute)})
		addFollow(t, st, model.Follow{FeedURL: feeds + "/c", NextFetchAt: now})
		addFollow(t, st, model.Follow{FeedURL: feeds + "/d", NextFetchAt: now})

		startRun(t, p)
		synctest.Wait()
		assertHits(t, s, map[string]int{"/a": 1, "/b": 1, "/c": 0, "/d": 0})

		// a is still in flight and still the most overdue, yet the freed slot goes to c, then d
		close(gateB)
		synctest.Wait()
		assertHits(t, s, map[string]int{"/a": 1, "/b": 1, "/c": 1, "/d": 1})

		time.Sleep(15 * time.Second)
		synctest.Wait()
		if !p.IsFetching(a.ID) || s.hitCount("/a") != 1 {
			t.Fatalf("a: fetching %v, %d requests; want one fetch still in flight", p.IsFetching(a.ID), s.hitCount("/a"))
		}

		// FetchNow waits for the scheduled fetch instead of starting another
		errc := make(chan error, 1)
		go func() { errc <- p.FetchNow(context.Background(), a.ID) }()
		synctest.Wait()
		close(gateA)
		if err := <-errc; err != nil {
			t.Errorf("FetchNow: %v", err)
		}
		if n := s.hitCount("/a"); n != 1 {
			t.Errorf("a was requested %d times, want 1", n)
		}
	})
}

func TestRunKick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := openStore(t)
		s := newSite()
		p, _ := newPoller(st, s, Options{Tick: time.Hour})
		startRun(t, p)
		synctest.Wait()

		addFollow(t, st, model.Follow{FeedURL: feeds + "/a"})
		synctest.Wait()
		if n := s.totalHits(); n != 0 {
			t.Fatalf("%d fetches before the kick", n)
		}
		p.Kick()
		synctest.Wait()
		assertHits(t, s, map[string]int{"/a": 1})
	})
}

func TestKickNeverBlocks(t *testing.T) {
	p, _ := newPoller(openStore(t), newSite(), Options{})
	for range 100 {
		p.Kick() // nothing drains the channel without Run
	}
}

func TestRunAlreadyRunning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, _ := newPoller(openStore(t), newSite(), Options{})
		startRun(t, p)
		synctest.Wait()
		if err := p.Run(context.Background()); err == nil {
			t.Error("second concurrent Run returned nil")
		}
	})
}

func TestRunBacksOffFailingFollows(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := openStore(t)
		s := newSite()
		s.handle = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }
		p, _ := newPoller(st, s, Options{Tick: time.Minute})
		f := addFollow(t, st, model.Follow{FeedURL: feeds + "/a", Importance: model.Realtime})
		startRun(t, p)

		// failures at 0, 10m and 30m: the gap doubles from the Realtime interval
		for _, step := range []struct {
			sleep time.Duration
			hits  int
		}{
			{0, 1},
			{9*time.Minute + 50*time.Second, 1},
			{20 * time.Second, 2},
			{19 * time.Minute, 2},
			{time.Minute, 3},
		} {
			time.Sleep(step.sleep)
			synctest.Wait()
			if n := s.hitCount("/a"); n != step.hits {
				t.Fatalf("after %v: %d requests, want %d", step.sleep, n, step.hits)
			}
		}
		if got := getFollow(t, st, f.ID); got.ErrorCount != 3 || got.LastError != "HTTP 500 Internal Server Error" {
			t.Errorf("error state = %q/%d", got.LastError, got.ErrorCount)
		}
	})
}

func TestRunShutdown(t *testing.T) {
	t.Run("abandons fetches in flight", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			st := openStore(t)
			s := newSite()
			s.gate("/a") // never opened: only cancellation ends the request
			p, ev := newPoller(st, s, Options{})
			sub, cancelSub := ev.Subscribe()
			defer cancelSub()
			f := addFollow(t, st, model.Follow{FeedURL: feeds + "/a"})

			stop := startRun(t, p)
			synctest.Wait()
			if !p.IsFetching(f.ID) {
				t.Fatal("fetch not in flight")
			}
			stop()
			if p.IsFetching(f.ID) {
				t.Error("fetch still in flight after Run returned")
			}
			got := getFollow(t, st, f.ID)
			if !got.LastFetchedAt.IsZero() || got.ErrorCount != 0 {
				t.Errorf("an abandoned fetch was recorded: fetched at %v, %d errors", got.LastFetchedAt, got.ErrorCount)
			}
			want := []events.Kind{events.FollowFetching, events.FollowUpdated}
			var kinds []events.Kind
			for _, e := range drain(sub) {
				kinds = append(kinds, e.Kind)
			}
			if !slices.Equal(kinds, want) {
				t.Errorf("events = %v, want %v so the UI clears its spinner", kinds, want)
			}
		})
	})

	t.Run("records results that arrive while stopping", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			st := openStore(t)
			s := newSite()
			s.ignoreCancel = true
			gate := s.gate("/a")
			p, _ := newPoller(st, s, Options{})
			f := addFollow(t, st, model.Follow{FeedURL: feeds + "/a"})

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- p.Run(ctx) }()
			synctest.Wait()
			cancel()
			synctest.Wait()
			select {
			case <-done:
				t.Fatal("Run returned before its fetch finished")
			default:
			}
			close(gate)
			if err := <-done; err != nil {
				t.Fatalf("Run: %v", err)
			}
			got := getFollow(t, st, f.ID)
			if !got.LastFetchedAt.Equal(time.Now()) || got.FeedTitle != "a" || postCount(t, st, f.ID) != postsPerFeed {
				t.Errorf("result lost on shutdown: fetched at %v, title %q", got.LastFetchedAt, got.FeedTitle)
			}
		})
	})
}

func TestRunHoldsFollowsWhoseResultWasNotStored(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := openStore(t)
		s := newSite()
		p, _ := newPoller(st, s, Options{Tick: 5 * time.Second})
		var failing atomic.Bool
		failing.Store(true)
		p.record = func(ctx context.Context, r store.FetchResult) error {
			if failing.Load() {
				return errors.New("disk full")
			}
			return st.RecordFetch(ctx, r)
		}
		f := addFollow(t, st, model.Follow{FeedURL: feeds + "/a", Importance: model.Realtime})

		startRun(t, p)
		synctest.Wait()
		// the follow is still due in the database, but Run leaves it alone until its intended next fetch
		time.Sleep(time.Minute)
		synctest.Wait()
		if n := s.hitCount("/a"); n != 1 {
			t.Fatalf("%d requests in the first minute, want 1", n)
		}

		// an explicit fetch bypasses the hold and reports the write failure
		if err := p.FetchNow(context.Background(), f.ID); err == nil || !strings.Contains(err.Error(), "disk full") {
			t.Errorf("FetchNow = %v, want the write error", err)
		}
		failing.Store(false)
		time.Sleep(10*time.Minute + 10*time.Second)
		synctest.Wait()
		if n := s.hitCount("/a"); n != 3 {
			t.Errorf("%d requests, want 3", n)
		}
		if got := getFollow(t, st, f.ID); got.LastFetchedAt.IsZero() || len(p.held) != 0 {
			t.Errorf("recovery not recorded: fetched at %v, held %v", got.LastFetchedAt, p.held)
		}
	})
}

func TestFetchNowSingleflight(t *testing.T) {
	tests := []struct {
		name    string
		handle  http.HandlerFunc
		wantErr string
	}{
		{"success", nil, ""},
		{"failure", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusGone) }, "HTTP 410 Gone"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				st := openStore(t)
				s := newSite()
				s.handle = tt.handle
				gate := s.gate("/a")
				p, _ := newPoller(st, s, Options{})
				f := addFollow(t, st, model.Follow{FeedURL: feeds + "/a"})

				var wg sync.WaitGroup
				errs := make([]error, 3)
				for i := range errs {
					wg.Go(func() { errs[i] = p.FetchNow(context.Background(), f.ID) })
					synctest.Wait()
				}
				if !p.IsFetching(f.ID) {
					t.Error("IsFetching = false during the fetch")
				}
				close(gate)
				wg.Wait()

				if n := s.hitCount("/a"); n != 1 {
					t.Errorf("%d requests for 3 concurrent FetchNow calls, want 1", n)
				}
				for i, err := range errs {
					got := ""
					if err != nil {
						got = err.Error()
					}
					if got != tt.wantErr {
						t.Errorf("FetchNow #%d = %q, want %q", i, got, tt.wantErr)
					}
				}
				if p.IsFetching(f.ID) {
					t.Error("IsFetching = true after the fetch")
				}
			})
		})
	}
}

func TestFetchNowWaiterTakesOverWhenLeaderGivesUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := openStore(t)
		s := newSite()
		var requests atomic.Int32
		s.handle = func(w http.ResponseWriter, r *http.Request) {
			if requests.Add(1) == 1 {
				<-r.Context().Done()
				return
			}
			writeFeed(w, r)
		}
		p, _ := newPoller(st, s, Options{})
		f := addFollow(t, st, model.Follow{FeedURL: feeds + "/a"})

		leaderCtx, cancelLeader := context.WithCancel(context.Background())
		leader, waiter := make(chan error, 1), make(chan error, 1)
		go func() { leader <- p.FetchNow(leaderCtx, f.ID) }()
		synctest.Wait()
		go func() { waiter <- p.FetchNow(context.Background(), f.ID) }()
		synctest.Wait()
		cancelLeader()

		if err := <-leader; !errors.Is(err, context.Canceled) {
			t.Errorf("leader FetchNow = %v, want context.Canceled", err)
		}
		if err := <-waiter; err != nil {
			t.Errorf("waiter FetchNow = %v, want a fetch of its own", err)
		}
		if n := requests.Load(); n != 2 {
			t.Errorf("%d requests, want 2", n)
		}
		if got := getFollow(t, st, f.ID); got.ErrorCount != 0 || got.FeedTitle != "a" {
			t.Errorf("the abandoned attempt was recorded: %d errors, title %q", got.ErrorCount, got.FeedTitle)
		}
	})
}

func TestFetchNowCallerCancelWhileWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := openStore(t)
		s := newSite()
		gate := s.gate("/a")
		p, _ := newPoller(st, s, Options{})
		f := addFollow(t, st, model.Follow{FeedURL: feeds + "/a"})

		leader := make(chan error, 1)
		go func() { leader <- p.FetchNow(context.Background(), f.ID) }()
		synctest.Wait()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := p.FetchNow(ctx, f.ID); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("waiting FetchNow = %v, want its own deadline", err)
		}
		close(gate)
		if err := <-leader; err != nil {
			t.Errorf("leader FetchNow = %v", err)
		}
	})
}

func assertHits(t *testing.T, s *site, want map[string]int) {
	t.Helper()
	for path, n := range want {
		if got := s.hitCount(path); got != n {
			t.Errorf("%s requested %d times, want %d", path, got, n)
		}
	}
}

func TestScheduledFetchSkipsFollowsNoLongerDue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := openStore(t)
		s := newSite()
		p, ev := newPoller(st, s, Options{})
		sub, cancel := ev.Subscribe()
		defer cancel()
		dueQueryAt := time.Now()
		// fetched by someone else between the due query and the claim
		f := addFollow(t, st, model.Follow{FeedURL: feeds + "/a", NextFetchAt: dueQueryAt.Add(time.Hour)})

		c := p.claim(f.ID, dueQueryAt)
		if o := p.lead(context.Background(), c, f.ID, dueQueryAt); o != skipped {
			t.Errorf("lead = %v, want skipped", o)
		}
		if n := s.totalHits(); n != 0 || p.IsFetching(f.ID) {
			t.Errorf("%d requests, fetching %v; want none", n, p.IsFetching(f.ID))
		}
		if got := drain(sub); len(got) != 0 {
			t.Errorf("events for a skipped fetch: %v", got)
		}
	})
}
