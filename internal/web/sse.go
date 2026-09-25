package web

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/oryanm/stalker/internal/events"
	"github.com/oryanm/stalker/internal/store"
)

// changedKey is the coalescer key of FollowsChanged; follow ids start at 1.
const changedKey int64 = 0

// changedNotice is the payload of follows-changed. Rows are never re-rendered
// live, so the page only offers a reload (handled by app.js).
const changedNotice = `<p class="notice" role="status">Your follows changed. ` +
	`<button type="button" data-reload>Reload</button></p>`

// events streams row summaries as SSE until the client leaves or CloseStreams is called.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	select {
	case <-s.done:
		http.Error(w, "Shutting down", http.StatusServiceUnavailable)
		return
	default:
	}
	rc := http.NewResponseController(w)
	// streams outlive any server WriteTimeout; not every writer supports deadlines
	_ = rc.SetWriteDeadline(time.Time{})

	ch, cancel := s.ev.Subscribe()
	defer cancel()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// subscribed before this comment is sent, so a client that has read it misses nothing
	if _, err := io.WriteString(w, "retry: 5000\n: connected\n\n"); err != nil {
		return
	}
	if err := rc.Flush(); err != nil {
		s.log.Warn("SSE needs a flushing response writer", "err", err)
		return
	}

	ctx := r.Context()
	ping := time.NewTicker(s.pingEvery)
	defer ping.Stop()
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	var due <-chan time.Time
	co := newCoalescer(s.coalesce)

	for {
		var keys []int64
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case e, ok := <-ch:
			if !ok {
				return
			}
			if key, ok := eventKey(e); ok && co.add(key, time.Now()) {
				keys = []int64{key}
			}
		case <-due:
			keys = co.due(time.Now())
		case <-ping.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}

		if len(keys) > 0 {
			for _, key := range keys {
				if err := s.emit(ctx, w, key); err != nil {
					return
				}
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}

		if next, ok := co.next(); ok {
			timer.Reset(time.Until(next))
			due = timer.C
		} else {
			timer.Stop()
			due = nil
		}
	}
}

func eventKey(e events.Event) (int64, bool) {
	switch e.Kind {
	case events.FollowsChanged:
		return changedKey, true
	case events.FollowUpdated, events.FollowFetching:
		return e.FollowID, e.FollowID > 0
	}
	return 0, false
}

// emit writes one event; only write errors (the client is gone) are returned.
func (s *Server) emit(ctx context.Context, w io.Writer, key int64) error {
	if key == changedKey {
		return writeEvent(w, "follows-changed", []byte(changedNotice))
	}
	rw, err := s.loadRow(ctx, key, s.now())
	if errors.Is(err, store.ErrNotFound) {
		// deleted; follows-changed tells the page
		return nil
	}
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("render SSE summary", "follow", key, "err", err)
		}
		return nil
	}
	var buf bytes.Buffer
	if err := s.base.ExecuteTemplate(&buf, "summary", rw); err != nil {
		s.log.Error("render SSE summary", "follow", key, "err", err)
		return nil
	}
	return writeEvent(w, "follow-"+strconv.FormatInt(key, 10), buf.Bytes())
}

// writeEvent frames data as one SSE event, one data: field per line.
func writeEvent(w io.Writer, name string, data []byte) error {
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	data = bytes.ReplaceAll(data, []byte("\r"), []byte("\n"))
	var b bytes.Buffer
	b.Grow(len(data) + 64)
	b.WriteString("event: ")
	b.WriteString(name)
	b.WriteByte('\n')
	for line := range bytes.Lines(bytes.TrimSpace(data)) {
		line = bytes.TrimRight(line, "\n")
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		b.WriteString("data: ")
		b.Write(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	_, err := w.Write(b.Bytes())
	return err
}

// coalescer limits renders to one per key per interval: the first event of a
// burst goes out at once, the rest collapse into one render when the interval
// has passed, which then reads the latest state.
type coalescer struct {
	every   time.Duration
	last    map[int64]time.Time // when each key was last emitted
	pending map[int64]time.Time // keys waiting, with the time they may go
}

func newCoalescer(every time.Duration) *coalescer {
	return &coalescer{every: every, last: map[int64]time.Time{}, pending: map[int64]time.Time{}}
}

// add reports whether key should be emitted now; otherwise it is scheduled.
func (c *coalescer) add(key int64, now time.Time) bool {
	if _, waiting := c.pending[key]; waiting {
		return false
	}
	if last, ok := c.last[key]; ok && now.Sub(last) < c.every {
		c.pending[key] = last.Add(c.every)
		return false
	}
	c.last[key] = now
	return true
}

// due removes and returns the scheduled keys whose time has come, sorted.
func (c *coalescer) due(now time.Time) []int64 {
	var keys []int64
	for key, at := range c.pending {
		if !at.After(now) {
			keys = append(keys, key)
			delete(c.pending, key)
			c.last[key] = now
		}
	}
	// forget keys that are quiet again so the map stays small on long streams
	for key, at := range c.last {
		if _, waiting := c.pending[key]; !waiting && now.Sub(at) >= c.every {
			delete(c.last, key)
		}
	}
	slices.Sort(keys)
	return keys
}

// next is the earliest time a scheduled key may go.
func (c *coalescer) next() (time.Time, bool) {
	var first time.Time
	for _, at := range c.pending {
		if first.IsZero() || at.Before(first) {
			first = at
		}
	}
	return first, !first.IsZero()
}
