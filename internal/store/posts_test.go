package store

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oryanm/stalker/internal/model"
)

const day = 24 * time.Hour

type fetch struct {
	at    time.Time
	posts []model.Post
}

func record(t *testing.T, s *Store, r FetchResult) {
	t.Helper()
	if err := s.RecordFetch(t.Context(), r); err != nil {
		t.Fatalf("RecordFetch: %v", err)
	}
}

func allPosts(t *testing.T, s *Store, followID int64) []model.Post {
	t.Helper()
	posts, err := s.RecentPosts(t.Context(), followID, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return posts
}

func guids(posts []model.Post) []string {
	var out []string
	for _, p := range posts {
		out = append(out, p.GUID)
	}
	return out
}

func TestRecordFetchPostDates(t *testing.T) {
	t1 := t0.Add(time.Hour)
	tests := []struct {
		name    string
		fetches []fetch
		want    model.Post // the stored post "g"; ID and FollowID are not compared
	}{
		{
			name:    "new post with a date",
			fetches: []fetch{{t0, []model.Post{{GUID: "g", Title: "T", PublishedAt: t0.Add(-time.Hour)}}}},
			want:    model.Post{GUID: "g", Title: "T", PublishedAt: t0.Add(-time.Hour), FirstSeenAt: t0},
		},
		{
			name:    "new post without a date uses first seen",
			fetches: []fetch{{t0, []model.Post{{GUID: "g"}}}},
			want:    model.Post{GUID: "g", PublishedAt: t0, FirstSeenAt: t0},
		},
		{
			name:    "future date is clamped",
			fetches: []fetch{{t0, []model.Post{{GUID: "g", PublishedAt: t0.Add(day)}}}},
			want:    model.Post{GUID: "g", PublishedAt: t0, FirstSeenAt: t0},
		},
		{
			name: "existing post keeps its date when the new one is zero",
			fetches: []fetch{
				{t0, []model.Post{{GUID: "g", PublishedAt: t0.Add(-time.Hour)}}},
				{t1, []model.Post{{GUID: "g"}}},
			},
			want: model.Post{GUID: "g", PublishedAt: t0.Add(-time.Hour), FirstSeenAt: t0},
		},
		{
			name: "existing post takes a new date",
			fetches: []fetch{
				{t0, []model.Post{{GUID: "g", PublishedAt: t0.Add(-time.Hour)}}},
				{t1, []model.Post{{GUID: "g", PublishedAt: t0.Add(-3 * time.Hour)}}},
			},
			want: model.Post{GUID: "g", PublishedAt: t0.Add(-3 * time.Hour), FirstSeenAt: t0},
		},
		{
			name: "undated post gets a real date later",
			fetches: []fetch{
				{t0, []model.Post{{GUID: "g"}}},
				{t1, []model.Post{{GUID: "g", PublishedAt: t0.Add(-5 * day)}}},
			},
			want: model.Post{GUID: "g", PublishedAt: t0.Add(-5 * day), FirstSeenAt: t0},
		},
		{
			name: "future-dated existing post is not bumped",
			fetches: []fetch{
				{t0, []model.Post{{GUID: "g", PublishedAt: t0.Add(2 * time.Hour)}}},
				{t1, []model.Post{{GUID: "g", PublishedAt: t0.Add(2 * time.Hour)}}},
			},
			want: model.Post{GUID: "g", PublishedAt: t0, FirstSeenAt: t0},
		},
		{
			name: "future date that has passed is taken",
			fetches: []fetch{
				{t0, []model.Post{{GUID: "g", PublishedAt: t0.Add(30 * time.Minute)}}},
				{t1, []model.Post{{GUID: "g", PublishedAt: t0.Add(30 * time.Minute)}}},
			},
			want: model.Post{GUID: "g", PublishedAt: t0.Add(30 * time.Minute), FirstSeenAt: t0},
		},
		{
			name: "later date without UpdatedAt is an edit",
			fetches: []fetch{
				{t0, []model.Post{{GUID: "g", PublishedAt: t0.Add(-365 * day)}}},
				{t1, []model.Post{{GUID: "g", PublishedAt: t0.Add(30 * time.Minute)}}},
			},
			want: model.Post{GUID: "g", PublishedAt: t0.Add(-365 * day), UpdatedAt: t0.Add(30 * time.Minute), FirstSeenAt: t0},
		},
		{
			name: "edit stays an edit on the next fetch",
			fetches: []fetch{
				{t0, []model.Post{{GUID: "g", PublishedAt: t0.Add(-365 * day)}}},
				{t1, []model.Post{{GUID: "g", PublishedAt: t0.Add(30 * time.Minute)}}},
				{t1.Add(time.Hour), []model.Post{{GUID: "g", PublishedAt: t0.Add(30 * time.Minute)}}},
			},
			want: model.Post{GUID: "g", PublishedAt: t0.Add(-365 * day), UpdatedAt: t0.Add(30 * time.Minute), FirstSeenAt: t0},
		},
		{
			name: "later date with UpdatedAt is taken",
			fetches: []fetch{
				{t0, []model.Post{{GUID: "g", PublishedAt: t0.Add(-365 * day)}}},
				{t1, []model.Post{{GUID: "g", PublishedAt: t0.Add(30 * time.Minute), UpdatedAt: t0.Add(30 * time.Minute)}}},
			},
			want: model.Post{GUID: "g", PublishedAt: t0.Add(30 * time.Minute), UpdatedAt: t0.Add(30 * time.Minute), FirstSeenAt: t0},
		},
		{
			name: "undated post takes a later date",
			fetches: []fetch{
				{t0, []model.Post{{GUID: "g"}}},
				{t1, []model.Post{{GUID: "g", PublishedAt: t0.Add(30 * time.Minute)}}},
			},
			want: model.Post{GUID: "g", PublishedAt: t0.Add(30 * time.Minute), FirstSeenAt: t0},
		},
		{
			name: "content fields follow the feed",
			fetches: []fetch{
				{t0, []model.Post{{GUID: "g", URL: "https://a/1", Title: "Old", Author: "A", UpdatedAt: t0.Add(-time.Hour)}}},
				{t1, []model.Post{{GUID: "g", URL: "https://a/2", Title: "New", Author: "B", UpdatedAt: t0}}},
			},
			want: model.Post{
				GUID: "g", URL: "https://a/2", Title: "New", Author: "B", PublishedAt: t0, UpdatedAt: t0, FirstSeenAt: t0,
			},
		},
		{
			name: "missing GUID falls back to URL",
			fetches: []fetch{
				{t0, []model.Post{{URL: "g", Title: "by URL"}, {Title: "no key, dropped"}}},
			},
			want: model.Post{GUID: "g", URL: "g", Title: "by URL", PublishedAt: t0, FirstSeenAt: t0},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openStore(t)
			f := mustCreate(t, s, model.Follow{FeedURL: "https://example.com/feed"})
			for _, fe := range tt.fetches {
				record(t, s, FetchResult{FollowID: f.ID, FetchedAt: fe.at, Posts: fe.posts})
			}
			posts := allPosts(t, s, f.ID)
			if len(posts) != 1 {
				t.Fatalf("stored %d posts (%q), want 1", len(posts), guids(posts))
			}
			got := posts[0]
			if got.FollowID != f.ID {
				t.Errorf("FollowID = %d, want %d", got.FollowID, f.ID)
			}
			got.ID, got.FollowID = 0, 0
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("stored post = %+v\nwant %+v", got, tt.want)
			}
			if lp := mustGet(t, s, f.ID).LastPostAt; !lp.Equal(tt.want.PublishedAt) {
				t.Errorf("LastPostAt = %v, want %v", lp, tt.want.PublishedAt)
			}
		})
	}
}

func TestRecordFetchDuplicateGUIDsKeepFirst(t *testing.T) {
	s := openStore(t)
	f := mustCreate(t, s, model.Follow{FeedURL: "https://example.com/feed"})
	record(t, s, FetchResult{FollowID: f.ID, FetchedAt: t0, Posts: []model.Post{
		{GUID: "a", Title: "first a", PublishedAt: t0.Add(-time.Hour)},
		{GUID: "b", Title: "b", PublishedAt: t0.Add(-2 * time.Hour)},
		{GUID: "a", Title: "second a", PublishedAt: t0.Add(-3 * time.Hour)},
	}})
	posts := allPosts(t, s, f.ID)
	if got := guids(posts); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("stored GUIDs = %q, want [a b]", got)
	}
	if posts[0].Title != "first a" || !posts[0].PublishedAt.Equal(t0.Add(-time.Hour)) {
		t.Errorf("duplicate GUID kept %+v, want the first occurrence", posts[0])
	}
}

func TestRecordFetchUndatedKeepsFeedOrder(t *testing.T) {
	s := openStore(t)
	f := mustCreate(t, s, model.Follow{FeedURL: "https://example.com/feed"})
	record(t, s, FetchResult{FollowID: f.ID, FetchedAt: t0, Posts: []model.Post{{GUID: "c"}, {GUID: "b"}, {GUID: "a"}}})
	record(t, s, FetchResult{FollowID: f.ID, FetchedAt: t0.Add(time.Hour), Posts: []model.Post{
		{GUID: "e"}, {GUID: "d"}, {GUID: "c"}, {GUID: "b"}, {GUID: "a"},
	}})
	if got, want := guids(allPosts(t, s, f.ID)), []string{"e", "d", "c", "b", "a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("post order = %q, want %q", got, want)
	}
}

func TestRecordFetchFollowMetadata(t *testing.T) {
	const feedURL = "https://example.com/feed"
	base := model.Follow{
		URL: "https://example.com/", FeedURL: feedURL, FeedTitle: "Old title", Description: "Old desc",
		PhotoURL: "https://example.com/old.png", ETag: "e0", LastModified: "lm0",
	}
	tests := []struct {
		name   string
		follow func(f *model.Follow)
		result FetchResult
		want   func(f *model.Follow)
	}{
		{
			name:   "empty metadata is ignored, validators replaced",
			result: FetchResult{ETag: "e1", LastModified: "lm1", SiteURL: "https://other.example/"},
			want:   func(f *model.Follow) { f.ETag, f.LastModified = "e1", "lm1" },
		},
		{
			name: "metadata applied",
			result: FetchResult{
				FeedTitle: " New title ", Description: "New desc", PhotoURL: "https://example.com/new.png",
			},
			want: func(f *model.Follow) {
				f.FeedTitle, f.Description, f.PhotoURL = "New title", "New desc", "https://example.com/new.png"
				f.ETag, f.LastModified = "", ""
			},
		},
		{
			name:   "site URL fills an empty URL",
			follow: func(f *model.Follow) { f.URL = "" },
			result: FetchResult{ETag: "e0", LastModified: "lm0", SiteURL: "https://site.example/"},
			want:   func(f *model.Follow) { f.URL = "https://site.example/" },
		},
		{
			name:   "site URL replaces the feed URL placeholder",
			follow: func(f *model.Follow) { f.URL = feedURL },
			result: FetchResult{ETag: "e0", LastModified: "lm0", SiteURL: "https://site.example/"},
			want:   func(f *model.Follow) { f.URL = "https://site.example/" },
		},
		{
			name:   "empty site URL keeps the placeholder",
			follow: func(f *model.Follow) { f.URL = feedURL },
			result: FetchResult{ETag: "e0", LastModified: "lm0"},
			want:   func(f *model.Follow) {},
		},
		{
			name:   "user title is untouched",
			follow: func(f *model.Follow) { f.Title = "Mine" },
			result: FetchResult{ETag: "e0", LastModified: "lm0", FeedTitle: "Feed"},
			want:   func(f *model.Follow) { f.FeedTitle = "Feed" },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openStore(t)
			in := base
			if tt.follow != nil {
				tt.follow(&in)
			}
			f := mustCreate(t, s, in)
			r := tt.result
			r.FollowID, r.FetchedAt, r.NextFetchAt = f.ID, t0.Add(time.Minute), t0.Add(time.Hour)
			record(t, s, r)

			want := f
			tt.want(&want)
			want.LastFetchedAt, want.NextFetchAt = t0.Add(time.Minute), t0.Add(time.Hour)
			if got := mustGet(t, s, f.ID); !reflect.DeepEqual(got, want) {
				t.Errorf("follow = %+v\nwant %+v", got, want)
			}
		})
	}
}

func TestRecordFetchNotModified(t *testing.T) {
	s := openStore(t)
	f := mustCreate(t, s, model.Follow{FeedURL: "https://example.com/feed", FeedTitle: "Feed"})
	record(t, s, FetchResult{FollowID: f.ID, FetchedAt: t0, ETag: "e0", LastModified: "lm0", Posts: []model.Post{{GUID: "a"}}})
	record(t, s, FetchResult{FollowID: f.ID, Err: "HTTP 500", FetchedAt: t0})

	// steps run in order against one follow
	steps := []struct {
		name   string
		result FetchResult
		etag   string
		lm     string
	}{
		{"validators kept when absent", FetchResult{}, "e0", "lm0"},
		{"new etag replaces only the etag", FetchResult{ETag: "e2"}, "e2", "lm0"},
		{"new last-modified replaces only it", FetchResult{LastModified: "lm3"}, "e2", "lm3"},
	}
	for i, st := range steps {
		at := t0.Add(time.Duration(i+1) * time.Hour)
		r := st.result
		r.FollowID, r.NotModified, r.FetchedAt, r.NextFetchAt = f.ID, true, at, at.Add(time.Hour)
		r.FeedTitle, r.Posts = "ignored", []model.Post{{GUID: "ignored"}}
		record(t, s, r)

		got := mustGet(t, s, f.ID)
		if got.ETag != st.etag || got.LastModified != st.lm {
			t.Errorf("%s: validators = (%q, %q), want (%q, %q)", st.name, got.ETag, got.LastModified, st.etag, st.lm)
		}
		if got.LastError != "" || got.ErrorCount != 0 {
			t.Errorf("%s: error state = (%q, %d), want cleared", st.name, got.LastError, got.ErrorCount)
		}
		if !got.LastFetchedAt.Equal(at) || !got.NextFetchAt.Equal(at.Add(time.Hour)) {
			t.Errorf("%s: fetch times = (%v, %v), want (%v, %v)", st.name, got.LastFetchedAt, got.NextFetchAt, at, at.Add(time.Hour))
		}
		if got.FeedTitle != "Feed" {
			t.Errorf("%s: FeedTitle = %q, want it untouched", st.name, got.FeedTitle)
		}
		if g := guids(allPosts(t, s, f.ID)); !reflect.DeepEqual(g, []string{"a"}) {
			t.Errorf("%s: posts = %q, want untouched [a]", st.name, g)
		}
	}
}

func TestRecordFetchErrors(t *testing.T) {
	s := openStore(t)
	f := mustCreate(t, s, model.Follow{FeedURL: "https://example.com/feed"})
	record(t, s, FetchResult{FollowID: f.ID, FetchedAt: t0, ETag: "e1", FeedTitle: "Feed", Posts: []model.Post{{GUID: "a"}}})

	steps := []struct {
		name      string
		result    FetchResult
		wantErr   string
		wantCount int
	}{
		{"first failure", FetchResult{Err: "HTTP 500"}, "HTTP 500", 1},
		{"second failure", FetchResult{Err: "timeout", FeedTitle: "ignored", Posts: []model.Post{{GUID: "b"}}}, "timeout", 2},
		{"third failure", FetchResult{Err: "HTTP 404"}, "HTTP 404", 3},
		{"success resets", FetchResult{ETag: "e1", FeedTitle: "Feed"}, "", 0},
		{"failure after success", FetchResult{Err: "dns"}, "dns", 1},
		{"not modified resets", FetchResult{NotModified: true}, "", 0},
	}
	for i, st := range steps {
		at := t0.Add(time.Duration(i+1) * time.Hour)
		r := st.result
		r.FollowID, r.FetchedAt, r.NextFetchAt = f.ID, at, at.Add(2*time.Hour)
		record(t, s, r)

		got := mustGet(t, s, f.ID)
		if got.LastError != st.wantErr || got.ErrorCount != st.wantCount {
			t.Errorf("%s: error state = (%q, %d), want (%q, %d)", st.name, got.LastError, got.ErrorCount, st.wantErr, st.wantCount)
		}
		if !got.LastFetchedAt.Equal(at) || !got.NextFetchAt.Equal(at.Add(2*time.Hour)) {
			t.Errorf("%s: fetch times = (%v, %v), want (%v, %v)", st.name, got.LastFetchedAt, got.NextFetchAt, at, at.Add(2*time.Hour))
		}
		if got.ETag != "e1" || got.FeedTitle != "Feed" {
			t.Errorf("%s: ETag, FeedTitle = %q, %q; want kept", st.name, got.ETag, got.FeedTitle)
		}
		if g := guids(allPosts(t, s, f.ID)); !reflect.DeepEqual(g, []string{"a"}) {
			t.Errorf("%s: posts = %q, want kept [a]", st.name, g)
		}
	}
}

func TestRecordFetchUnknownFollow(t *testing.T) {
	s := openStore(t)
	for name, r := range map[string]FetchResult{
		"success":      {Posts: []model.Post{{GUID: "a"}}},
		"not modified": {NotModified: true},
		"error":        {Err: "HTTP 500"},
	} {
		r.FollowID, r.FetchedAt = 999, t0
		if err := s.RecordFetch(t.Context(), r); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: RecordFetch(unknown follow) = %v, want ErrNotFound", name, err)
		}
	}
}

func TestRecordFetchZeroFetchedAtIsNow(t *testing.T) {
	s := openStore(t)
	f := mustCreate(t, s, model.Follow{FeedURL: "https://example.com/feed"})
	record(t, s, FetchResult{FollowID: f.ID, Posts: []model.Post{{GUID: "a", PublishedAt: t0.Add(day)}}})
	posts := allPosts(t, s, f.ID)
	if got := mustGet(t, s, f.ID).LastFetchedAt; !got.Equal(t0) {
		t.Errorf("LastFetchedAt = %v, want now (%v)", got, t0)
	}
	if !posts[0].PublishedAt.Equal(t0) || !posts[0].FirstSeenAt.Equal(t0) {
		t.Errorf("post = %+v, want clamped to and first seen at now", posts[0])
	}
}

// agedPosts returns n posts, newest first, the i-th published age(i) before now.
func agedPosts(n int, now time.Time, age func(i int) time.Duration) []model.Post {
	posts := make([]model.Post, n)
	for i := range n {
		posts[i] = model.Post{GUID: fmt.Sprintf("p%02d", i), PublishedAt: now.Add(-age(i))}
	}
	return posts
}

func TestRecordFetchPrune(t *testing.T) {
	tests := []struct {
		name    string
		fetches []fetch // a fetch without posts prunes whatever the feed stopped listing
		want    int     // posts kept: always the newest ones
	}{
		{
			name: "old posts among the newest 50 are kept",
			fetches: []fetch{
				{t0, agedPosts(10, t0, func(i int) time.Duration { return 365*day + time.Duration(i)*day })},
				{t0, nil},
			},
			want: 10,
		},
		{
			name: "recent posts beyond the newest 50 are kept",
			fetches: []fetch{
				{t0, agedPosts(60, t0, func(i int) time.Duration { return time.Duration(i) * day })},
				{t0, nil},
			},
			want: 60,
		},
		{
			name: "old posts beyond the newest 50 are pruned",
			fetches: []fetch{
				{t0, agedPosts(60, t0, func(i int) time.Duration { return time.Duration(i) * 5 * day })},
				{t0, nil},
			},
			want: 50,
		},
		{
			name: "posts the feed still lists are not pruned",
			fetches: []fetch{
				{t0, agedPosts(60, t0, func(i int) time.Duration { return time.Duration(i) * 5 * day })},
				{t0.Add(time.Hour), agedPosts(60, t0, func(i int) time.Duration { return time.Duration(i) * 5 * day })},
			},
			want: 60,
		},
		{
			name: "exactly 180 days old is kept",
			fetches: []fetch{
				{t0, agedPosts(52, t0, func(i int) time.Duration {
					switch i {
					case 50:
						return 180 * day
					case 51:
						return 180*day + time.Millisecond
					}
					return time.Duration(i) * time.Hour
				})},
				{t0, nil},
			},
			want: 51,
		},
		{
			name: "age is relative to the latest fetch",
			fetches: []fetch{
				{t0, agedPosts(55, t0, func(i int) time.Duration { return time.Duration(i) * day })},
				{t0.Add(181 * day), nil},
			},
			want: 50,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openStore(t)
			f := mustCreate(t, s, model.Follow{FeedURL: "https://example.com/feed"})
			for _, fe := range tt.fetches {
				record(t, s, FetchResult{FollowID: f.ID, FetchedAt: fe.at, Posts: fe.posts})
			}
			got := guids(allPosts(t, s, f.ID))
			want := guids(agedPosts(tt.want, t0, func(int) time.Duration { return 0 }))
			if !reflect.DeepEqual(got, want) {
				t.Errorf("kept %d posts %q, want %d %q", len(got), got, len(want), want)
			}
			newest := tt.fetches[0].posts[0].PublishedAt
			if lp := mustGet(t, s, f.ID).LastPostAt; !lp.Equal(newest) {
				t.Errorf("LastPostAt = %v, want %v", lp, newest)
			}
		})
	}
}

// undatedPosts returns n posts without dates named prefix-0 to prefix-(n-1).
func undatedPosts(prefix string, n int) []model.Post {
	posts := make([]model.Post, n)
	for i := range n {
		posts[i] = model.Post{GUID: fmt.Sprintf("%s-%d", prefix, i)}
	}
	return posts
}

func TestRecordFetchKeepsListedUndatedPosts(t *testing.T) {
	s := openStore(t)
	f := mustCreate(t, s, model.Follow{FeedURL: "https://example.com/feed"})
	old := undatedPosts("old", 10)
	record(t, s, FetchResult{FollowID: f.ID, FetchedAt: t0, Posts: old})
	// half a year later the feed has 60 newer undated items and still lists the old ones
	later := t0.Add(181 * day)
	listing := append(undatedPosts("new", 60), old...)
	record(t, s, FetchResult{FollowID: f.ID, FetchedAt: later, Posts: listing})
	record(t, s, FetchResult{FollowID: f.ID, FetchedAt: later.Add(10 * time.Minute), Posts: listing})

	posts := allPosts(t, s, f.ID)
	if len(posts) != 70 {
		t.Fatalf("stored %d posts, want 70", len(posts))
	}
	for _, p := range posts {
		if !strings.HasPrefix(p.GUID, "old-") {
			continue
		}
		if !p.PublishedAt.Equal(t0) || !p.FirstSeenAt.Equal(t0) {
			t.Errorf("%s: published %v, first seen %v; want both kept at %v", p.GUID, p.PublishedAt, p.FirstSeenAt, t0)
		}
	}
	if got := posts[0].GUID; got != "new-0" {
		t.Errorf("newest post = %s, want new-0", got)
	}
	if lp := mustGet(t, s, f.ID).LastPostAt; !lp.Equal(later) {
		t.Errorf("LastPostAt = %v, want %v", lp, later)
	}
}

func TestRecordFetchCaps(t *testing.T) {
	s := openStore(t)
	f := mustCreate(t, s, model.Follow{FeedURL: "https://example.com/feed"})
	record(t, s, FetchResult{FollowID: f.ID, FetchedAt: t0, Posts: undatedPosts("f0", 300)})
	posts := allPosts(t, s, f.ID)
	if len(posts) != maxFetchPosts || posts[len(posts)-1].GUID != fmt.Sprintf("f0-%d", maxFetchPosts-1) {
		t.Fatalf("one fetch of 300 stored %d posts, want the first %d", len(posts), maxFetchPosts)
	}

	// a feed inventing fresh GUIDs on every fetch
	for i := 1; i <= 4; i++ {
		record(t, s, FetchResult{FollowID: f.ID, FetchedAt: t0.Add(time.Duration(i) * time.Minute), Posts: undatedPosts(fmt.Sprintf("f%d", i), 300)})
	}
	posts = allPosts(t, s, f.ID)
	if len(posts) != maxPosts {
		t.Fatalf("stored %d posts, want at most %d", len(posts), maxPosts)
	}
	if posts[0].GUID != "f4-0" || posts[maxFetchPosts-1].GUID != fmt.Sprintf("f4-%d", maxFetchPosts-1) {
		t.Errorf("newest posts start %s ... %s, want the latest fetch", posts[0].GUID, posts[maxFetchPosts-1].GUID)
	}
}

func TestRecordFetchDropsUnlistedPosts(t *testing.T) {
	t1 := t0.Add(2 * time.Hour)
	tests := []struct {
		name    string
		fetches []fetch
		want    []string
	}{
		{
			name: "re-uploaded video replaces the deleted one",
			fetches: []fetch{
				{t0, []model.Post{{GUID: "mistake", PublishedAt: t0.Add(-time.Hour)}, {GUID: "old", PublishedAt: t0.Add(-10 * day)}}},
				{t1, []model.Post{{GUID: "fixed", PublishedAt: t0.Add(90 * time.Minute)}, {GUID: "old", PublishedAt: t0.Add(-10 * day)}}},
			},
			want: []string{"fixed", "old"},
		},
		{
			name: "posts older than the oldest listed stay",
			fetches: []fetch{
				{t0, []model.Post{
					{GUID: "a", PublishedAt: t0.Add(-time.Hour)},
					{GUID: "b", PublishedAt: t0.Add(-2 * day)},
					{GUID: "c", PublishedAt: t0.Add(-5 * day)},
				}},
				{t1, []model.Post{{GUID: "new", PublishedAt: t0.Add(time.Hour)}, {GUID: "a", PublishedAt: t0.Add(-time.Hour)}}},
			},
			want: []string{"new", "a", "b", "c"},
		},
		{
			name: "updated date counts",
			fetches: []fetch{
				{t0, []model.Post{
					{GUID: "a", PublishedAt: t0.Add(-3 * day), UpdatedAt: t0.Add(-time.Hour)},
					{GUID: "b", PublishedAt: t0.Add(-2 * day)},
				}},
				{t1, []model.Post{
					{GUID: "new", PublishedAt: t0.Add(time.Hour)},
					{GUID: "c", PublishedAt: t0.Add(-4 * day), UpdatedAt: t0.Add(-30 * time.Hour)},
				}},
			},
			want: []string{"new", "b", "c"},
		},
		{
			name: "a single listed post is no range",
			fetches: []fetch{
				{t0, []model.Post{{GUID: "a", PublishedAt: t0.Add(-time.Hour)}, {GUID: "b", PublishedAt: t0.Add(-2 * time.Hour)}}},
				{t1, []model.Post{{GUID: "c", PublishedAt: t0.Add(-3 * time.Hour)}}},
			},
			want: []string{"a", "b", "c"},
		},
		{
			name: "undated listings drop nothing",
			fetches: []fetch{
				{t0, []model.Post{{GUID: "a", PublishedAt: t0.Add(-time.Hour)}, {GUID: "b", PublishedAt: t0.Add(-2 * time.Hour)}}},
				{t1, []model.Post{{GUID: "c"}, {GUID: "d"}}},
			},
			want: []string{"c", "d", "a", "b"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openStore(t)
			f := mustCreate(t, s, model.Follow{FeedURL: "https://example.com/feed"})
			for _, fe := range tt.fetches {
				record(t, s, FetchResult{FollowID: f.ID, FetchedAt: fe.at, Posts: fe.posts})
			}
			if got := guids(allPosts(t, s, f.ID)); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("posts = %q, want %q", got, tt.want)
			}
		})
	}

	// a not-modified or failed fetch lists nothing, so it drops nothing
	s := openStore(t)
	f := mustCreate(t, s, model.Follow{FeedURL: "https://example.com/feed"})
	record(t, s, FetchResult{FollowID: f.ID, FetchedAt: t0, Posts: []model.Post{{GUID: "a", PublishedAt: t0.Add(-time.Hour)}}})
	record(t, s, FetchResult{FollowID: f.ID, FetchedAt: t1, NotModified: true})
	record(t, s, FetchResult{FollowID: f.ID, FetchedAt: t1, Err: "HTTP 500"})
	if got := guids(allPosts(t, s, f.ID)); !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("posts after not modified and failure = %q, want [a]", got)
	}
}

func TestRecordFetchFailingSince(t *testing.T) {
	ctx := t.Context()
	s := openStore(t)
	f := model.Follow{FeedURL: "https://a.example/feed"}
	if err := s.CreateFollow(ctx, &f); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 9, 27, 4, 0, 0, 0, time.UTC)
	record := func(r FetchResult) model.Follow {
		t.Helper()
		r.FollowID = f.ID
		if err := s.RecordFetch(ctx, r); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetFollow(ctx, f.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := record(FetchResult{FetchedAt: t0, Err: "HTTP 404"}); !got.FailingSince.Equal(t0) {
		t.Errorf("first failure: FailingSince = %v, want %v", got.FailingSince, t0)
	}
	if got := record(FetchResult{FetchedAt: t0.Add(6 * time.Hour), Err: "HTTP 404"}); !got.FailingSince.Equal(t0) {
		t.Errorf("second failure: FailingSince = %v, want it kept at %v", got.FailingSince, t0)
	}
	if got := record(FetchResult{FetchedAt: t0.Add(9 * time.Hour), NotModified: true}); !got.FailingSince.IsZero() {
		t.Errorf("a not-modified success should clear FailingSince, got %v", got.FailingSince)
	}
	record(FetchResult{FetchedAt: t0.Add(10 * time.Hour), Err: "timed out"})
	if got := record(FetchResult{FetchedAt: t0.Add(11 * time.Hour), FeedTitle: "A"}); !got.FailingSince.IsZero() {
		t.Errorf("a success should clear FailingSince, got %v", got.FailingSince)
	}
	record(FetchResult{FetchedAt: t0.Add(12 * time.Hour), Err: "HTTP 500"})
	got, _ := s.GetFollow(ctx, f.ID)
	got.FeedURL = "https://b.example/feed"
	if err := s.UpdateFollowSettings(ctx, got); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetFollow(ctx, f.ID); !got.FailingSince.IsZero() {
		t.Errorf("a new feed URL should clear FailingSince, got %v", got.FailingSince)
	}
}

func TestRecordFetchStaleFeedURL(t *testing.T) {
	const feedURL = "https://example.com/feed"
	s := openStore(t)
	f := mustCreate(t, s, model.Follow{FeedURL: feedURL, NextFetchAt: t0})
	for name, r := range map[string]FetchResult{
		"success":      {ETag: "old", FeedTitle: "Old", Posts: []model.Post{{GUID: "a"}}},
		"not modified": {NotModified: true, ETag: "old"},
		"error":        {Err: "HTTP 500"},
	} {
		r.FollowID, r.FeedURL, r.FetchedAt, r.NextFetchAt = f.ID, "https://example.com/old-feed", t0, t0.Add(day)
		if err := s.RecordFetch(t.Context(), r); !errors.Is(err, ErrStale) {
			t.Errorf("%s: RecordFetch(other feed URL) = %v, want ErrStale", name, err)
		}
		r.FollowID = 999
		if err := s.RecordFetch(t.Context(), r); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: RecordFetch(unknown follow) = %v, want ErrNotFound", name, err)
		}
	}
	if got := mustGet(t, s, f.ID); !reflect.DeepEqual(got, f) {
		t.Errorf("stale results changed the follow: %+v\nwant %+v", got, f)
	}
	if n := len(allPosts(t, s, f.ID)); n != 0 {
		t.Errorf("stale results stored %d posts", n)
	}

	// the tier NextFetchAt was computed for is checked too, once normalized
	r := FetchResult{FollowID: f.ID, FeedURL: feedURL, Importance: model.Rarely, FetchedAt: t0, NextFetchAt: t0.Add(day)}
	if err := s.RecordFetch(t.Context(), r); !errors.Is(err, ErrStale) {
		t.Errorf("RecordFetch(other tier) = %v, want ErrStale", err)
	}
	if got := mustGet(t, s, f.ID); !reflect.DeepEqual(got, f) {
		t.Errorf("a stale tier changed the follow: %+v\nwant %+v", got, f)
	}
	g := mustCreate(t, s, model.Follow{FeedURL: "https://example.com/other", Importance: model.Occasional})
	record(t, s, FetchResult{FollowID: g.ID, FeedURL: g.FeedURL, Importance: model.Importance(10), FetchedAt: t0})

	for _, u := range []string{feedURL, ""} {
		record(t, s, FetchResult{FollowID: f.ID, FeedURL: u, FetchedAt: t0, ETag: "cur", Posts: []model.Post{{GUID: "a"}}})
	}
	if got := mustGet(t, s, f.ID).ETag; got != "cur" {
		t.Errorf("ETag = %q, want the current feed's", got)
	}
}

// seedPosts creates three follows: a with four posts (two sharing a date),
// b with one and c with none.
func seedPosts(t *testing.T, s *Store) (a, b, c model.Follow) {
	t.Helper()
	a = mustCreate(t, s, model.Follow{FeedURL: "https://a.example/feed"})
	b = mustCreate(t, s, model.Follow{FeedURL: "https://b.example/feed"})
	c = mustCreate(t, s, model.Follow{FeedURL: "https://c.example/feed"})
	record(t, s, FetchResult{FollowID: a.ID, FetchedAt: t0, Posts: []model.Post{
		{GUID: "a-old", PublishedAt: t0.Add(-100 * day)},
		{GUID: "a-tie1", PublishedAt: t0.Add(-10 * day)},
		{GUID: "a-tie2", PublishedAt: t0.Add(-10 * day)},
		{GUID: "a-new", PublishedAt: t0.Add(-1 * day)},
	}})
	record(t, s, FetchResult{FollowID: b.ID, FetchedAt: t0, Posts: []model.Post{{GUID: "b-1", PublishedAt: t0.Add(-2 * day)}}})
	return a, b, c
}

func TestRecentPosts(t *testing.T) {
	s := openStore(t)
	a, b, c := seedPosts(t, s)
	tests := []struct {
		name   string
		follow int64
		limit  int
		want   []string
	}{
		{"newest first, ties in feed order", a.ID, 10, []string{"a-new", "a-tie1", "a-tie2", "a-old"}},
		{"limit", a.ID, 2, []string{"a-new", "a-tie1"}},
		{"zero limit", a.ID, 0, nil},
		{"other follow", b.ID, 10, []string{"b-1"}},
		{"no posts", c.ID, 10, nil},
		{"unknown follow", 999, 10, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			posts, err := s.RecentPosts(t.Context(), tt.follow, tt.limit)
			if err != nil {
				t.Fatal(err)
			}
			if got := guids(posts); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("RecentPosts = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLatestPosts(t *testing.T) {
	s := openStore(t)
	a, b, _ := seedPosts(t, s)
	tests := []struct {
		name      string
		perFollow int
		want      map[int64][]string
	}{
		{"two per follow", 2, map[int64][]string{a.ID: {"a-new", "a-tie1"}, b.ID: {"b-1"}}},
		{"more than stored", 10, map[int64][]string{a.ID: {"a-new", "a-tie1", "a-tie2", "a-old"}, b.ID: {"b-1"}}},
		{"one per follow", 1, map[int64][]string{a.ID: {"a-new"}, b.ID: {"b-1"}}},
		{"zero", 0, map[int64][]string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			latest, err := s.LatestPosts(t.Context(), tt.perFollow)
			if err != nil {
				t.Fatal(err)
			}
			if latest == nil {
				t.Fatal("LatestPosts returned a nil map")
			}
			got := map[int64][]string{}
			for id, posts := range latest {
				for _, p := range posts {
					if p.FollowID != id {
						t.Errorf("post %q of follow %d keyed under %d", p.GUID, p.FollowID, id)
					}
				}
				got[id] = guids(posts)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("LatestPosts = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestActivity(t *testing.T) {
	s := openStore(t)
	a, b, c := seedPosts(t, s)
	tests := []struct {
		name      string
		since     time.Time
		followIDs []int64
		want      map[int64][]time.Time
	}{
		{
			name:  "all follows since a date, newest first",
			since: t0.Add(-30 * day),
			want: map[int64][]time.Time{
				a.ID: {t0.Add(-day), t0.Add(-10 * day), t0.Add(-10 * day)},
				b.ID: {t0.Add(-2 * day)},
			},
		},
		{
			name:  "since is inclusive",
			since: t0.Add(-day),
			want:  map[int64][]time.Time{a.ID: {t0.Add(-day)}},
		},
		{
			name: "zero since means everything",
			want: map[int64][]time.Time{
				a.ID: {t0.Add(-day), t0.Add(-10 * day), t0.Add(-10 * day), t0.Add(-100 * day)},
				b.ID: {t0.Add(-2 * day)},
			},
		},
		{
			name:      "limited to some follows",
			since:     t0.Add(-30 * day),
			followIDs: []int64{b.ID, c.ID, 999},
			want:      map[int64][]time.Time{b.ID: {t0.Add(-2 * day)}},
		},
		{
			name:      "empty follow list",
			since:     t0.Add(-30 * day),
			followIDs: []int64{},
			want:      map[int64][]time.Time{},
		},
		{
			name:  "nothing recent",
			since: t0.Add(time.Hour),
			want:  map[int64][]time.Time{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.Activity(t.Context(), tt.since, tt.followIDs)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Activity = %v\nwant %v", got, tt.want)
			}
		})
	}
}
