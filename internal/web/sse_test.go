package web

import (
	"bufio"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oryanm/stalker/internal/events"
	"github.com/oryanm/stalker/internal/model"
)

// sseEvent is one parsed server-sent event (comments are reported with Name ":").
type sseEvent struct {
	Name string
	Data string
}

// openStream connects to /events and returns parsed events until the stream ends.
func openStream(t *testing.T, e *env) (*http.Response, <-chan sseEvent) {
	t.Helper()
	ts := httptest.NewServer(e.h)
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(testUser, testPass)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	out := make(chan sseEvent, 64)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		var ev sseEvent
		var data []string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if ev.Name != "" || len(data) > 0 {
					ev.Data = strings.Join(data, "\n")
					out <- ev
				}
				ev, data = sseEvent{}, nil
			case strings.HasPrefix(line, ":"):
				out <- sseEvent{Name: ":", Data: strings.TrimSpace(line[1:])}
			case strings.HasPrefix(line, "event: "):
				ev.Name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = append(data, strings.TrimPrefix(line, "data: "))
			}
		}
	}()

	// the handler subscribes before sending this, so later publishes are not lost
	if ev := next(t, out); ev.Name != ":" || ev.Data != "connected" {
		t.Fatalf("first event = %+v, want the connected comment", ev)
	}
	return resp, out
}

func next(t *testing.T, events <-chan sseEvent) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-events:
		if !ok {
			t.Fatal("stream ended")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an event")
	}
	return sseEvent{}
}

// nextNamed skips pings.
func nextNamed(t *testing.T, events <-chan sseEvent) sseEvent {
	t.Helper()
	for {
		if ev := next(t, events); ev.Name != ":" {
			return ev
		}
	}
}

func TestEventsStream(t *testing.T) {
	e := newEnv(t)
	e.srv.pingEvery = time.Hour
	e.srv.coalesce = time.Millisecond
	f := e.createFollow(model.Follow{URL: "https://a.example/", FeedURL: "https://a.example/feed", FeedTitle: "Title <A>"})
	e.recordPosts(f.ID, model.Post{GUID: "1", URL: "https://a.example/1", Title: "Line one", PublishedAt: now.Add(-time.Hour)})

	resp, stream := openStream(t, e)
	for header, want := range map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-cache",
		"X-Accel-Buffering": "no",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}

	e.ev.Publish(events.Event{Kind: events.FollowUpdated, FollowID: f.ID})
	ev := nextNamed(t, stream)
	if ev.Name != "follow-1" {
		t.Fatalf("event = %q, want follow-1", ev.Name)
	}
	if !strings.HasPrefix(ev.Data, `<div class="head age-h">`) || !strings.Contains(ev.Data, "Title &lt;A&gt;</a>") ||
		!strings.Contains(ev.Data, "Line one") || strings.Contains(ev.Data, `sse-swap`) {
		t.Errorf("unexpected summary:\n%s", ev.Data)
	}
	// the payload is exactly what the refresh endpoint swaps in
	refreshed := strings.TrimSpace(e.post("/follows/1/refresh", nil, htmxFragment...).Body.String())
	var nonEmpty []string
	for line := range strings.Lines(refreshed) {
		if line = strings.TrimRight(line, "\n"); strings.TrimSpace(line) != "" {
			nonEmpty = append(nonEmpty, line)
		}
	}
	if ev.Data != strings.Join(nonEmpty, "\n") {
		t.Errorf("SSE payload differs from the refresh fragment:\n%s\n---\n%s", ev.Data, refreshed)
	}

	e.fetcher.setFetching(f.ID, true)
	e.ev.Publish(events.Event{Kind: events.FollowFetching, FollowID: f.ID})
	if ev := nextNamed(t, stream); ev.Name != "follow-1" || !strings.Contains(ev.Data, `class="fetching"`) {
		t.Errorf("fetching event = %+v", ev)
	}

	// unknown follows and zero ids produce nothing; follows-changed carries the notice
	e.ev.Publish(events.Event{Kind: events.FollowUpdated, FollowID: 999})
	e.ev.Publish(events.Event{Kind: events.FollowUpdated})
	e.ev.Publish(events.Event{Kind: events.FollowsChanged})
	ev = nextNamed(t, stream)
	if ev.Name != "follows-changed" || !strings.Contains(ev.Data, "data-reload") {
		t.Errorf("event = %+v, want follows-changed", ev)
	}
}

func TestEventsCoalesceAndPing(t *testing.T) {
	e := newEnv(t)
	e.srv.pingEvery = 150 * time.Millisecond
	e.srv.coalesce = 100 * time.Millisecond
	a := e.createFollow(model.Follow{FeedURL: "https://a.example/feed"})
	b := e.createFollow(model.Follow{FeedURL: "https://b.example/feed"})
	_, stream := openStream(t, e)

	for range 20 {
		e.ev.Publish(events.Event{Kind: events.FollowUpdated, FollowID: a.ID})
		e.ev.Publish(events.Event{Kind: events.FollowUpdated, FollowID: b.ID})
	}
	counts := map[string]int{}
	pings := 0
	deadline := time.After(700 * time.Millisecond)
	for done := false; !done; {
		select {
		case ev := <-stream:
			if ev.Name == ":" {
				pings++
			} else {
				counts[ev.Name]++
			}
		case <-deadline:
			done = true
		}
	}
	// the first event of each burst goes out at once, the other 19 collapse into one
	for _, name := range []string{"follow-1", "follow-2"} {
		if counts[name] != 2 {
			t.Errorf("%s sent %d times, want 2", name, counts[name])
		}
	}
	if pings == 0 {
		t.Error("no ping comments")
	}
}

func TestEventsEndWithCloseStreams(t *testing.T) {
	e := newEnv(t)
	_, stream := openStream(t, e)
	e.srv.CloseStreams()
	e.srv.CloseStreams() // idempotent
	for {
		select {
		case _, ok := <-stream:
			if !ok {
				// new streams are refused while shutting down
				w := e.get("/events")
				wantStatus(t, w, http.StatusServiceUnavailable)
				return
			}
		case <-time.After(5 * time.Second):
			t.Fatal("stream still open after CloseStreams")
		}
	}
}

func TestEventsEndWithRequest(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/events", nil)
	req.SetBasicAuth(testUser, testPass)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.h.ServeHTTP(w, req)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after the request ended")
	}
}

func TestCoalescer(t *testing.T) {
	const every = 500 * time.Millisecond
	c := newCoalescer(every)
	at := func(ms int) time.Time { return now.Add(time.Duration(ms) * time.Millisecond) }

	if !c.add(1, at(0)) {
		t.Fatal("the first event goes out at once")
	}
	if _, ok := c.next(); ok {
		t.Fatal("nothing should be pending")
	}
	if c.add(1, at(100)) || c.add(1, at(200)) {
		t.Fatal("events inside the interval wait")
	}
	if !c.add(2, at(250)) {
		t.Fatal("other keys are independent")
	}
	if next, ok := c.next(); !ok || !next.Equal(at(500)) {
		t.Fatalf("next = %v, %v; want 500ms", next, ok)
	}
	if got := c.due(at(499)); got != nil {
		t.Fatalf("due early: %v", got)
	}
	if got := c.due(at(500)); !slices.Equal(got, []int64{1}) {
		t.Fatalf("due = %v, want [1]", got)
	}
	// the delayed emit restarts the interval
	if c.add(1, at(900)) {
		t.Fatal("an event right after a delayed emit waits")
	}
	if c.add(2, at(700)) {
		t.Fatal("key 2 is still inside its interval")
	}
	if next, _ := c.next(); !next.Equal(at(750)) {
		t.Fatalf("next = %v, want 750ms", next)
	}
	if got := c.due(at(1000)); !slices.Equal(got, []int64{1, 2}) {
		t.Fatalf("due = %v, want [1 2]", got)
	}
	if !c.add(3, at(1000)) || !c.add(1, at(1600)) {
		t.Fatal("quiet keys go out at once")
	}
	c.due(at(5000))
	if len(c.last) != 0 || len(c.pending) != 0 {
		t.Errorf("state not pruned: last %v pending %v", c.last, c.pending)
	}
}

func TestWriteEvent(t *testing.T) {
	var buf bytes.Buffer
	if err := writeEvent(&buf, "follow-7", []byte("\n<div>\r\n\n  <b>x</b>\r</div>\n\n")); err != nil {
		t.Fatal(err)
	}
	want := "event: follow-7\ndata: <div>\ndata:   <b>x</b>\ndata: </div>\n\n"
	if buf.String() != want {
		t.Errorf("got %q, want %q", buf.String(), want)
	}
}
