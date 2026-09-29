package web

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oryanm/stalker/internal/model"
)

var now = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

const devTag = "👨‍💻"

func TestTimeAgo(t *testing.T) {
	tests := []struct {
		ago  time.Duration
		want string
	}{
		{0, "1m"},
		{29 * time.Second, "1m"},
		{30 * time.Second, "1m"},
		{89 * time.Second, "1m"},
		{90 * time.Second, "2m"},
		{45 * time.Minute, "45m"},
		{45*time.Minute + 29*time.Second, "45m"},
		{45*time.Minute + 30*time.Second, "1h"},
		{90 * time.Minute, "1h"},
		{91 * time.Minute, "2h"},
		{149 * time.Minute, "2h"},
		{150 * time.Minute, "3h"},
		{24 * time.Hour, "24h"},
		{24*time.Hour + time.Minute, "1d"},
		{48 * time.Hour, "1d"},
		{48*time.Hour + time.Minute, "2d"},
		{72 * time.Hour, "2d"},
		{72*time.Hour + time.Minute, "Sep 21"},
		{200 * day, "Mar 8"},
		{365 * day, "Sep 24"},
		{365*day + time.Minute, "Sep 24, 2025"},
		{3 * 365 * day, "Sep 25, 2023"},
		{-10 * time.Minute, "10m"}, // clock skew counts like the past
	}
	for _, tt := range tests {
		if got := timeAgo(now.Add(-tt.ago), now); got != tt.want {
			t.Errorf("timeAgo(now - %v) = %q, want %q", tt.ago, got, tt.want)
		}
	}
	if got := timeAgo(time.Time{}, now); got != "" {
		t.Errorf("timeAgo(zero) = %q, want empty", got)
	}
}

func TestTimeAgoUsesNowsLocation(t *testing.T) {
	toronto := time.FixedZone("EDT", -4*3600)
	post := time.Date(2026, 9, 1, 2, 0, 0, 0, time.UTC) // still Aug 31 in Toronto
	if got := timeAgo(post, now.In(toronto)); got != "Aug 31" {
		t.Errorf("got %q, want Aug 31", got)
	}
	if got := timeAgo(post, now); got != "Sep 1" {
		t.Errorf("got %q, want Sep 1", got)
	}
}

func TestAgeClass(t *testing.T) {
	tests := []struct {
		ago  time.Duration
		want string
	}{
		{0, "age-h"},
		{3 * day, "age-h"},
		{3*day + 29*time.Second, "age-h"}, // rounds to the same minute
		{3*day + time.Minute, "age-d"},
		{30 * day, "age-d"},
		{30*day + time.Minute, "age-M"},
		{400 * day, "age-M"},
		{-time.Hour, "age-h"},
	}
	for _, tt := range tests {
		if got := ageClass(now.Add(-tt.ago), now); got != tt.want {
			t.Errorf("ageClass(now - %v) = %q, want %q", tt.ago, got, tt.want)
		}
	}
	if got := ageClass(time.Time{}, now); got != "" {
		t.Errorf("ageClass(zero) = %q, want empty", got)
	}
}

func TestSparkBuckets(t *testing.T) {
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	tests := []struct {
		name       string
		times      []time.Time
		wantRecent bool
		want       map[int]int // index -> count; every other bucket is 0
	}{
		{name: "no activity", times: nil},
		{name: "only older than 180 days", times: []time.Time{ago(180 * day), ago(400 * day)}},
		{
			name:       "today is the rightmost point",
			times:      []time.Time{ago(time.Hour), ago(23 * time.Hour)},
			wantRecent: true,
			want:       map[int]int{59: 2},
		},
		{
			name:       "per day over 60 days",
			times:      []time.Time{ago(24 * time.Hour), ago(59*day + 23*time.Hour), ago(2*day + time.Hour)},
			wantRecent: true,
			want:       map[int]int{58: 1, 57: 1, 0: 1},
		},
		{
			name:       "recent activity ignores older posts",
			times:      []time.Time{ago(day + time.Hour), ago(100 * day)},
			wantRecent: true,
			want:       map[int]int{58: 1},
		},
		{
			name:  "no recent activity falls back to 3-day buckets",
			times: []time.Time{ago(60 * day), ago(61 * day), ago(62*day + time.Hour), ago(63 * day), ago(179 * day)},
			want:  map[int]int{39: 3, 38: 1, 0: 1},
		},
		{
			name:       "future posts count as today",
			times:      []time.Time{now.Add(time.Hour)},
			wantRecent: true,
			want:       map[int]int{59: 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			counts, recent := sparkBuckets(tt.times, now)
			if tt.want == nil {
				if counts != nil {
					t.Fatalf("got %v, want nil", counts)
				}
				return
			}
			if recent != tt.wantRecent {
				t.Errorf("recent = %v, want %v", recent, tt.wantRecent)
			}
			if len(counts) != 60 {
				t.Fatalf("got %d buckets, want 60", len(counts))
			}
			for i, c := range counts {
				if c != tt.want[i] {
					t.Errorf("bucket %d = %d, want %d", i, c, tt.want[i])
				}
			}
		})
	}
}

func TestSparkline(t *testing.T) {
	if got := sparkline(nil, now); got != "" {
		t.Errorf("no activity rendered %q", got)
	}

	recent := string(sparkline([]time.Time{now.Add(-time.Hour), now.Add(-time.Hour), now.Add(-10 * day)}, now))
	for _, want := range []string{
		`<svg class="spark spark-recent" width="120" height="20" viewBox="0 0 120 20"`,
		`<title>3 posts in the last 60 days</title>`,
		`<polyline points="1,19 `,
		` 119,2"/>`, // the busiest day reaches the top, on the right
		`<polygon points="1,19 1,19 `,
	} {
		if !strings.Contains(recent, want) {
			t.Errorf("recent sparkline lacks %q:\n%s", want, recent)
		}
	}
	points := regexp.MustCompile(`<polyline points="([^"]*)"`).FindStringSubmatch(recent)
	if points == nil || len(strings.Fields(points[1])) != 60 {
		t.Errorf("want 60 points, got %v", points)
	}
	// only numbers, commas and spaces reach the points attribute
	if !regexp.MustCompile(`^[0-9., ]+$`).MatchString(points[1]) {
		t.Errorf("points contain unexpected characters: %q", points[1])
	}

	old := string(sparkline([]time.Time{now.Add(-100 * day)}, now))
	if !strings.Contains(old, `class="spark spark-old"`) || !strings.Contains(old, "1 post in the last 180 days") {
		t.Errorf("old sparkline = %s", old)
	}
}

func TestParseTier(t *testing.T) {
	tests := []struct {
		in   string
		want model.Importance
		ok   bool
	}{
		{"0", model.Realtime, true},
		{"1", model.Frequent, true},
		{" 7 ", model.Occasional, true},
		{"30", model.Sometime, true},
		{"365", model.Rarely, true},
		{"2", 0, false},
		{"-1", 0, false},
		{"", 0, false},
		{"seven", 0, false},
		{"1e3", 0, false},
	}
	for _, tt := range tests {
		got, ok := parseTier(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("parseTier(%q) = %v, %v; want %v, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestParseTags(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"  ", nil},
		{"go", []string{"go"}},
		{"a, b c,,d\te\nf", []string{"a", "b", "c", "d", "e", "f"}},
		{devTag + " 📹,🏠", []string{devTag, "📹", "🏠"}},
		{"☂️ 👩🏽‍🔬", []string{"☂️", "👩🏽‍🔬"}}, // variation selector and skin tone survive
	}
	for _, tt := range tests {
		if got := parseTags(tt.in); !slices.Equal(got, tt.want) {
			t.Errorf("parseTags(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSafeURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://example.com/a?b=c", "https://example.com/a?b=c"},
		{" http://example.com ", "http://example.com"},
		{"HTTPS://example.com/", "https://example.com/"},
		{"javascript:alert(1)", ""},
		{"JavaScript:alert(1)", ""},
		{"data:text/html,<script>", ""},
		{"//example.com/x", ""},
		{"/relative", ""},
		{"https://", ""},
		{"mailto:a@example.com", ""},
		{"", ""},
		{"https://example.com/" + strings.Repeat("a", maxURLLen), ""},
	}
	for _, tt := range tests {
		if got := safeURL(tt.in); got != tt.want {
			t.Errorf("safeURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func follow(id int64, title string, imp model.Importance, tags ...string) model.Follow {
	return model.Follow{ID: id, FeedURL: "https://example.com/" + title, Title: title, Importance: imp, Tags: tags}
}

func TestSortRows(t *testing.T) {
	mk := func(id int64, title string, created, lastPost time.Time) row {
		f := follow(id, title, model.Frequent)
		f.CreatedAt, f.LastPostAt = created, lastPost
		return row{Follow: f}
	}
	rows := []row{
		mk(1, "beta", now.Add(-3*day), now.Add(-time.Hour)),
		mk(2, "Alpha", now.Add(-1*day), time.Time{}),
		mk(3, "gamma", now.Add(-2*day), now.Add(-2*time.Hour)),
		mk(4, "delta", now.Add(-2*day), now.Add(-time.Hour)),
		mk(5, "alpha", now.Add(-5*day), time.Time{}),
	}
	// a post newer than LastPostAt (not yet reflected) still counts
	rows[2].Posts = []model.Post{{PublishedAt: now.Add(-time.Minute)}}

	tests := []struct {
		by   string
		want []int64
	}{
		{sortRecent, []int64{3, 1, 4, 2, 5}},
		{"", []int64{3, 1, 4, 2, 5}},
		{sortFollowed, []int64{2, 4, 3, 1, 5}},
		{sortTitle, []int64{2, 5, 1, 4, 3}},
	}
	for _, tt := range tests {
		got := slices.Clone(rows)
		sortRows(got, tt.by)
		var ids []int64
		for _, r := range got {
			ids = append(ids, r.Follow.ID)
		}
		if !slices.Equal(ids, tt.want) {
			t.Errorf("sort %q = %v, want %v", tt.by, ids, tt.want)
		}
	}
}

func TestBuildTagTabs(t *testing.T) {
	rt := func(f model.Follow, lastPost time.Time) model.Follow { f.LastPostAt = lastPost; return f }
	follows := []model.Follow{
		rt(follow(1, "a", model.Realtime, "📹"), now.Add(-time.Hour)),
		rt(follow(2, "b", model.Frequent, "📹", "news"), now.Add(-time.Minute)), // not Realtime: no colour
		rt(follow(3, "c", model.Realtime, "news"), now.Add(-10*day)),
		follow(4, "d", model.Rarely, devTag),
		rt(follow(5, "e", model.Realtime), now.Add(-100*day)), // untagged: 🏠
	}
	tabs := buildTagTabs(follows, "news", now)
	want := []tagTab{
		{Tag: model.HomeTag, AgeClass: "age-M", Count: 1},
		{Tag: "news", AgeClass: "age-d", Count: 2, Active: true},
		{Tag: devTag, Count: 1},
		{Tag: "📹", AgeClass: "age-h", Count: 2},
	}
	if !slices.Equal(tabs, want) {
		t.Errorf("got  %+v\nwant %+v", tabs, want)
	}

	// 🏠 leads even when nothing is listed there
	tabs = buildTagTabs(follows[:1], "📹", now)
	if len(tabs) != 2 || tabs[0].Tag != model.HomeTag || tabs[0].Count != 0 || tabs[1].Tag != "📹" {
		t.Errorf("got %+v", tabs)
	}
}

func TestDefaultTag(t *testing.T) {
	tests := []struct {
		name    string
		follows []model.Follow
		want    string
	}{
		{"no follows", nil, model.HomeTag},
		{"untagged follow", []model.Follow{follow(1, "a", 0, "x"), follow(2, "b", 0)}, model.HomeTag},
		{"explicit home tag", []model.Follow{follow(1, "a", 0, "x", model.HomeTag)}, model.HomeTag},
		{"first tag when home is empty", []model.Follow{follow(1, "a", 0, "z"), follow(2, "b", 0, "📹", "m")}, "m"},
	}
	for _, tt := range tests {
		if got := defaultTag(tt.follows); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestFollowsInTag(t *testing.T) {
	follows := []model.Follow{
		follow(1, "a", 0),
		follow(2, "b", 0, model.HomeTag, "x"),
		follow(3, "c", 0, "x"),
	}
	ids := func(fs []model.Follow) (out []int64) {
		for _, f := range fs {
			out = append(out, f.ID)
		}
		return out
	}
	if got := ids(followsInTag(follows, model.HomeTag)); !slices.Equal(got, []int64{1, 2}) {
		t.Errorf("home = %v", got)
	}
	if got := ids(followsInTag(follows, "x")); !slices.Equal(got, []int64{2, 3}) {
		t.Errorf("x = %v", got)
	}
	if got := followsInTag(follows, "nope"); got != nil {
		t.Errorf("unknown tag = %v", got)
	}
}

func TestTierTabs(t *testing.T) {
	inTag := []model.Follow{
		follow(1, "a", model.Occasional),
		follow(2, "b", model.Occasional),
		follow(3, "c", model.Rarely),
		follow(4, "d", 500), // normalized to Rarely
	}
	if got := defaultTier(inTag); got != model.Occasional {
		t.Errorf("defaultTier = %v, want Occasional", got)
	}
	if got := defaultTier(nil); got != model.Realtime {
		t.Errorf("defaultTier(nil) = %v, want Realtime", got)
	}

	type tab struct {
		imp    model.Importance
		count  int
		active bool
	}
	flatten := func(tabs []tierTab) (out []tab) {
		for _, t := range tabs {
			out = append(out, tab{t.Tier.Importance, t.Count, t.Active})
		}
		return out
	}
	tests := []struct {
		current model.Importance
		want    []tab
	}{
		// every tier is shown, the empty ones too
		{model.Occasional, []tab{{model.Realtime, 0, false}, {model.Frequent, 0, false}, {model.Occasional, 2, true}, {model.Sometime, 0, false}, {model.Rarely, 2, false}}},
		{model.Sometime, []tab{{model.Realtime, 0, false}, {model.Frequent, 0, false}, {model.Occasional, 2, false}, {model.Sometime, 0, true}, {model.Rarely, 2, false}}},
	}
	for _, tt := range tests {
		if got := flatten(buildTierTabs(inTag, tt.current)); !slices.Equal(got, tt.want) {
			t.Errorf("current %v: got %v, want %v", tt.current, got, tt.want)
		}
	}
}

func TestURLs(t *testing.T) {
	frequent := model.Frequent
	tests := []struct{ got, want string }{
		{homeURL("", nil), "/"},
		{homeURL("📹", nil), "/?tag=%F0%9F%93%B9"},
		{homeURL("a&b", &frequent), "/?tag=a%26b&tier=1"},
		{addURL(model.HomeTag, &frequent), "/add?tier=1"},
		{addURL("news", nil), "/add?tag=news"},
		{addURL("", nil), "/add"},
		{followLocation(follow(7, "x", model.Occasional, "b", "a")), "/?tag=b&tier=7#follow-7"},
		{followLocation(follow(8, "x", 3)), "/?tag=%F0%9F%8F%A0&tier=1#follow-8"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("got %q, want %q", tt.got, tt.want)
		}
	}
}

func TestRowLink(t *testing.T) {
	tests := []struct {
		site, feed, want string
	}{
		{"https://site.example/", "https://site.example/feed", "https://site.example/"},
		{"", "https://site.example/feed", "https://site.example/feed"},
		{"javascript:alert(1)", "https://site.example/feed", "https://site.example/feed"},
		{"javascript:alert(1)", "data:x", ""},
	}
	for _, tt := range tests {
		r := row{Follow: model.Follow{URL: tt.site, FeedURL: tt.feed}}
		if got := r.Link(); got != tt.want {
			t.Errorf("Link(%q, %q) = %q, want %q", tt.site, tt.feed, got, tt.want)
		}
	}
}

func TestErrorSummary(t *testing.T) {
	tests := []struct {
		f    model.Follow
		want string
	}{
		{model.Follow{}, ""},
		{model.Follow{LastError: "HTTP 404", ErrorCount: 1}, "Last fetch failed: HTTP 404"},
		{model.Follow{LastError: "HTTP 404", ErrorCount: 3}, "Last 3 fetches failed: HTTP 404"},
	}
	for _, tt := range tests {
		if got := errorSummary(tt.f); got != tt.want {
			t.Errorf("got %q, want %q", got, tt.want)
		}
	}
}

func TestIsLocalPath(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"/", true},
		{"/?tag=x&tier=1", true},
		{"/settings", true},
		{"", false},
		{"//evil.example/", false},
		{`/\evil.example`, false},
		{"https://evil.example/", false},
		{"settings", false},
	}
	for _, tt := range tests {
		if got := isLocalPath(tt.in); got != tt.want {
			t.Errorf("isLocalPath(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestDial(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	if got := dialX(0); got != 1000 {
		t.Errorf("dialX(now) = %v, want 1000", got)
	}
	if got := dialX(2 * dialSpan); got != 0 {
		t.Errorf("dialX(beyond the span) = %v, want 0", got)
	}
	if day, week := dialX(24*time.Hour), dialX(7*24*time.Hour); day <= week {
		t.Errorf("a day ago (%v) should sit right of a week ago (%v)", day, week)
	}

	rows := []row{
		{Follow: model.Follow{ID: 7, LastPostAt: now}, Activity: []time.Time{now, now}, Now: now},
		{Follow: model.Follow{ID: 8, LastPostAt: now.Add(-60 * day)}, Now: now},
		{Follow: model.Follow{ID: 9}, Now: now}, // no posts: no station
	}
	got := string(dial(rows))
	for _, want := range []string{
		`<svg class="stations" viewBox="0 0 1000 100" preserveAspectRatio="none" aria-hidden="true">`,
		`<line class="station age-h" data-follow="7" x1="1000" x2="1000" y1="100" y2="8"/>`,
		`<line class="station age-M" data-follow="8" `,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dial lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, `data-follow="9"`) {
		t.Errorf("a follow without posts got a station:\n%s", got)
	}
}

func TestRowIcon(t *testing.T) {
	tests := []struct {
		name        string
		follow      model.Follow
		icon, first string
	}{
		{"feed image first", model.Follow{Title: "blog", PhotoURL: "https://a.example/p.png", IconURL: "https://a.example/i.png"}, "https://a.example/p.png", "B"},
		{"home page icon", model.Follow{Title: "Blog", IconURL: "https://a.example/i.png"}, "https://a.example/i.png", "B"},
		{"http upgraded for the CSP", model.Follow{Title: "Blog", IconURL: "http://a.example/i.png"}, "https://a.example/i.png", "B"},
		{"unsafe scheme dropped", model.Follow{Title: "Blog", IconURL: "javascript:alert(1)"}, "", "B"},
		{"initial skips symbols", model.Follow{Title: "📹 @ viva"}, "", "V"},
		{"initial of the host", model.Follow{FeedURL: "https://www.example.com/feed"}, "", "E"},
		{"no letters", model.Follow{Title: "📹"}, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := row{Follow: tt.follow}
			if got := r.Icon(); got != tt.icon {
				t.Errorf("Icon() = %q, want %q", got, tt.icon)
			}
			if got := r.Initial(); got != tt.first {
				t.Errorf("Initial() = %q, want %q", got, tt.first)
			}
		})
	}
}
