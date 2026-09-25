package store

import (
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/oryanm/stalker/internal/model"
)

func TestNormalizeTags(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil", nil, nil},
		{"only blanks", []string{"", "  ", "\t"}, nil},
		{"trim sort dedupe", []string{" tech ", "art", "tech", "", "art "}, []string{"art", "tech"}},
		{"home tag kept literally", []string{model.HomeTag, "news"}, []string{"news", model.HomeTag}},
		{"case sensitive", []string{"Go", "go"}, []string{"Go", "go"}},
		{"emoji tags", []string{"👨‍💻", "🎨", "👨‍💻"}, []string{"🎨", "👨‍💻"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeTags(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("normalizeTags(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestCreateFollowRoundTrip(t *testing.T) {
	est := time.FixedZone("EST", -5*3600)
	tests := []struct {
		name string
		in   model.Follow
		want model.Follow // ID is filled in by the test
	}{
		{
			name: "zero times get defaults",
			in:   model.Follow{FeedURL: "https://example.com/feed.xml"},
			want: model.Follow{
				FeedURL: "https://example.com/feed.xml", CreatedAt: t0, EditedAt: t0, NextFetchAt: t0,
			},
		},
		{
			name: "every field, times normalized to UTC milliseconds",
			in: model.Follow{
				ID: 42, URL: "https://example.com/", FeedURL: "https://example.com/rss", Title: "Mine",
				FeedTitle: "Example", Description: "A blog", PhotoURL: "https://example.com/me.png",
				Importance: model.Occasional, Tags: []string{" tech ", model.HomeTag, "art", "tech", ""},
				CreatedAt:     time.Date(2024, 6, 24, 10, 28, 9, 123456789, est),
				EditedAt:      time.Date(2025, 1, 2, 3, 4, 5, 999999, time.UTC),
				ETag:          `W/"abc"`,
				LastModified:  "Mon, 02 Jan 2006 15:04:05 GMT",
				LastFetchedAt: t0.Add(-time.Hour),
				NextFetchAt:   t0.Add(time.Hour),
				LastError:     "HTTP 500",
				ErrorCount:    3,
				LastPostAt:    t0.Add(-48 * time.Hour),
			},
			want: model.Follow{
				URL: "https://example.com/", FeedURL: "https://example.com/rss", Title: "Mine",
				FeedTitle: "Example", Description: "A blog", PhotoURL: "https://example.com/me.png",
				Importance: model.Occasional, Tags: []string{"art", "tech", model.HomeTag},
				CreatedAt:     time.Date(2024, 6, 24, 15, 28, 9, 123000000, time.UTC),
				EditedAt:      time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC),
				ETag:          `W/"abc"`,
				LastModified:  "Mon, 02 Jan 2006 15:04:05 GMT",
				LastFetchedAt: t0.Add(-time.Hour),
				NextFetchAt:   t0.Add(time.Hour),
				LastError:     "HTTP 500",
				ErrorCount:    3,
				LastPostAt:    t0.Add(-48 * time.Hour),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openStore(t)
			ctx := t.Context()
			f := tt.in
			if err := s.CreateFollow(ctx, &f); err != nil {
				t.Fatal(err)
			}
			if f.ID <= 0 {
				t.Fatalf("CreateFollow set ID %d, want > 0", f.ID)
			}
			want := tt.want
			want.ID = f.ID
			if !reflect.DeepEqual(f, want) {
				t.Errorf("created follow = %+v\nwant %+v", f, want)
			}

			byID := mustGet(t, s, f.ID)
			byURL, err := s.GetFollowByFeedURL(ctx, f.FeedURL)
			if err != nil {
				t.Fatal(err)
			}
			list, err := s.ListFollows(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(list) != 1 {
				t.Fatalf("ListFollows returned %d follows, want 1", len(list))
			}
			for name, got := range map[string]model.Follow{"GetFollow": byID, "GetFollowByFeedURL": byURL, "ListFollows": list[0]} {
				if !reflect.DeepEqual(got, want) {
					t.Errorf("%s = %+v\nwant %+v", name, got, want)
				}
			}
		})
	}
}

func TestCreateFollowErrors(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	orig := mustCreate(t, s, model.Follow{FeedURL: "https://example.com/feed", Tags: []string{"a"}})

	dup := model.Follow{FeedURL: orig.FeedURL, Title: "dup", Tags: []string{"b"}}
	if err := s.CreateFollow(ctx, &dup); !errors.Is(err, ErrDuplicate) {
		t.Errorf("CreateFollow(duplicate) = %v, want ErrDuplicate", err)
	}
	if dup.ID != 0 || dup.Title != "dup" {
		t.Errorf("failed CreateFollow modified its argument: %+v", dup)
	}

	empty := model.Follow{FeedURL: "  "}
	if err := s.CreateFollow(ctx, &empty); err == nil || errors.Is(err, ErrDuplicate) {
		t.Errorf("CreateFollow(empty feed URL) = %v, want a validation error", err)
	}

	list, err := s.ListFollows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || !reflect.DeepEqual(list[0], orig) {
		t.Errorf("ListFollows = %+v, want only %+v", list, orig)
	}
}

func TestGetFollowNotFound(t *testing.T) {
	s := openStore(t)
	mustCreate(t, s, model.Follow{FeedURL: "https://example.com/feed"})
	if _, err := s.GetFollow(t.Context(), 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetFollow(999) = %v, want ErrNotFound", err)
	}
	if _, err := s.GetFollowByFeedURL(t.Context(), "https://example.com/other"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetFollowByFeedURL(unknown) = %v, want ErrNotFound", err)
	}
}

func TestListFollowsEmpty(t *testing.T) {
	s := openStore(t)
	list, err := s.ListFollows(t.Context())
	if err != nil || len(list) != 0 {
		t.Errorf("ListFollows on an empty store = %v, %v; want no follows", list, err)
	}
}

func TestImportFollows(t *testing.T) {
	tests := []struct {
		name        string
		existing    []string
		in          []model.Follow
		wantAdded   int
		wantSkipped int
		wantTitles  map[string]string // feed URL -> title after import
	}{
		{
			name:       "empty input",
			wantTitles: map[string]string{},
		},
		{
			name: "all new",
			in: []model.Follow{
				{FeedURL: "https://a.example/feed", Title: "A"},
				{FeedURL: "https://b.example/feed", Title: "B"},
			},
			wantAdded:  2,
			wantTitles: map[string]string{"https://a.example/feed": "A", "https://b.example/feed": "B"},
		},
		{
			name:     "existing, repeated and empty feed URLs are skipped",
			existing: []string{"https://a.example/feed"},
			in: []model.Follow{
				{FeedURL: "https://a.example/feed", Title: "A again"},
				{FeedURL: "https://b.example/feed", Title: "B first"},
				{FeedURL: "https://c.example/feed", Title: "C"},
				{FeedURL: "https://b.example/feed", Title: "B second"},
				{FeedURL: "", Title: "no URL"},
			},
			wantAdded:   2,
			wantSkipped: 3,
			wantTitles: map[string]string{
				"https://a.example/feed": "existing",
				"https://b.example/feed": "B first",
				"https://c.example/feed": "C",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openStore(t)
			ctx := t.Context()
			for _, u := range tt.existing {
				mustCreate(t, s, model.Follow{FeedURL: u, Title: "existing"})
			}
			added, skipped, err := s.ImportFollows(ctx, tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if added != tt.wantAdded || skipped != tt.wantSkipped {
				t.Errorf("ImportFollows = (%d added, %d skipped), want (%d, %d)", added, skipped, tt.wantAdded, tt.wantSkipped)
			}
			list, err := s.ListFollows(ctx)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]string{}
			for _, f := range list {
				got[f.FeedURL] = f.Title
			}
			if !reflect.DeepEqual(got, tt.wantTitles) {
				t.Errorf("follows after import = %v, want %v", got, tt.wantTitles)
			}
		})
	}
}

func TestImportFollowsNormalizes(t *testing.T) {
	s := openStore(t)
	created := t0.Add(-365 * 24 * time.Hour)
	_, _, err := s.ImportFollows(t.Context(), []model.Follow{
		{FeedURL: "https://a.example/feed", Importance: model.Rarely, Tags: []string{"b", " a ", "b"}, CreatedAt: created},
		{FeedURL: "https://b.example/feed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.GetFollowByFeedURL(t.Context(), "https://a.example/feed")
	if err != nil {
		t.Fatal(err)
	}
	want := model.Follow{
		ID: a.ID, FeedURL: "https://a.example/feed", Importance: model.Rarely, Tags: []string{"a", "b"},
		CreatedAt: created, EditedAt: t0, NextFetchAt: t0,
	}
	if !reflect.DeepEqual(a, want) {
		t.Errorf("imported follow = %+v\nwant %+v", a, want)
	}
}

func TestUpdateFollowSettings(t *testing.T) {
	later := t0.Add(time.Hour)
	fetchState := model.Follow{
		URL: "https://example.com/", FeedURL: "https://example.com/feed", Title: "Old", FeedTitle: "Feed",
		Description: "desc", PhotoURL: "https://example.com/p.png", Importance: model.Frequent,
		Tags: []string{"old"}, CreatedAt: t0.Add(-time.Hour), EditedAt: t0.Add(-time.Hour),
		ETag: "etag", LastModified: "lm", LastFetchedAt: t0.Add(-time.Minute), NextFetchAt: t0.Add(3 * time.Hour),
		LastError: "HTTP 503", ErrorCount: 2, LastPostAt: t0.Add(-24 * time.Hour),
	}
	tests := []struct {
		name    string
		edit    func(f *model.Follow)
		wantErr error
		want    func(f *model.Follow) // applied to the original follow
	}{
		{
			name: "same feed URL keeps fetch state",
			edit: func(f *model.Follow) {
				f.URL, f.Title, f.Importance = "https://example.com/blog", "New", model.Occasional
				f.Tags, f.EditedAt = []string{"b", " a", "b"}, later
			},
			want: func(f *model.Follow) {
				f.URL, f.Title, f.Importance = "https://example.com/blog", "New", model.Occasional
				f.Tags, f.EditedAt = []string{"a", "b"}, later
			},
		},
		{
			name: "more important tier is fetched now",
			edit: func(f *model.Follow) { f.Importance, f.EditedAt = model.Realtime, later },
			want: func(f *model.Follow) { f.Importance, f.EditedAt, f.NextFetchAt = model.Realtime, later, t0 },
		},
		{
			name: "new feed URL resets fetch state",
			edit: func(f *model.Follow) { f.FeedURL, f.EditedAt = "https://example.com/atom", later },
			want: func(f *model.Follow) {
				f.FeedURL, f.EditedAt = "https://example.com/atom", later
				f.ETag, f.LastModified, f.NextFetchAt, f.LastError, f.ErrorCount = "", "", t0, "", 0
			},
		},
		{
			name: "zero EditedAt defaults to now",
			edit: func(f *model.Follow) { f.EditedAt = time.Time{} },
			want: func(f *model.Follow) { f.EditedAt = t0 },
		},
		{
			name: "fields outside the settings are ignored",
			edit: func(f *model.Follow) {
				f.FeedTitle, f.Description, f.ETag, f.ErrorCount, f.CreatedAt = "x", "x", "x", 99, later
				f.EditedAt = later
			},
			want: func(f *model.Follow) { f.EditedAt = later },
		},
		{
			name: "clearing tags",
			edit: func(f *model.Follow) { f.Tags, f.EditedAt = []string{" "}, later },
			want: func(f *model.Follow) { f.Tags, f.EditedAt = nil, later },
		},
		{
			name:    "unknown id",
			edit:    func(f *model.Follow) { f.ID = 999 },
			wantErr: ErrNotFound,
		},
		{
			name:    "feed URL of another follow",
			edit:    func(f *model.Follow) { f.FeedURL, f.Tags = "https://other.example/feed", []string{"changed"} },
			wantErr: ErrDuplicate,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openStore(t)
			orig := mustCreate(t, s, fetchState)
			other := mustCreate(t, s, model.Follow{FeedURL: "https://other.example/feed", Tags: []string{"x"}})

			edited := orig
			edited.Tags = slices.Clone(orig.Tags)
			tt.edit(&edited)
			err := s.UpdateFollowSettings(t.Context(), edited)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("UpdateFollowSettings = %v, want %v", err, tt.wantErr)
			}

			want := orig
			if tt.want != nil {
				tt.want(&want)
			}
			if got := mustGet(t, s, orig.ID); !reflect.DeepEqual(got, want) {
				t.Errorf("follow after update = %+v\nwant %+v", got, want)
			}
			if got := mustGet(t, s, other.ID); !reflect.DeepEqual(got, other) {
				t.Errorf("other follow changed: %+v, want %+v", got, other)
			}
		})
	}
}

func TestUpdateFollowSettingsReschedule(t *testing.T) {
	tests := []struct {
		name     string
		from, to model.Importance
		next     time.Time // stored NextFetchAt
		want     time.Time
	}{
		{"promotion waits no longer", model.Rarely, model.Realtime, t0.Add(30 * time.Hour), t0},
		{"promotion by one tier", model.Sometime, model.Occasional, t0.Add(15 * time.Hour), t0},
		{"promotion keeps an earlier schedule", model.Rarely, model.Frequent, t0.Add(-time.Minute), t0.Add(-time.Minute)},
		{"unnormalized value of the same tier", model.Occasional, model.Importance(10), t0.Add(5 * time.Hour), t0.Add(5 * time.Hour)},
		{"same tier", model.Frequent, model.Frequent, t0.Add(time.Hour), t0.Add(time.Hour)},
		{"demotion", model.Realtime, model.Rarely, t0.Add(10 * time.Minute), t0.Add(10 * time.Minute)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openStore(t)
			f := mustCreate(t, s, model.Follow{FeedURL: "https://example.com/feed", Importance: tt.from, NextFetchAt: tt.next})
			f.Importance = tt.to
			if err := s.UpdateFollowSettings(t.Context(), f); err != nil {
				t.Fatal(err)
			}
			if got := mustGet(t, s, f.ID).NextFetchAt; !got.Equal(tt.want) {
				t.Errorf("NextFetchAt = %v, want %v", got, tt.want)
			}
			due, err := s.DueFollows(t.Context(), t0, 10)
			if err != nil {
				t.Fatal(err)
			}
			if wantDue := !tt.want.After(t0); (len(due) == 1) != wantDue {
				t.Errorf("due at t0: %d follows, want due %v", len(due), wantDue)
			}
		})
	}
}

func TestSameFeedUnderAnotherURL(t *testing.T) {
	const (
		channelPage = "https://www.youtube.com/channel/UCY1kMZp36IQSyNx_9h4mpCg"
		channelFeed = "https://www.youtube.com/feeds/videos.xml?channel_id=UCY1kMZp36IQSyNx_9h4mpCg"
		oldReddit   = "https://old.reddit.com/r/golang/top/.rss"
		newReddit   = "https://www.reddit.com/r/golang/top/.rss"
	)
	s := openStore(t)
	ctx := t.Context()
	yt := mustCreate(t, s, model.Follow{FeedURL: channelPage, Title: "Mark Rober"})
	reddit := mustCreate(t, s, model.Follow{FeedURL: oldReddit})
	other := mustCreate(t, s, model.Follow{FeedURL: "https://blog.example/feed"})

	for _, u := range []string{channelFeed, newReddit, "https://m.youtube.com/channel/UCY1kMZp36IQSyNx_9h4mpCg/videos"} {
		f := model.Follow{FeedURL: u}
		if err := s.CreateFollow(ctx, &f); !errors.Is(err, ErrDuplicate) {
			t.Errorf("CreateFollow(%q) = %v, want ErrDuplicate", u, err)
		}
	}

	added, skipped, err := s.ImportFollows(ctx, []model.Follow{
		{FeedURL: channelFeed},
		{FeedURL: "https://www.youtube.com/playlist?list=PL1"},
		{FeedURL: "https://www.youtube.com/feeds/videos.xml?playlist_id=PL1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 || skipped != 2 {
		t.Errorf("ImportFollows = (%d added, %d skipped), want (1, 2)", added, skipped)
	}

	for u, want := range map[string]int64{channelPage: yt.ID, channelFeed: yt.ID, newReddit: reddit.ID} {
		got, err := s.GetFollowByFeedURL(ctx, u)
		if err != nil || got.ID != want {
			t.Errorf("GetFollowByFeedURL(%q) = follow %d, %v; want follow %d", u, got.ID, err, want)
		}
	}
	if _, err := s.GetFollowByFeedURL(ctx, "https://www.youtube.com/channel/UCother"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetFollowByFeedURL(unknown channel) = %v, want ErrNotFound", err)
	}

	moved := other
	moved.FeedURL = channelFeed
	if err := s.UpdateFollowSettings(ctx, moved); !errors.Is(err, ErrDuplicate) {
		t.Errorf("UpdateFollowSettings(another follow's channel) = %v, want ErrDuplicate", err)
	}
	// a follow may switch between the forms of its own feed
	yt.FeedURL = channelFeed
	if err := s.UpdateFollowSettings(ctx, yt); err != nil {
		t.Errorf("UpdateFollowSettings(own channel feed) = %v, want nil", err)
	}
	if got := mustGet(t, s, yt.ID).FeedURL; got != channelFeed {
		t.Errorf("FeedURL = %q, want %q", got, channelFeed)
	}
}

func TestSetNextFetch(t *testing.T) {
	ctx := t.Context()
	st := openStore(t)
	a := model.Follow{FeedURL: "https://a.example/feed"}
	b := model.Follow{FeedURL: "https://b.example/feed"}
	for _, f := range []*model.Follow{&a, &b} {
		if err := st.CreateFollow(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Date(2026, 9, 25, 15, 0, 0, 0, time.UTC)
	if err := st.SetNextFetch(ctx, map[int64]time.Time{a.ID: at, 999: at}); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetFollow(ctx, a.ID); !got.NextFetchAt.Equal(at) {
		t.Errorf("a NextFetchAt = %v, want %v", got.NextFetchAt, at)
	}
	if got, _ := st.GetFollow(ctx, b.ID); !got.NextFetchAt.Equal(b.NextFetchAt) {
		t.Errorf("b NextFetchAt = %v, want it unchanged", got.NextFetchAt)
	}
	if err := st.SetNextFetch(ctx, nil); err != nil {
		t.Errorf("SetNextFetch(nil) = %v", err)
	}
}

func TestDeleteFollow(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	a := mustCreate(t, s, model.Follow{FeedURL: "https://a.example/feed", Tags: []string{"x", "y"}})
	b := mustCreate(t, s, model.Follow{FeedURL: "https://b.example/feed", Tags: []string{"x"}})
	for _, id := range []int64{a.ID, b.ID} {
		err := s.RecordFetch(ctx, FetchResult{FollowID: id, FetchedAt: t0, Posts: []model.Post{{GUID: "1"}, {GUID: "2"}}})
		if err != nil {
			t.Fatal(err)
		}
	}

	if err := s.DeleteFollow(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetFollow(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetFollow(deleted) = %v, want ErrNotFound", err)
	}
	for table, want := range map[string]int{"posts": 0, "follow_tags": 0} {
		var n int
		if err := s.r.QueryRowContext(ctx, "SELECT count(*) FROM "+table+" WHERE follow_id = ?", a.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Errorf("%d %s rows left for the deleted follow, want %d", n, table, want)
		}
	}
	if err := s.DeleteFollow(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteFollow(deleted) = %v, want ErrNotFound", err)
	}

	if got := mustGet(t, s, b.ID); !reflect.DeepEqual(got.Tags, []string{"x"}) {
		t.Errorf("other follow tags = %q, want [x]", got.Tags)
	}
	if posts, err := s.RecentPosts(ctx, b.ID, 10); err != nil || len(posts) != 2 {
		t.Errorf("other follow posts = %d, %v; want 2", len(posts), err)
	}

	c := mustCreate(t, s, model.Follow{FeedURL: a.FeedURL})
	if c.ID <= b.ID {
		t.Errorf("new follow reused id %d (max was %d)", c.ID, b.ID)
	}
}

func TestDueFollows(t *testing.T) {
	s := openStore(t)
	var ids []int64
	for _, next := range []time.Duration{time.Hour, -time.Hour, 0, -2 * time.Hour} {
		f := mustCreate(t, s, model.Follow{FeedURL: "https://example.com/" + next.String(), NextFetchAt: t0.Add(next)})
		ids = append(ids, f.ID)
	}
	tests := []struct {
		name  string
		now   time.Time
		limit int
		want  []int64
	}{
		{"oldest due first, due at now included", t0, 10, []int64{ids[3], ids[1], ids[2]}},
		{"limit", t0, 2, []int64{ids[3], ids[1]}},
		{"zero limit", t0, 0, nil},
		{"negative limit", t0, -1, nil},
		{"nothing due", t0.Add(-3 * time.Hour), 10, nil},
		{"all due", t0.Add(time.Hour), 10, []int64{ids[3], ids[1], ids[2], ids[0]}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			due, err := s.DueFollows(t.Context(), tt.now, tt.limit)
			if err != nil {
				t.Fatal(err)
			}
			var got []int64
			for _, f := range due {
				got = append(got, f.ID)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("DueFollows = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestImportFollowsDoesNotUseUpIDs(t *testing.T) {
	s := openStore(t)
	in := []model.Follow{{FeedURL: "https://a.example/feed"}, {FeedURL: "https://b.example/feed"}}
	for range 3 {
		if _, _, err := s.ImportFollows(t.Context(), in); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateFollow(t.Context(), &model.Follow{FeedURL: "https://a.example/feed"}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("CreateFollow duplicate: err = %v, want ErrDuplicate", err)
	}
	f := mustCreate(t, s, model.Follow{FeedURL: "https://c.example/feed"})
	if f.ID != 3 {
		t.Errorf("ID after re-imports = %d, want 3", f.ID)
	}
}
