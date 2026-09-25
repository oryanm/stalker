package events

import (
	"sync"
	"testing"
	"testing/synctest"
)

func (b *Broker) subscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}

// drain reads everything currently buffered without blocking.
func drain(ch <-chan Event) []Event {
	var got []Event
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, e)
		default:
			return got
		}
	}
}

func TestFanOut(t *testing.T) {
	tests := []struct {
		name        string
		subscribers int
		events      int
	}{
		{"no subscribers", 0, 5},
		{"one subscriber", 1, 5},
		{"many subscribers", 5, 10},
		{"exactly a full buffer", 3, bufferSize},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := NewBroker()
			chans := make([]<-chan Event, tt.subscribers)
			for i := range chans {
				ch, cancel := b.Subscribe()
				defer cancel()
				chans[i] = ch
			}
			for i := range tt.events {
				b.Publish(Event{Kind: FollowUpdated, FollowID: int64(i + 1)})
			}
			for i, ch := range chans {
				got := drain(ch)
				if len(got) != tt.events {
					t.Fatalf("subscriber %d got %d events, want %d", i, len(got), tt.events)
				}
				for j, e := range got {
					if e.Kind != FollowUpdated || e.FollowID != int64(j+1) {
						t.Fatalf("subscriber %d event %d = %+v, want in-order FollowUpdated %d", i, j, e, j+1)
					}
				}
			}
		})
	}
}

func TestSlowSubscriberMissesEvents(t *testing.T) {
	// inside the bubble a blocked Publish is reported as a deadlock instead of hanging the test
	synctest.Test(t, func(t *testing.T) {
		b := NewBroker()
		slow, cancelSlow := b.Subscribe()
		defer cancelSlow()
		fast, cancelFast := b.Subscribe()
		defer cancelFast()

		const total = bufferSize * 3
		var fastGot []Event
		for i := range total {
			b.Publish(Event{Kind: FollowFetching, FollowID: int64(i + 1)})
			fastGot = append(fastGot, <-fast)
		}

		if len(fastGot) != total {
			t.Errorf("fast subscriber got %d events, want %d", len(fastGot), total)
		}
		slowGot := drain(slow)
		if len(slowGot) != bufferSize {
			t.Fatalf("slow subscriber got %d events, want %d", len(slowGot), bufferSize)
		}
		// the oldest events are kept and later ones dropped
		if first, last := slowGot[0].FollowID, slowGot[bufferSize-1].FollowID; first != 1 || last != bufferSize {
			t.Errorf("slow subscriber kept events %d..%d, want 1..%d", first, last, bufferSize)
		}

		// once drained the slow subscriber receives new events again
		b.Publish(Event{Kind: FollowsChanged})
		if e := <-slow; e.Kind != FollowsChanged {
			t.Errorf("slow subscriber got %+v after draining, want FollowsChanged", e)
		}
	})
}

func TestCancel(t *testing.T) {
	b := NewBroker()
	ch, cancel := b.Subscribe()
	other, cancelOther := b.Subscribe()
	defer cancelOther()

	b.Publish(Event{Kind: FollowUpdated, FollowID: 1})
	cancel()
	cancel() // idempotent
	b.Publish(Event{Kind: FollowUpdated, FollowID: 2})

	if got := b.subscriberCount(); got != 1 {
		t.Errorf("%d subscribers after cancel, want 1", got)
	}
	// events buffered before cancel are still delivered, then the channel is closed
	if e, ok := <-ch; !ok || e.FollowID != 1 {
		t.Errorf("first receive = %+v, %v; want buffered event 1", e, ok)
	}
	if e, ok := <-ch; ok {
		t.Errorf("received %+v after cancel, want a closed channel", e)
	}
	if got := drain(other); len(got) != 2 {
		t.Errorf("other subscriber got %d events, want 2", len(got))
	}
}

func TestConcurrentCancel(t *testing.T) {
	b := NewBroker()
	ch, cancel := b.Subscribe()
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(cancel)
	}
	wg.Wait()
	if _, ok := <-ch; ok {
		t.Error("channel still open after cancel")
	}
	if got := b.subscriberCount(); got != 0 {
		t.Errorf("%d subscribers after cancel, want 0", got)
	}
}

func TestCancelDuringPublishStorm(t *testing.T) {
	b := NewBroker()
	stop := make(chan struct{})
	var publishers sync.WaitGroup
	for p := range 8 {
		publishers.Go(func() {
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
					b.Publish(Event{Kind: FollowUpdated, FollowID: int64(p*1_000_000 + i)})
				}
			}
		})
	}

	var subscribers sync.WaitGroup
	for s := range 200 {
		subscribers.Go(func() {
			ch, cancel := b.Subscribe()
			for range s % 5 {
				<-ch
			}
			// half the subscribers race two cancels against each other and the publishers
			if s%2 == 0 {
				var cancels sync.WaitGroup
				cancels.Go(cancel)
				cancels.Go(cancel)
				cancels.Wait()
			} else {
				cancel()
			}
			for range ch {
				// cancel closed the channel, so this loop ends once the buffer is drained
			}
		})
	}
	subscribers.Wait()
	close(stop)
	publishers.Wait()

	if got := b.subscriberCount(); got != 0 {
		t.Errorf("%d subscribers left after every cancel, want 0", got)
	}
}

func TestZeroValueBroker(t *testing.T) {
	var b Broker
	b.Publish(Event{Kind: FollowsChanged})
	ch, cancel := b.Subscribe()
	defer cancel()
	b.Publish(Event{Kind: FollowsChanged})
	if got := drain(ch); len(got) != 1 || got[0].Kind != FollowsChanged {
		t.Errorf("zero value broker delivered %+v, want one FollowsChanged", got)
	}
}

func TestNilBrokerPublish(t *testing.T) {
	var b *Broker
	b.Publish(Event{Kind: FollowUpdated, FollowID: 1}) // must not panic
}
