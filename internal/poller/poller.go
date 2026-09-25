// Package poller fetches follows on a schedule driven by their importance.
package poller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oryanm/stalker/internal/events"
	"github.com/oryanm/stalker/internal/feed"
	"github.com/oryanm/stalker/internal/model"
	"github.com/oryanm/stalker/internal/store"
)

const (
	defaultWorkers = 6
	defaultTick    = 5 * time.Second

	// fetchTimeout bounds one fetch, including the wait for a per-host throttle slot.
	fetchTimeout = 60 * time.Second
	// recordTimeout bounds storing a result, which also happens after the run context ends.
	recordTimeout = 10 * time.Second
	// maxBackoff caps the error backoff unless the tier's interval is longer.
	maxBackoff = 24 * time.Hour
	// maxDoublings caps the exponent of the error backoff.
	maxDoublings = 6
	// maxErrorRunes keeps a stored error short enough for a tooltip.
	maxErrorRunes = 300
)

type Options struct {
	Workers int              // concurrent fetches, default 6
	Tick    time.Duration    // how often due follows are checked, default 5s
	Now     func() time.Time // default time.Now
	Jitter  func() float64   // returns [0,1), default math/rand/v2
}

// Poller fetches due follows in the background (Run) and on demand
// (FetchNow, CheckAll). Safe for concurrent use.
type Poller struct {
	st      *store.Store
	fc      *feed.Client
	ev      *events.Broker
	workers int
	tick    time.Duration
	now     func() time.Time
	jitter  func() float64

	fetchTimeout time.Duration
	// record stores a result; tests swap it to simulate write failures
	record func(context.Context, store.FetchResult) error

	kick    chan struct{}
	running atomic.Bool

	mu       sync.Mutex
	inflight map[int64]*call
	// held maps follows whose last result could not be stored to the time Run
	// may fetch them again, so a failing database does not cause a fetch storm
	held map[int64]time.Time
}

// call is one in-flight fetch, shared by everyone asking for the same follow.
type call struct {
	done      chan struct{}
	err       error // set before done is closed
	abandoned bool  // the leader's context ended mid-fetch and nothing was recorded
}

// New returns a Poller. A nil fc gets a default feed.Client; a nil ev
// disables events.
func New(st *store.Store, fc *feed.Client, ev *events.Broker, opts Options) *Poller {
	if fc == nil {
		fc = feed.NewClient(feed.Options{})
	}
	if opts.Workers <= 0 {
		opts.Workers = defaultWorkers
	}
	if opts.Tick <= 0 {
		opts.Tick = defaultTick
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Jitter == nil {
		opts.Jitter = rand.Float64
	}
	return &Poller{
		st:           st,
		fc:           fc,
		ev:           ev,
		workers:      opts.Workers,
		tick:         opts.Tick,
		now:          opts.Now,
		jitter:       opts.Jitter,
		fetchTimeout: fetchTimeout,
		record:       st.RecordFetch,
		kick:         make(chan struct{}, 1),
		inflight:     make(map[int64]*call),
		held:         make(map[int64]time.Time),
	}
}

// Run schedules fetches until ctx is cancelled, then waits for in-flight
// fetches to finish. A follow is never fetched twice concurrently.
//
// Cancelling ctx aborts in-flight fetches; results that already arrived are
// still recorded. Run returns nil once stopped, or an error straight away when
// it is already running.
func (p *Poller) Run(ctx context.Context) error {
	if !p.running.CompareAndSwap(false, true) {
		return errors.New("poller: already running")
	}
	defer p.running.Store(false)

	ticker := time.NewTicker(p.tick)
	defer ticker.Stop()
	slots := make(chan struct{}, p.workers)
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		p.schedule(ctx, slots, &wg)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-p.kick:
		}
	}
}

// schedule starts fetches of due follows while worker slots are free.
func (p *Poller) schedule(ctx context.Context, slots chan struct{}, wg *sync.WaitGroup) {
	free := cap(slots) - len(slots)
	if free == 0 || ctx.Err() != nil {
		return
	}
	now := p.now()
	// in-flight and held follows are still due in the database, so ask for enough to fill every slot anyway
	due, err := p.st.DueFollows(ctx, now, free+p.skippable(now))
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("poller: query due follows", "err", err)
		}
		return
	}
	for _, f := range due {
		if free == 0 {
			break
		}
		c := p.claim(f.ID, now)
		if c == nil {
			continue
		}
		// never blocks: only workers take tokens out, so at least free slots are open
		slots <- struct{}{}
		free--
		wg.Go(func() {
			o := p.lead(ctx, c, f.ID, now)
			<-slots
			if o == recorded {
				// refill the slot now rather than on the next tick; failures wait for the tick
				p.Kick()
			}
		})
	}
}

// Kick makes Run check for due follows immediately (after imports and adds).
func (p *Poller) Kick() {
	select {
	case p.kick <- struct{}{}:
	default:
		// a check is already pending
	}
}

// FetchNow fetches one follow synchronously, bypassing its schedule, and
// returns the fetch error (the result is recorded either way). If the follow
// is already being fetched it waits for that fetch instead.
//
// When ctx ends before the fetch completes, the fetch is abandoned without
// recording anything and ctx's error is returned. The returned error's message
// is the one stored as the follow's LastError. An unknown id returns
// store.ErrNotFound.
func (p *Poller) FetchNow(ctx context.Context, id int64) error {
	for {
		c, leader := p.join(id)
		if leader {
			p.lead(ctx, c, id, time.Time{})
			return c.err
		}
		select {
		case <-c.done:
			// the leader's caller gave up, but this caller still wants the fetch
			if c.abandoned && ctx.Err() == nil {
				continue
			}
			return c.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// IsFetching reports whether a fetch of the follow is in flight.
func (p *Poller) IsFetching(id int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.inflight[id]
	return ok
}

// claim registers a scheduled fetch of id, or returns nil when the follow is
// already in flight or held back after a failed write.
func (p *Poller) claim(id int64, now time.Time) *call {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.inflight[id]; ok {
		return nil
	}
	if until, ok := p.held[id]; ok {
		if now.Before(until) {
			return nil
		}
		delete(p.held, id)
	}
	c := &call{done: make(chan struct{})}
	p.inflight[id] = c
	return c
}

// join returns the in-flight call for id, or registers one the caller must lead.
func (p *Poller) join(id int64) (c *call, leader bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.inflight[id]; ok {
		return c, false
	}
	c = &call{done: make(chan struct{})}
	p.inflight[id] = c
	return c, true
}

// skippable counts the follows schedule may find due but must pass over.
func (p *Poller) skippable(now time.Time) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, until := range p.held {
		if !now.Before(until) {
			delete(p.held, id)
		}
	}
	return len(p.inflight) + len(p.held)
}

func (p *Poller) setHeld(id int64, until time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if until.IsZero() {
		delete(p.held, id)
	} else {
		p.held[id] = until
	}
}

// outcome says what became of a fetch.
type outcome int

const (
	skipped    outcome = iota // nothing was fetched
	recorded                  // the result, success or failure, was stored
	unrecorded                // the result could not be stored
	abandoned                 // the context ended mid-fetch, nothing was stored
	stale                     // the feed URL or tier was edited mid-fetch, so the result was discarded
)

// lead performs c, which the caller registered, and releases it. A non-zero
// dueBy skips follows that are no longer due by then: a FetchNow may have
// fetched them since the due query.
func (p *Poller) lead(ctx context.Context, c *call, id int64, dueBy time.Time) (o outcome) {
	started := false
	defer func() {
		c.abandoned = o == abandoned
		p.release(id, c, started)
	}()

	f, err := p.st.GetFollow(ctx, id)
	switch {
	case err == nil:
	case ctx.Err() != nil:
		c.err = err
		return abandoned
	default:
		if !errors.Is(err, store.ErrNotFound) {
			slog.Error("poller: load follow", "follow", id, "err", err)
		}
		c.err = err
		return skipped
	}
	if !dueBy.IsZero() && f.NextFetchAt.After(dueBy) {
		return skipped
	}
	started = true
	p.ev.Publish(events.Event{Kind: events.FollowFetching, FollowID: id})
	o, c.err = p.fetchOne(ctx, f)
	if o != stale {
		return o
	}
	// fetch again as edited: the edit's Kick found this follow in flight and passed it over
	if f, err = p.st.GetFollow(ctx, id); err != nil {
		c.err = err
		if ctx.Err() != nil {
			return abandoned
		}
		return unrecorded
	}
	o, c.err = p.fetchOne(ctx, f)
	if o == stale {
		// edited yet again; the store has scheduled the follow for now, so Run picks it up
		o = unrecorded
	}
	return o
}

// release unregisters c and wakes its waiters.
func (p *Poller) release(id int64, c *call, started bool) {
	p.mu.Lock()
	delete(p.inflight, id)
	p.mu.Unlock()
	close(c.done)
	if started {
		// published after the delete so a row rendered for it no longer shows a fetch in progress
		p.ev.Publish(events.Event{Kind: events.FollowUpdated, FollowID: id})
	}
}

// fetchOne fetches f and records the result.
func (p *Poller) fetchOne(ctx context.Context, f model.Follow) (outcome, error) {
	log := slog.With("follow", f.ID, "feed", f.FeedURL)
	start := time.Now()
	fctx, cancel := context.WithTimeout(ctx, p.fetchTimeout)
	res, fetchErr := p.fc.Fetch(fctx, f.FeedURL, f.ETag, f.LastModified)
	cancel()
	took := time.Since(start).Round(time.Millisecond)
	if ctx.Err() != nil && isContextErr(fetchErr) {
		// shutdown or a caller giving up says nothing about the feed
		log.Debug("fetch abandoned", "took", took)
		return abandoned, ctx.Err()
	}

	r := fetchResult(f, res, fetchErr, p.now(), p.jitter())
	// a result that arrived just as ctx ended is still worth keeping
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	recordErr := p.record(rctx, r)
	rcancel()

	switch {
	case fetchErr != nil:
		log.Warn("fetch failed", "err", fetchErr, "errors", f.ErrorCount+1, "next", r.NextFetchAt, "took", took)
		fetchErr = &fetchError{msg: r.Err, err: fetchErr}
	case res.NotModified:
		log.Debug("feed not modified", "next", r.NextFetchAt, "took", took)
	default:
		log.Debug("feed fetched", "posts", len(res.Posts), "next", r.NextFetchAt, "took", took)
	}

	switch {
	case errors.Is(recordErr, store.ErrNotFound):
		log.Debug("follow deleted during fetch")
		return unrecorded, store.ErrNotFound
	case errors.Is(recordErr, store.ErrStale):
		log.Debug("feed URL or tier changed during fetch, result discarded")
		return stale, store.ErrStale
	case recordErr != nil:
		log.Error("poller: record fetch", "err", recordErr)
		p.setHeld(f.ID, r.NextFetchAt)
		return unrecorded, errors.Join(fetchErr, recordErr)
	}
	p.setHeld(f.ID, time.Time{})
	return recorded, fetchErr
}

func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// fetchError keeps the underlying error reachable while its message matches
// the stored LastError.
type fetchError struct {
	msg string
	err error
}

func (e *fetchError) Error() string { return e.msg }
func (e *fetchError) Unwrap() error { return e.err }

// fetchResult turns the outcome of a fetch at now into what RecordFetch stores.
func fetchResult(f model.Follow, res *feed.Result, err error, now time.Time, jitter float64) store.FetchResult {
	r := store.FetchResult{FollowID: f.ID, FeedURL: f.FeedURL, Importance: f.Importance, FetchedAt: now}
	if err != nil {
		var retryAfter time.Duration
		if he, ok := errors.AsType[*feed.HTTPError](err); ok {
			retryAfter = he.RetryAfter
		}
		r.Err = errorMessage(err)
		r.NextFetchAt = NextFetch(now, f.Importance, f.ErrorCount+1, retryAfter, jitter)
		return r
	}
	r.NextFetchAt = NextFetch(now, f.Importance, 0, 0, jitter)
	r.ETag = res.ETag
	r.LastModified = res.LastModified
	if res.NotModified {
		r.NotModified = true
		return r
	}
	r.FeedTitle = res.Title
	r.Description = res.Description
	r.SiteURL = res.SiteURL
	r.PhotoURL = res.ImageURL
	r.Posts = res.Posts
	return r
}

// errorMessage describes a fetch failure for the UI. It is never empty.
func errorMessage(err error) string {
	if he, ok := errors.AsType[*feed.HTTPError](err); ok {
		if text := http.StatusText(he.StatusCode); text != "" {
			return fmt.Sprintf("HTTP %d %s", he.StatusCode, text)
		}
		return fmt.Sprintf("HTTP %d", he.StatusCode)
	}
	msg := strings.Join(strings.Fields(err.Error()), " ")
	if errors.Is(err, feed.ErrNotAFeed) {
		// the parser's detail goes to the log; the UI only needs the verdict and where it came from
		msg = feed.ErrNotAFeed.Error()
		if re, ok := errors.AsType[*feed.RedirectError](err); ok {
			msg += " (redirected to " + re.URL + ")"
		}
	}
	if msg == "" {
		return "fetch failed"
	}
	if r := []rune(msg); len(r) > maxErrorRunes {
		msg = string(r[:maxErrorRunes-1]) + "…"
	}
	return msg
}

// Interval is the base polling interval for an importance: Realtime 10m,
// Frequent 1h, Occasional 4h, Sometime 12h, Rarely 24h.
func Interval(imp model.Importance) time.Duration {
	switch model.NormalizeImportance(int(imp)) {
	case model.Realtime:
		return 10 * time.Minute
	case model.Frequent:
		return time.Hour
	case model.Occasional:
		return 4 * time.Hour
	case model.Sometime:
		return 12 * time.Hour
	default:
		return 24 * time.Hour
	}
}

// NextFetch schedules the next fetch. Success: now + Interval * (1 + 0.5*jitter).
// After errorCount consecutive failures: Interval * 2^min(errorCount-1, 6),
// capped at 24h (or Interval when larger), plus the same jitter, and never
// sooner than retryAfter.
//
// Jitter outside [0,1] is clamped.
func NextFetch(now time.Time, imp model.Importance, errorCount int, retryAfter time.Duration, jitter float64) time.Time {
	base := Interval(imp)
	d := base
	if errorCount > 0 {
		d = min(base<<min(errorCount-1, maxDoublings), max(maxBackoff, base))
	}
	// the negated comparison also catches NaN
	if !(jitter > 0) {
		jitter = 0
	}
	jitter = min(jitter, 1)
	d += time.Duration(float64(d) * 0.5 * jitter)
	return now.Add(max(d, retryAfter))
}

// CheckResult is one follow's outcome from CheckAll.
type CheckResult struct {
	Follow   model.Follow
	Posts    int // posts stored after the fetch
	Err      error
	Duration time.Duration
}

// CheckAll fetches every follow once (ignoring schedules) with the given
// concurrency, records results like normal polling, and reports each outcome.
// Used by `stalker check`.
//
// Results are sorted by display title and hold each follow as stored after
// its fetch. A concurrency below 1 means Options.Workers. When ctx ends early
// the follows not fetched carry ctx's error, which is also returned.
func (p *Poller) CheckAll(ctx context.Context, concurrency int) ([]CheckResult, error) {
	follows, err := p.st.ListFollows(ctx)
	if err != nil {
		return nil, fmt.Errorf("poller: check all: %w", err)
	}
	if concurrency < 1 {
		concurrency = p.workers
	}
	results := make([]CheckResult, len(follows))
	slots := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, f := range follows {
		results[i].Follow = f
		if ctx.Err() != nil {
			results[i].Err = ctx.Err()
			continue
		}
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			results[i].Err = ctx.Err()
			continue
		}
		wg.Go(func() {
			defer func() { <-slots }()
			start := time.Now()
			results[i].Err = p.FetchNow(ctx, f.ID)
			results[i].Duration = time.Since(start)
			p.reload(ctx, &results[i])
		})
	}
	wg.Wait()

	slices.SortStableFunc(results, func(a, b CheckResult) int {
		return cmp.Or(
			strings.Compare(strings.ToLower(a.Follow.DisplayTitle()), strings.ToLower(b.Follow.DisplayTitle())),
			cmp.Compare(a.Follow.ID, b.Follow.ID),
		)
	})
	return results, ctx.Err()
}

// reload refreshes a check result from the store, even after ctx ended, so an
// interrupted check still reports what was stored.
func (p *Poller) reload(ctx context.Context, r *CheckResult) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if f, err := p.st.GetFollow(ctx, r.Follow.ID); err == nil {
		r.Follow = f
	}
	if posts, err := p.st.RecentPosts(ctx, r.Follow.ID, math.MaxInt32); err == nil {
		r.Posts = len(posts)
	}
}
