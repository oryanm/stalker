package web

import (
	"cmp"
	"fmt"
	"html/template"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/oryanm/stalker/internal/model"
)

// Sort settings, stored under the settingSort key.
const (
	settingSort  = "sort"
	sortRecent   = "recent"
	sortFollowed = "followed"
	sortTitle    = "title"
)

type sortOption struct{ Value, Label string }

var sortOptions = []sortOption{
	{sortRecent, "Recent posts"},
	{sortFollowed, "Recently followed"},
	{sortTitle, "A to Z"},
}

func validSort(v string) bool {
	return slices.ContainsFunc(sortOptions, func(o sortOption) bool { return o.Value == v })
}

const (
	day = 24 * time.Hour
	// activityWindow is how far back the sparkline looks.
	activityWindow = 180 * day
	recentDays     = 60
	sparkPoints    = 60
)

// minutesBetween rounds the distance between two instants to whole minutes the
// way Fraidycat does (seconds first, then half up), so ages match its labels.
func minutesBetween(t, now time.Time) int64 {
	secs := now.Unix() - t.Unix()
	if secs < 0 {
		secs = -secs
	}
	return (secs + 30) / 60
}

// timeAgo is Fraidycat's compact age label. Dates are shown in now's location.
func timeAgo(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	switch mins := minutesBetween(t, now); {
	case mins == 0:
		return "1m"
	case mins <= 45:
		return strconv.FormatInt(mins, 10) + "m"
	case mins <= 90:
		return "1h"
	case mins <= 24*60:
		return strconv.FormatInt((mins+30)/60, 10) + "h"
	case mins <= 48*60:
		return "1d"
	case mins <= 72*60:
		return "2d"
	case mins <= 365*24*60:
		return t.In(now.Location()).Format("Jan 2")
	default:
		return t.In(now.Location()).Format("Jan 2, 2006")
	}
}

// ageClass is the freshness colour class; the zero time (no posts) gets none.
func ageClass(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	switch mins := minutesBetween(t, now); {
	case mins <= 3*24*60:
		return "age-h"
	case mins <= 30*24*60:
		return "age-d"
	default:
		return "age-M"
	}
}

// sparkBuckets counts posts per day over the last 60 days (recent) or, when
// those are empty, per 3 days over the last 180. Index 0 is the oldest bucket.
// It returns nil when there was no activity at all.
func sparkBuckets(times []time.Time, now time.Time) (counts []int, recent bool) {
	daysAgo := func(t time.Time) int {
		d := now.Sub(t)
		if d < 0 {
			return 0
		}
		return int(d / day)
	}
	for _, t := range times {
		if daysAgo(t) < recentDays {
			recent = true
			break
		}
	}
	counts = make([]int, sparkPoints)
	found := false
	for _, t := range times {
		d := daysAgo(t)
		bucket := d
		if !recent {
			bucket = d / 3
		}
		if bucket >= sparkPoints {
			continue
		}
		counts[sparkPoints-1-bucket]++
		found = true
	}
	if !found {
		return nil, false
	}
	return counts, recent
}

const (
	sparkWidth  = 120
	sparkHeight = 20
	sparkPad    = 1.0 // keeps the stroke inside the view box
	sparkTop    = 2.0
)

// sparkline renders the activity graph as inline SVG. The markup is built from
// numbers and constants only, so it is safe to mark as template.HTML.
func sparkline(times []time.Time, now time.Time) template.HTML {
	counts, recent := sparkBuckets(times, now)
	if counts == nil {
		return ""
	}
	class, label, span := "spark-old", "posts every 3 days over the last 6 months", 180
	if recent {
		class, label, span = "spark-recent", "posts per day over the last 2 months", recentDays
	}
	peak := slices.Max(counts)
	total := 0
	for _, c := range counts {
		total += c
	}

	bottom := sparkHeight - sparkPad
	step := (sparkWidth - 2*sparkPad) / float64(len(counts)-1)
	var pts strings.Builder
	for i, c := range counts {
		x := sparkPad + float64(i)*step
		y := bottom - float64(c)/float64(peak)*(bottom-sparkTop)
		if i > 0 {
			pts.WriteByte(' ')
		}
		pts.WriteString(num(x))
		pts.WriteByte(',')
		pts.WriteString(num(y))
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="spark %s" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-label="%s">`,
		class, sparkWidth, sparkHeight, sparkWidth, sparkHeight, label)
	noun := "posts"
	if total == 1 {
		noun = "post"
	}
	fmt.Fprintf(&b, `<title>%d %s in the last %d days</title>`, total, noun, span)
	fmt.Fprintf(&b, `<polygon points="%s,%s %s %s,%s"/>`,
		num(sparkPad), num(bottom), pts.String(), num(sparkWidth-sparkPad), num(bottom))
	fmt.Fprintf(&b, `<polyline points="%s"/>`, pts.String())
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// num formats a coordinate with at most two decimals.
func num(f float64) string {
	return strconv.FormatFloat(math.Round(f*100)/100, 'f', -1, 64)
}

// tierOf snaps a stored importance to a defined tier.
func tierOf(f model.Follow) model.Importance {
	return model.NormalizeImportance(int(f.Importance))
}

// parseTier accepts only the exact importance values of model.Tiers.
func parseTier(s string) (model.Importance, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, false
	}
	for _, t := range model.Tiers {
		if int(t.Importance) == n {
			return t.Importance, true
		}
	}
	return 0, false
}

// parseTags splits on commas and whitespace; emoji sequences survive because
// joiners and variation selectors are neither.
func parseTags(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
}

// row is one follow as listed, also the data of the SSE summary fragment.
type row struct {
	Follow   model.Follow
	Posts    []model.Post // newest first
	Activity []time.Time
	Fetching bool
	Now      time.Time
}

// LastPost is the newest post time, zero when the follow has no posts.
func (r row) LastPost() time.Time {
	t := r.Follow.LastPostAt
	if len(r.Posts) > 0 && r.Posts[0].PublishedAt.After(t) {
		t = r.Posts[0].PublishedAt
	}
	return t
}

// Link is the title link: the site, else the feed, else nothing.
func (r row) Link() string { return cmp.Or(safeURL(r.Follow.URL), safeURL(r.Follow.FeedURL)) }

// postView is a post with the time its age is measured against.
type postView struct {
	model.Post
	Now time.Time
}

func withNow(p model.Post, now time.Time) postView { return postView{p, now} }

func (p postView) Link() string         { return safeURL(p.URL) }
func (p postView) DisplayTitle() string { return cmp.Or(strings.TrimSpace(p.Title), "Untitled") }

// tierSelectData feeds the "tierselect" template.
type tierSelectData struct {
	ID       string
	Selected model.Importance
}

func tierSelect(id string, selected model.Importance) tierSelectData {
	return tierSelectData{id, selected}
}

// errorGrace is how long a follow must keep failing before its row shows a
// warning, so a feed that is down for a few hours (YouTube's feeds return
// false 404s at times) raises no alarm.
const errorGrace = 24 * time.Hour

// shownError is the row's warning text: empty until the follow has been
// failing for errorGrace, except for a follow that has never had a post,
// whose feed may simply be wrong.
func shownError(f model.Follow, now time.Time) string {
	if f.LastError == "" || !f.LastPostAt.IsZero() && now.Sub(f.FailingSince) < errorGrace {
		return ""
	}
	return errorSummary(f)
}

// errorSummary is the error marker's tooltip text.
func errorSummary(f model.Follow) string {
	if f.LastError == "" {
		return ""
	}
	if f.ErrorCount > 1 {
		return fmt.Sprintf("Last %d fetches failed: %s", f.ErrorCount, f.LastError)
	}
	return "Last fetch failed: " + f.LastError
}

// sortRows orders rows by the sort setting; ties fall back to title, then id.
func sortRows(rows []row, by string) {
	slices.SortStableFunc(rows, func(a, b row) int {
		var c int
		switch by {
		case sortFollowed:
			c = b.Follow.CreatedAt.Compare(a.Follow.CreatedAt)
		case sortTitle:
			// titles are the primary key here; the tie-breaks below handle equal ones
		default:
			c = b.LastPost().Compare(a.LastPost())
		}
		if c != 0 {
			return c
		}
		if c := compareTitles(a.Follow, b.Follow); c != 0 {
			return c
		}
		return cmp.Compare(a.Follow.ID, b.Follow.ID)
	})
}

func compareTitles(a, b model.Follow) int {
	return strings.Compare(strings.ToLower(a.DisplayTitle()), strings.ToLower(b.DisplayTitle()))
}

// tagTab is one tag in the header.
type tagTab struct {
	Tag      string
	AgeClass string // freshness of the newest post among the tag's Realtime follows
	Count    int
	Active   bool
}

// buildTagTabs lists HomeTag first, then every other tag sorted.
func buildTagTabs(follows []model.Follow, current string, now time.Time) []tagTab {
	newest := map[string]time.Time{model.HomeTag: {}}
	counts := map[string]int{}
	for _, f := range follows {
		for _, tag := range f.DisplayTags() {
			counts[tag]++
			t, seen := newest[tag]
			if !seen {
				newest[tag] = time.Time{}
			}
			if tierOf(f) == model.Realtime && f.LastPostAt.After(t) {
				newest[tag] = f.LastPostAt
			}
		}
	}
	tags := make([]string, 0, len(newest))
	for tag := range newest {
		if tag != model.HomeTag {
			tags = append(tags, tag)
		}
	}
	slices.Sort(tags)
	tags = slices.Insert(tags, 0, model.HomeTag)

	tabs := make([]tagTab, len(tags))
	for i, tag := range tags {
		tabs[i] = tagTab{Tag: tag, AgeClass: ageClass(newest[tag], now), Count: counts[tag], Active: tag == current}
	}
	return tabs
}

// defaultTag is HomeTag unless it is empty while other tags have follows.
func defaultTag(follows []model.Follow) string {
	first := ""
	for _, f := range follows {
		for _, tag := range f.DisplayTags() {
			if tag == model.HomeTag {
				return model.HomeTag
			}
			if first == "" || tag < first {
				first = tag
			}
		}
	}
	return cmp.Or(first, model.HomeTag)
}

// followsInTag returns the follows listed under tag.
func followsInTag(follows []model.Follow, tag string) []model.Follow {
	var out []model.Follow
	for _, f := range follows {
		if slices.Contains(f.DisplayTags(), tag) {
			out = append(out, f)
		}
	}
	return out
}

// tierTab is one importance tab within a tag.
type tierTab struct {
	Tier   model.Tier
	Count  int
	Active bool
}

// buildTierTabs shows tiers with follows, plus the current one even when empty.
func buildTierTabs(inTag []model.Follow, current model.Importance) []tierTab {
	counts := map[model.Importance]int{}
	for _, f := range inTag {
		counts[tierOf(f)]++
	}
	var tabs []tierTab
	for _, t := range model.Tiers {
		n := counts[t.Importance]
		if n > 0 || t.Importance == current {
			tabs = append(tabs, tierTab{Tier: t, Count: n, Active: t.Importance == current})
		}
	}
	return tabs
}

// defaultTier is the most important tier present, Realtime when there is none.
func defaultTier(inTag []model.Follow) model.Importance {
	for _, t := range model.Tiers {
		for _, f := range inTag {
			if tierOf(f) == t.Importance {
				return t.Importance
			}
		}
	}
	return model.Realtime
}

// homeURL links to a tag, and to a tier within it when tier is non-nil.
func homeURL(tag string, tier *model.Importance) string {
	v := url.Values{}
	if tag != "" {
		v.Set("tag", tag)
	}
	if tier != nil {
		v.Set("tier", strconv.Itoa(int(*tier)))
	}
	if len(v) == 0 {
		return "/"
	}
	return "/?" + v.Encode()
}

// followLocation is where a follow is listed: its first tag and its tier.
func followLocation(f model.Follow) string {
	tier := tierOf(f)
	loc := homeURL(f.DisplayTags()[0], &tier)
	if f.ID != 0 {
		loc += "#follow-" + strconv.FormatInt(f.ID, 10)
	}
	return loc
}

// addURL prefills the add form with the tag and tier being viewed.
func addURL(tag string, tier *model.Importance) string {
	v := url.Values{}
	if tag != "" && tag != model.HomeTag {
		v.Set("tag", tag)
	}
	if tier != nil {
		v.Set("tier", strconv.Itoa(int(*tier)))
	}
	if len(v) == 0 {
		return "/add"
	}
	return "/add?" + v.Encode()
}

// safeURL returns raw when it is an absolute http(s) URL, else "", so feed
// data can never produce javascript: or data: links.
func safeURL(raw string) string {
	u, err := parseHTTPURL(raw)
	if err != nil {
		return ""
	}
	return u.String()
}

func parseHTTPURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errEmptyURL
	}
	if len(raw) > maxURLLen {
		return nil, errLongURL
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errBadURL
	}
	if s := strings.ToLower(u.Scheme); (s != "http" && s != "https") || u.Host == "" {
		return nil, errBadURL
	}
	return u, nil
}

// dialSpan is the oldest age the tuning dial shows; older posts sit at its left end.
const dialSpan = 365 * day

// dialX places an age on the dial's 0 to 1000 scale: a year ago on the left,
// now on the right, logarithmic so the last hours and days get room.
func dialX(age time.Duration) float64 {
	h := max(age.Hours(), 0)
	u := math.Log1p(h) / math.Log1p(dialSpan.Hours())
	return 1000 * (1 - min(u, 1))
}

// dial renders the tuning dial's stations: one glowing tick per follow at the
// age of its latest post, taller for busier follows. Built from numbers and
// constant class names only, so it is safe to mark as template.HTML.
func dial(rows []row) template.HTML {
	peak := 1
	for _, r := range rows {
		peak = max(peak, len(r.Activity))
	}
	var b strings.Builder
	b.WriteString(`<svg class="stations" viewBox="0 0 1000 100" preserveAspectRatio="none" aria-hidden="true">`)
	for _, r := range rows {
		last := r.LastPost()
		if last.IsZero() {
			continue
		}
		x := num(dialX(r.Now.Sub(last)))
		top := 100 - (30 + 62*math.Sqrt(float64(len(r.Activity))/float64(peak)))
		fmt.Fprintf(&b, `<line class="station %s" data-follow="%d" x1="%s" x2="%s" y1="100" y2="%s"/>`,
			ageClass(last, r.Now), r.Follow.ID, x, x, num(top))
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}
