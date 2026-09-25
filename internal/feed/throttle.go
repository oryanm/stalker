package feed

import (
	"context"
	"sync"
	"time"
)

// sweepThreshold is the host count above which idle entries are pruned on acquire.
const sweepThreshold = 256

// throttle limits concurrency and start spacing per host.
type throttle struct {
	parallel int
	spacing  time.Duration

	mu    sync.Mutex
	hosts map[string]*hostSlot
}

type hostSlot struct {
	sem   chan struct{} // one token per in-flight request
	next  time.Time     // earliest start of the next request, guarded by throttle.mu
	users int           // holders plus waiters, guarded by throttle.mu
}

func newThrottle(parallel int, spacing time.Duration) *throttle {
	return &throttle{
		parallel: max(parallel, 1),
		spacing:  max(spacing, 0),
		hosts:    make(map[string]*hostSlot),
	}
}

// acquire blocks until a request to host may start, or ctx is done. The
// returned release func must be called once the request has finished.
func (t *throttle) acquire(ctx context.Context, host string) (release func(), err error) {
	// select picks randomly among ready cases, so a free slot could otherwise win over a done ctx
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h := t.join(host)

	select {
	case h.sem <- struct{}{}:
	case <-ctx.Done():
		t.leave(host, h)
		return nil, ctx.Err()
	}

	if t.spacing > 0 {
		t.mu.Lock()
		now := time.Now()
		start := h.next
		if start.Before(now) {
			start = now
		}
		h.next = start.Add(t.spacing)
		t.mu.Unlock()

		if wait := start.Sub(now); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				t.mu.Lock()
				// give the reserved start back unless a later request already queued behind it
				if h.next.Equal(start.Add(t.spacing)) {
					h.next = start
				}
				t.mu.Unlock()
				<-h.sem
				t.leave(host, h)
				return nil, ctx.Err()
			}
		}
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			<-h.sem
			t.leave(host, h)
		})
	}, nil
}

func (t *throttle) join(host string) *hostSlot {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.hosts) > sweepThreshold {
		now := time.Now()
		for k, h := range t.hosts {
			if h.users == 0 && !now.Before(h.next) {
				delete(t.hosts, k)
			}
		}
	}
	h := t.hosts[host]
	if h == nil {
		h = &hostSlot{sem: make(chan struct{}, t.parallel)}
		t.hosts[host] = h
	}
	h.users++
	return h
}

func (t *throttle) leave(host string, h *hostSlot) {
	t.mu.Lock()
	defer t.mu.Unlock()
	h.users--
	// an entry is only dropped once its spacing has elapsed, so dropping never shortens a gap
	if h.users == 0 && !time.Now().Before(h.next) && t.hosts[host] == h {
		delete(t.hosts, host)
	}
}
