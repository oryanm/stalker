// Package events is an in-process pub/sub used to push updates to SSE clients.
package events

import "sync"

// Kind of event.
type Kind string

const (
	FollowUpdated  Kind = "follow-updated"  // one follow's data or fetch state changed
	FollowFetching Kind = "follow-fetching" // a fetch of one follow started
	FollowsChanged Kind = "follows-changed" // follows were added, removed or retagged
)

type Event struct {
	Kind     Kind
	FollowID int64 // zero for FollowsChanged
}

// bufferSize is how far a subscriber may fall behind before it misses events.
const bufferSize = 64

// Broker fans events out to subscribers. Publish never blocks: a subscriber
// whose buffer (64) is full misses the event. Safe for concurrent use. The
// zero value is ready to use.
type Broker struct {
	mu   sync.RWMutex
	subs map[chan Event]struct{}
}

func NewBroker() *Broker {
	return &Broker{subs: make(map[chan Event]struct{})}
}

// Subscribe returns a channel of events and a cancel func that unsubscribes
// and closes the channel. Cancel is idempotent.
func (b *Broker) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, bufferSize)
	b.mu.Lock()
	if b.subs == nil {
		b.subs = make(map[chan Event]struct{})
	}
	b.subs[ch] = struct{}{}
	b.mu.Unlock()

	cancel := func() {
		// closing under the write lock guarantees no Publish is sending on ch
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
	}
	return ch, cancel
}

// Publish delivers e to every subscriber with room in its buffer. Publishing
// on a nil Broker is a no-op, so events are optional for callers.
func (b *Broker) Publish(e Event) {
	if b == nil {
		return
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.subs {
		select {
		case ch <- e:
		default:
			// a stalled SSE client must not hold up the poller
		}
	}
}
