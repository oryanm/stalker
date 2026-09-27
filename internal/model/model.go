// Package model holds the domain types shared by every stalker package.
package model

import (
	"net/url"
	"strings"
	"time"
)

// Importance is how closely a follow is watched. The values match Fraidycat's
// OPML export (`importance/N` in the category attribute) so exports round-trip.
type Importance int

const (
	Realtime   Importance = 0
	Frequent   Importance = 1
	Occasional Importance = 7
	Sometime   Importance = 30
	Rarely     Importance = 365
)

// Tier describes an importance level for display.
type Tier struct {
	Importance  Importance
	Name        string
	Emoji       string
	Description string
}

// Tiers lists every importance level, most important first.
var Tiers = []Tier{
	{Realtime, "Realtime", "🚄", "Following this with complete devotion."},
	{Frequent, "Frequent", "🌄", "Keep just out of view. Nevertheless: beloved."},
	{Occasional, "Occasional", "🐇", "For when I have free time."},
	{Sometime, "Sometime", "🍊", "Maintaining a mild curiosity here."},
	{Rarely, "Rarely", "☂", "Not very active. Or, just don't lose this."},
}

// NormalizeImportance snaps an arbitrary number to the nearest defined tier at
// or below it (the same bucketing Fraidycat's scheduler uses).
func NormalizeImportance(n int) Importance {
	switch {
	case n < 1:
		return Realtime
	case n < 7:
		return Frequent
	case n < 30:
		return Occasional
	case n < 365:
		return Sometime
	default:
		return Rarely
	}
}

// TierFor returns the tier for an importance, normalizing unknown values.
func TierFor(i Importance) Tier {
	n := NormalizeImportance(int(i))
	for _, t := range Tiers {
		if t.Importance == n {
			return t
		}
	}
	return Tiers[1]
}

// HomeTag is the implicit tag for follows without tags (Fraidycat's "house").
const HomeTag = "🏠"

// Follow is a subscription to one feed.
type Follow struct {
	ID          int64
	URL         string // home page, used for the title link
	FeedURL     string // unique
	Title       string // user override, empty means use FeedTitle
	FeedTitle   string // title reported by the feed
	Description string
	PhotoURL    string
	Importance  Importance
	Tags        []string // sorted and deduplicated, never contains HomeTag implicitly
	CreatedAt   time.Time
	EditedAt    time.Time

	// fetch state, owned by the poller
	ETag          string
	LastModified  string
	LastFetchedAt time.Time // zero means never fetched
	NextFetchAt   time.Time
	LastError     string    // empty when the last fetch succeeded
	ErrorCount    int       // consecutive failures
	FailingSince  time.Time // first failure of the current run, zero while fetches succeed
	LastPostAt    time.Time // zero means no posts
}

// DisplayTitle is the user title, then the feed title, then the host name.
func (f Follow) DisplayTitle() string {
	if t := strings.TrimSpace(f.Title); t != "" {
		return t
	}
	if t := strings.TrimSpace(f.FeedTitle); t != "" {
		return t
	}
	for _, raw := range []string{f.URL, f.FeedURL} {
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			return strings.TrimPrefix(u.Host, "www.")
		}
	}
	return f.FeedURL
}

// DisplayTags are the tags a follow is listed under: its own, or HomeTag.
func (f Follow) DisplayTags() []string {
	if len(f.Tags) == 0 {
		return []string{HomeTag}
	}
	return f.Tags
}

// Post is one entry of a follow's feed.
type Post struct {
	ID          int64
	FollowID    int64
	GUID        string // unique per follow
	URL         string
	Title       string
	Author      string
	PublishedAt time.Time // zero from the feed package when unknown, never zero once stored
	UpdatedAt   time.Time
	FirstSeenAt time.Time
}
