package poller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oryanm/stalker/internal/feed"
	"github.com/oryanm/stalker/internal/model"
	"github.com/oryanm/stalker/internal/store"
)

func TestParseInterval(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr string
	}{
		{"", 0, ""},
		{"  ", 0, ""},
		{"0", 0, ""},
		{"0h", 0, ""},
		{"3h", 3 * time.Hour, ""},
		{" 3H ", 3 * time.Hour, ""},
		{"90m", 90 * time.Minute, ""},
		{"1h30m", 90 * time.Minute, ""},
		{"1h 30m", 90 * time.Minute, ""},
		{"2d", 48 * time.Hour, ""},
		{"1d12h", 36 * time.Hour, ""},
		{"1.5d", 36 * time.Hour, ""},
		{"1m", time.Minute, ""},
		{"90s", 2 * time.Minute, ""}, // rounded to the minute
		{"30d", 30 * 24 * time.Hour, ""},
		{"3", 0, "not an interval"},
		{"three hours", 0, "not an interval"},
		{"d", 0, "not an interval"},
		{"2d3", 0, "not an interval"},
		{"-1h", 0, "not an interval"},
		{"20s", 0, "at least 1m"},
		{"31d", 0, "at most 30d"},
	}
	for _, tt := range tests {
		got, err := ParseInterval(tt.in)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("ParseInterval(%q) error = %v, want one containing %q", tt.in, err, tt.wantErr)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("ParseInterval(%q) = %v, %v, want %v", tt.in, got, err, tt.want)
		}
	}
}

func TestFormatInterval(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{0, ""},
		{-time.Hour, ""},
		{10 * time.Minute, "10m"},
		{3 * time.Hour, "3h"},
		{90 * time.Minute, "1h30m"},
		{48 * time.Hour, "2d"},
		{36*time.Hour + 5*time.Minute, "1d12h5m"},
		{149 * time.Second, "2m"}, // rounded to the minute
	}
	for _, tt := range tests {
		got := FormatInterval(tt.in)
		if got != tt.want {
			t.Errorf("FormatInterval(%v) = %q, want %q", tt.in, got, tt.want)
		}
		if tt.in <= 0 {
			continue
		}
		if back, err := ParseInterval(got); err != nil || back != tt.in.Round(time.Minute) {
			t.Errorf("ParseInterval(FormatInterval(%v)) = %v, %v", tt.in, back, err)
		}
	}
}

func TestEffectiveInterval(t *testing.T) {
	limit := 3 * time.Hour
	want := map[model.Importance]time.Duration{
		model.Realtime:   3 * time.Hour,
		model.Frequent:   3 * time.Hour,
		model.Occasional: 4 * time.Hour,
		model.Sometime:   12 * time.Hour,
		model.Rarely:     24 * time.Hour,
	}
	for imp, w := range want {
		if got := EffectiveInterval(imp, limit); got != w {
			t.Errorf("EffectiveInterval(%v, 3h) = %v, want %v", imp, got, w)
		}
		if got := EffectiveInterval(imp, 0); got != Interval(imp) {
			t.Errorf("EffectiveInterval(%v, 0) = %v, want the tier interval", imp, got)
		}
	}
}

func TestFetchResultMinInterval(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	f := model.Follow{ID: 1, FeedURL: "https://a.example/feed", Importance: model.Realtime}
	ok := fetchResult(f, &feed.Result{Title: "A"}, nil, now, 0, 3*time.Hour)
	if want := now.Add(3 * time.Hour); !ok.NextFetchAt.Equal(want) {
		t.Errorf("success NextFetchAt = %v, want %v", ok.NextFetchAt, want)
	}
	f.ErrorCount = 1
	failed := fetchResult(f, nil, errors.New("boom"), now, 0, 3*time.Hour)
	// the second failure in a row doubles the limited interval
	if want := now.Add(6 * time.Hour); !failed.NextFetchAt.Equal(want) {
		t.Errorf("failure NextFetchAt = %v, want %v", failed.NextFetchAt, want)
	}
	rarely := fetchResult(model.Follow{ID: 2, Importance: model.Rarely}, &feed.Result{}, nil, now, 0, 3*time.Hour)
	if want := now.Add(24 * time.Hour); !rarely.NextFetchAt.Equal(want) {
		t.Errorf("a tier slower than the limit keeps its interval: NextFetchAt = %v, want %v", rarely.NextFetchAt, want)
	}
}

func TestSetMinInterval(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	p, _ := newPoller(st, nil, Options{Now: func() time.Time { return now }})
	fetched := now.Add(-time.Hour)

	record := func(f model.Follow, errMsg string) model.Follow {
		t.Helper()
		f = addFollow(t, st, f)
		r := store.FetchResult{FollowID: f.ID, FeedURL: f.FeedURL, Importance: f.Importance, FetchedAt: fetched,
			NextFetchAt: fetched.Add(Interval(f.Importance)), Err: errMsg}
		if err := st.RecordFetch(ctx, r); err != nil {
			t.Fatal(err)
		}
		return getFollow(t, st, f.ID)
	}
	realtime := record(model.Follow{FeedURL: "https://rt.example/feed", Importance: model.Realtime}, "")
	rarely := record(model.Follow{FeedURL: "https://rarely.example/feed", Importance: model.Rarely}, "")
	failing := record(model.Follow{FeedURL: "https://failing.example/feed", Importance: model.Realtime}, "HTTP 500")
	never := addFollow(t, st, model.Follow{FeedURL: "https://new.example/feed", Importance: model.Realtime, NextFetchAt: now})

	if got := p.MinInterval(ctx); got != 0 {
		t.Fatalf("MinInterval before any setting = %v, want 0", got)
	}
	if err := p.SetMinInterval(ctx, 3*time.Hour); err != nil {
		t.Fatal(err)
	}
	if got := p.MinInterval(ctx); got != 3*time.Hour {
		t.Errorf("MinInterval = %v, want 3h", got)
	}
	if v, _ := st.GetSetting(ctx, SettingMinInterval, ""); v != "3h" {
		t.Errorf("stored setting = %q, want 3h", v)
	}
	if got := getFollow(t, st, realtime.ID).NextFetchAt; !got.Equal(fetched.Add(3 * time.Hour)) {
		t.Errorf("realtime NextFetchAt = %v, want its last fetch + 3h", got)
	}
	for _, f := range []model.Follow{rarely, failing, never} {
		if got := getFollow(t, st, f.ID).NextFetchAt; !got.Equal(f.NextFetchAt) {
			t.Errorf("%s NextFetchAt = %v, want it unchanged at %v", f.FeedURL, got, f.NextFetchAt)
		}
	}
	select {
	case <-p.kick:
	default:
		t.Error("changing the limit should kick the scheduler")
	}

	// removing the limit brings the realtime follow back to its own interval
	if err := p.SetMinInterval(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if got := getFollow(t, st, realtime.ID).NextFetchAt; !got.Equal(fetched.Add(10 * time.Minute)) {
		t.Errorf("realtime NextFetchAt after removing the limit = %v, want its last fetch + 10m", got)
	}
	if v, _ := st.GetSetting(ctx, SettingMinInterval, "x"); v != "" {
		t.Errorf("stored setting after removing = %q, want empty", v)
	}

	for _, d := range []time.Duration{time.Second, 31 * 24 * time.Hour, -time.Hour} {
		if err := p.SetMinInterval(ctx, d); err == nil {
			t.Errorf("SetMinInterval(%v) accepted an out-of-range limit", d)
		}
	}
}

func TestMinIntervalIgnoresBadSetting(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	p, _ := newPoller(st, nil, Options{})
	if err := st.SetSetting(ctx, SettingMinInterval, "soon"); err != nil {
		t.Fatal(err)
	}
	if got := p.MinInterval(ctx); got != 0 {
		t.Errorf("MinInterval with an unreadable setting = %v, want 0", got)
	}
}
