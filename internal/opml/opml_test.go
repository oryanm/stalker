package opml

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oryanm/stalker/internal/model"
)

// devTag is the ZWJ sequence 👨‍💻 spelled out so the test pins the exact bytes.
const devTag = "\U0001F468‍\U0001F4BB"

var edt = time.FixedZone("EDT", -4*3600)

func entryEqual(a, b Entry) bool {
	return a.Title == b.Title &&
		a.Text == b.Text &&
		a.FeedURL == b.FeedURL &&
		a.SiteURL == b.SiteURL &&
		a.Importance == b.Importance &&
		slices.Equal(a.Tags, b.Tags) &&
		a.CreatedAt.Equal(b.CreatedAt)
}

func formatEntries(es []Entry) string {
	var b strings.Builder
	for _, e := range es {
		fmt.Fprintf(&b, "\n\t%+v", e)
	}
	return b.String()
}

func parseString(t *testing.T, s string) []Entry {
	t.Helper()
	entries, err := Parse(strings.NewReader(s))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return entries
}

func parseFraidycatExport(t *testing.T) []Entry {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "testdata", "fraidycat-sample.opml"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	entries, err := Parse(f)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return entries
}

func TestParseFraidycatExport(t *testing.T) {
	entries := parseFraidycatExport(t)
	if len(entries) != 14 {
		t.Fatalf("got %d entries, want 14", len(entries))
	}

	importances := map[model.Importance]int{}
	tags := map[string]int{}
	byFeed := map[string]Entry{}
	for _, e := range entries {
		importances[e.Importance]++
		for _, tag := range e.Tags {
			tags[tag]++
		}
		byFeed[e.FeedURL] = e
		if e.SiteURL == "" || e.Text == "" || e.CreatedAt.IsZero() {
			t.Errorf("incomplete entry: %+v", e)
		}
	}
	wantImportances := map[model.Importance]int{model.Frequent: 6, model.Occasional: 5, model.Sometime: 1, model.Rarely: 2}
	if fmt.Sprint(importances) != fmt.Sprint(wantImportances) {
		t.Errorf("importance counts = %v, want %v", importances, wantImportances)
	}
	wantTags := map[string]int{"📹": 7, devTag: 4, model.HomeTag: 3}
	if fmt.Sprint(tags) != fmt.Sprint(wantTags) {
		t.Errorf("tag counts = %v, want %v", tags, wantTags)
	}

	if got := byFeed["https://xkcd.com/atom.xml"].Title; got != "xkcd (the comic)" {
		t.Errorf("xkcd title = %q", got)
	}
	if e := byFeed["https://www.geektime.co.il/feed"]; e.Text != "גיקטיים" || e.Tags != nil || e.Title != "" {
		t.Errorf("geektime entry = %+v, want Text גיקטיים, no title and no tags", e)
	}
	luke := byFeed["https://jvns.ca/atom.xml"]
	if want := time.Date(2026, 6, 24, 8, 47, 26, 0, edt); !luke.CreatedAt.Equal(want) {
		t.Errorf("Mark Rober CreatedAt = %v, want %v", luke.CreatedAt, want)
	}
	if luke.Text != "Julia Evans's blog" || luke.SiteURL != "https://jvns.ca/" {
		t.Errorf("Mark Rober entry = %+v", luke)
	}
}

const inoreaderExport = `<?xml version="1.0" encoding="UTF-8"?>
<opml version="1.0">
  <head><title>Subscriptions in Inoreader</title></head>
  <body>
    <outline text="Tech" title="Tech">
      <outline type="rss" text="The Go Blog" title="The Go Blog" xmlUrl="https://go.dev/blog/feed.atom" htmlUrl="https://go.dev/blog"/>
      <outline text="Deep  Dives" title="Deep Dives">
        <outline type="rss" text="LWN" xmlUrl="https://lwn.net/headlines/rss"/>
      </outline>
    </outline>
    <outline text="News">
      <outline type="rss" text="Go again" xmlUrl="https://go.dev/blog/feed.atom"/>
      <outline type="rss" text="BBC" xmlUrl="https://feeds.bbci.co.uk/news/rss.xml" htmlUrl="https://www.bbc.co.uk/news"/>
    </outline>
    <outline type="rss" text="Top level" xmlUrl="https://top.example/rss"/>
  </body>
</opml>`

func TestParse(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []Entry
	}{
		{
			name: "nested folders become tags",
			in:   inoreaderExport,
			want: []Entry{
				{Title: "The Go Blog", Text: "The Go Blog", FeedURL: "https://go.dev/blog/feed.atom", SiteURL: "https://go.dev/blog",
					Importance: DefaultImportance, Tags: []string{"Tech"}},
				{Text: "LWN", FeedURL: "https://lwn.net/headlines/rss", Importance: DefaultImportance, Tags: []string{"Deep Dives", "Tech"}},
				{Text: "BBC", FeedURL: "https://feeds.bbci.co.uk/news/rss.xml", SiteURL: "https://www.bbc.co.uk/news",
					Importance: DefaultImportance, Tags: []string{"News"}},
				{Text: "Top level", FeedURL: "https://top.example/rss", Importance: DefaultImportance},
			},
		},
		{
			name: "importance and category tags are inherited by descendants only",
			in: `<opml><body>
				<outline text="Work" category="importance/30,job">
					<outline text="A" xmlUrl="https://a.example/feed"/>
					<outline text="B" xmlUrl="https://b.example/feed" category="importance/0"/>
					<outline text="Inner" category="importance/7">
						<outline text="C" xmlUrl="https://c.example/feed"/>
					</outline>
				</outline>
				<outline text="D" xmlUrl="https://d.example/feed"/>
			</body></opml>`,
			want: []Entry{
				{Text: "A", FeedURL: "https://a.example/feed", Importance: model.Sometime, Tags: []string{"Work", "job"}},
				{Text: "B", FeedURL: "https://b.example/feed", Importance: model.Realtime, Tags: []string{"Work", "job"}},
				{Text: "C", FeedURL: "https://c.example/feed", Importance: model.Occasional, Tags: []string{"Inner", "Work", "job"}},
				{Text: "D", FeedURL: "https://d.example/feed", Importance: DefaultImportance},
			},
		},
		{
			name: "a feed outline passes its category but not its text to children",
			in: `<opml><body>
				<outline text="Parent" xmlUrl="https://p.example/feed" category="importance/7,x">
					<outline text="Child" xmlUrl="https://c.example/feed"/>
				</outline>
			</body></opml>`,
			want: []Entry{
				{Text: "Parent", FeedURL: "https://p.example/feed", Importance: model.Occasional, Tags: []string{"x"}},
				{Text: "Child", FeedURL: "https://c.example/feed", Importance: model.Occasional, Tags: []string{"x"}},
			},
		},
		{
			name: "category tags are trimmed, deduplicated and sorted",
			in: `<opml><body><outline text="T" xmlUrl="https://t.example/feed"
				category=" /tag1 , ,tag2,tag1, ` + devTag + ` ,🏠,importance/365"/></body></opml>`,
			want: []Entry{
				{Text: "T", FeedURL: "https://t.example/feed", Importance: model.Rarely, Tags: []string{"tag1", "tag2", "🏠", devTag}},
			},
		},
		{
			name: "attribute and element names are case-insensitive and head is optional",
			in: `<OPML version="2.0"><BODY><Outline TEXT="X" Title="Mine" XMLURL="https://x.example/feed" HtmlUrl="https://x.example/"
				CATEGORY="importance/7,t" Created="Wed Jun 24 2026 08:47:26 GMT-0400 (Eastern Daylight Time)"/></BODY></OPML>`,
			want: []Entry{
				{Title: "Mine", Text: "X", FeedURL: "https://x.example/feed", SiteURL: "https://x.example/", Importance: model.Occasional,
					Tags: []string{"t"}, CreatedAt: time.Date(2026, 6, 24, 8, 47, 26, 0, edt)},
			},
		},
		{
			name: "htmlUrl is the feed URL when xmlUrl is missing",
			in:   `<opml version="2.0"><body><outline text="Site" htmlUrl="https://site.example/" xmlUrl=" "/></body></opml>`,
			want: []Entry{
				{Text: "Site", FeedURL: "https://site.example/", SiteURL: "https://site.example/", Importance: DefaultImportance},
			},
		},
		{
			name: "outlines without a URL are skipped",
			in: `<opml version="2.0"><head><title>t</title></head><body>
				<outline text="Empty folder"/>
				<outline title="Only a title" category="importance/7"></outline>
			</body></opml>`,
		},
		{
			name: "duplicate feed URLs keep the first",
			in: `<opml><body>
				<outline text="First" xmlUrl=" https://dup.example/feed " category="importance/7,a"/>
				<outline text="Second" xmlUrl="https://dup.example/feed" category="importance/30,b"/>
			</body></opml>`,
			want: []Entry{
				{Text: "First", FeedURL: "https://dup.example/feed", Importance: model.Occasional, Tags: []string{"a"}},
			},
		},
		{
			name: "HTML entities and bare ampersands are tolerated",
			in:   `<opml><body><outline text="Tom &amp; Jerry&nbsp;&eacute; &apos;s" xmlUrl="https://e.example/feed?a=1&b=2"/></body></opml>`,
			want: []Entry{
				{Text: "Tom & Jerry é 's", FeedURL: "https://e.example/feed?a=1&b=2", Importance: DefaultImportance},
			},
		},
		{
			name: "declared ISO-8859-1 charset is decoded",
			in:   "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><opml><body><outline text=\"caf\xe9\" xmlUrl=\"https://cafe.example/feed\"/></body></opml>",
			want: []Entry{{Text: "café", FeedURL: "https://cafe.example/feed", Importance: DefaultImportance}},
		},
		{
			name: "declared windows-1252 charset is decoded",
			in:   "<?xml version=\"1.0\" encoding=\"windows-1252\"?><opml><body><outline text=\"\x93q\x94\" xmlUrl=\"https://q.example/feed\"/></body></opml>",
			want: []Entry{{Text: "“q”", FeedURL: "https://q.example/feed", Importance: DefaultImportance}},
		},
		{
			name: "UTF-8 byte order mark",
			in:   "\xef\xbb\xbf<?xml version=\"1.0\" encoding=\"UTF-8\"?><opml><body><outline text=\"b\" xmlUrl=\"https://b.example/feed\"/></body></opml>",
			want: []Entry{{Text: "b", FeedURL: "https://b.example/feed", Importance: DefaultImportance}},
		},
		{
			name: "content after the root element is ignored",
			in:   `<opml><body><outline xmlUrl="https://a.example/feed"/></body></opml><outline xmlUrl="https://b.example/feed"/><junk`,
			want: []Entry{{FeedURL: "https://a.example/feed", Importance: DefaultImportance}},
		},
		{
			name: "empty body",
			in:   `<?xml version="1.0"?><opml version="2.0"><head/><body/></opml>`,
		},
		{
			name: "maximum nesting depth",
			in:   "<opml><body>" + strings.Repeat(`<outline text="f">`, maxDepth-1) + `<outline xmlUrl="https://deep.example/feed"/>` + "</body></opml>",
			want: []Entry{{FeedURL: "https://deep.example/feed", Importance: DefaultImportance, Tags: []string{"f"}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseString(t, tt.in)
			if !slices.EqualFunc(got, tt.want, entryEqual) {
				t.Errorf("got:%s\nwant:%s", formatEntries(got), formatEntries(tt.want))
			}
		})
	}
}

func TestParseImportance(t *testing.T) {
	tests := []struct {
		category string
		want     model.Importance
		wantTags []string
	}{
		{"", DefaultImportance, nil},
		{"news", DefaultImportance, []string{"news"}},
		{"importance/0", model.Realtime, nil},
		{"importance/1", model.Frequent, nil},
		{"importance/3", model.Frequent, nil},
		{"importance/7", model.Occasional, nil},
		{"importance/29", model.Occasional, nil},
		{"importance/30", model.Sometime, nil},
		{"importance/100", model.Sometime, nil},
		{"importance/365", model.Rarely, nil},
		{"importance/1000", model.Rarely, nil},
		{"importance/99999999999999999999999", model.Rarely, nil},
		{"Importance/7", model.Occasional, nil},
		{"/importance/30", model.Sometime, nil},
		{"importance/7,importance/30", model.Sometime, nil},
		{"importance/-1", DefaultImportance, []string{"importance/-1"}},
		{"importance/x", DefaultImportance, []string{"importance/x"}},
	}
	for _, tt := range tests {
		t.Run(tt.category, func(t *testing.T) {
			in := fmt.Sprintf(`<opml><body><outline xmlUrl="https://i.example/feed" category="%s"/></body></opml>`, tt.category)
			got := parseString(t, in)
			if len(got) != 1 {
				t.Fatalf("got %d entries, want 1", len(got))
			}
			if got[0].Importance != tt.want || !slices.Equal(got[0].Tags, tt.wantTags) {
				t.Errorf("got importance %d tags %q, want %d %q", got[0].Importance, got[0].Tags, tt.want, tt.wantTags)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr string
	}{
		{"empty", "", "no root element"},
		{"whitespace", " \n\t", "no root element"},
		{"plain text", "just some text", "no root element"},
		{"RSS feed", `<?xml version="1.0"?><rss version="2.0"><channel><title>x</title></channel></rss>`, "root element is <rss>"},
		{"HTML page", `<html><body><outline xmlUrl="https://a.example/feed"/></body></html>`, "root element is <html>"},
		{"truncated", `<opml><body><outline text="a" xmlUrl="https://a.example/feed">`, "unexpected EOF"},
		{"unsupported charset", `<?xml version="1.0" encoding="x-bogus"?><opml/>`, "x-bogus"},
		{
			"nested too deep",
			"<opml><body>" + strings.Repeat(`<outline text="f">`, maxDepth) + `<outline xmlUrl="https://deep.example/feed"/>` + "</body></opml>",
			"nested deeper than",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries, err := Parse(strings.NewReader(tt.in))
			if err == nil {
				t.Fatalf("Parse succeeded with %d entries, want error containing %q", len(entries), tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) || !strings.HasPrefix(err.Error(), "opml: ") {
				t.Errorf("error %q, want an opml error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseBoundsTags(t *testing.T) {
	var cat strings.Builder
	cat.WriteString("importance/7")
	for i := range 20000 {
		fmt.Fprintf(&cat, ",t%05d", i)
	}
	long := strings.Repeat("x", maxTagRunes+1)
	var doc strings.Builder
	fmt.Fprintf(&doc, `<opml><body><outline text="folder" category="%s,%s">`, long, cat.String())
	for i := range 300 {
		fmt.Fprintf(&doc, `<outline text="f%d" xmlUrl="https://f%d.example/feed" category="own"/>`, i, i)
	}
	doc.WriteString(`</outline></body></opml>`)

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	entries, err := Parse(strings.NewReader(doc.String()))
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 300 {
		t.Fatalf("got %d entries, want 300", len(entries))
	}
	for _, e := range entries {
		if len(e.Tags) != maxTags || e.Importance != model.Occasional {
			t.Fatalf("entry %s has %d tags, importance %d; want %d, 7", e.FeedURL, len(e.Tags), e.Importance, maxTags)
		}
	}
	// the folder name and the first category tags fill the limit; the long tag is ignored
	if want := []string{"folder", "t00000", "t00001"}; !slices.Equal(entries[0].Tags[:3], want) {
		t.Errorf("tags start %q, want %q", entries[0].Tags[:3], want)
	}
	// unbounded, every child copied 20000 tags: about 190 MB for this file
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 50<<20 {
		t.Errorf("Parse allocated %d MB", alloc>>20)
	}
}

func TestParseTagLimits(t *testing.T) {
	long := strings.Repeat("é", maxTagRunes)
	doc := fmt.Sprintf(`<opml><body><outline xmlUrl="https://a.example/feed" category="a,  a ,%s,%sx,b"/></body></opml>`, long, long)
	entries, err := Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "b", long}; len(entries) != 1 || !slices.Equal(entries[0].Tags, want) {
		t.Errorf("entries = %+v, want tags %q", entries, want)
	}
}

func TestParseTooManyEntries(t *testing.T) {
	var doc strings.Builder
	doc.WriteString(`<opml><body>`)
	for i := range maxEntries + 1 {
		fmt.Fprintf(&doc, `<outline xmlUrl="https://f%d.example/"/>`, i)
	}
	doc.WriteString(`</body></opml>`)
	if _, err := Parse(strings.NewReader(doc.String())); err == nil || !strings.Contains(err.Error(), "more than 10000 feeds") {
		t.Errorf("Parse = %v, want too many feeds", err)
	}
	// a repeated feed does not count twice
	var dup strings.Builder
	dup.WriteString(`<opml><body>`)
	for i := range maxEntries + 1 {
		fmt.Fprintf(&dup, `<outline xmlUrl="https://f%d.example/"/>`, i%maxEntries)
	}
	dup.WriteString(`</body></opml>`)
	if entries, err := Parse(strings.NewReader(dup.String())); err != nil || len(entries) != maxEntries {
		t.Errorf("Parse = %d entries, %v; want %d", len(entries), err, maxEntries)
	}
}

func TestParseCreated(t *testing.T) {
	utc := time.Date(2026, 6, 24, 12, 47, 26, 0, time.UTC)
	tests := []struct {
		in   string
		want time.Time
	}{
		{"Wed Jun 24 2026 08:47:26 GMT-0400 (Eastern Daylight Time)", utc},
		{"Wed Jun 24 2026 18:17:26 GMT+0530 (India Standard Time)", utc},
		{"Wed Jun 24 2026 12:47:26 GMT+0000 (Coordinated Universal Time)", utc},
		{"Wed Jun 24 2026 08:47:26 GMT-0400", utc},
		{"Wed, 24 Jun 2026 12:47:26 GMT", utc},
		{"Wed, 24 Jun 2026 12:47:26 UTC", utc},
		{"Wed, 24 Jun 2026 08:47:26 EDT", utc},
		{"Wed, 24 Jun 2026 05:47:26 PDT", utc},
		{"Wed, 24 Jun 2026 08:47:26 -0400", utc},
		{"Wed, 24 Jun 2026 12:47:26 +0000", utc},
		{"Sat, 4 Jul 2026 12:00:00 +0000", time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)},
		{"24 Jun 26 08:47 EDT", utc.Add(-26 * time.Second)},
		{"24 Jun 26 08:47 -0400", utc.Add(-26 * time.Second)},
		{"2026-06-24T08:47:26-04:00", utc},
		{"2026-06-24T12:47:26Z", utc},
		{"2026-06-24T12:47:26.000Z", utc},
		{"2026-06-24T12:47:26.123Z", utc.Add(123 * time.Millisecond)},
		{"1782305246000", utc},
		{"  Wed, 24 Jun 2026 12:47:26 GMT  ", utc},
		{"", time.Time{}},
		{"   ", time.Time{}},
		{"(Eastern Daylight Time)", time.Time{}},
		{"yesterday", time.Time{}},
		{"Invalid Date", time.Time{}},
		{"99999999999999999999999", time.Time{}},
		{"2026-06-24", time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := parseCreated(tt.in)
			if !got.Equal(tt.want) {
				t.Errorf("parseCreated(%q) = %v, want %v", tt.in, got, tt.want)
			}
			if !got.IsZero() && got.Location() != time.UTC {
				t.Errorf("parseCreated(%q) location = %v, want UTC", tt.in, got.Location())
			}
		})
	}
}

func TestToFollow(t *testing.T) {
	now := time.Date(2026, 9, 24, 20, 0, 0, 0, time.UTC)
	created := time.Date(2026, 6, 24, 12, 47, 26, 0, time.UTC)
	tests := []struct {
		name  string
		entry Entry
		want  model.Follow
	}{
		{
			name: "full entry",
			entry: Entry{Title: "Mine", Text: "Feed", FeedURL: "https://f.example/feed", SiteURL: "https://f.example/",
				Importance: model.Occasional, Tags: []string{"a", "b"}, CreatedAt: created},
			want: model.Follow{URL: "https://f.example/", FeedURL: "https://f.example/feed", Title: "Mine", FeedTitle: "Feed",
				Importance: model.Occasional, Tags: []string{"a", "b"}, CreatedAt: created, EditedAt: created, NextFetchAt: now},
		},
		{
			name:  "no site URL and no created time",
			entry: Entry{Text: "Feed", FeedURL: "https://f.example/feed", Importance: model.Rarely},
			want: model.Follow{URL: "https://f.example/feed", FeedURL: "https://f.example/feed", FeedTitle: "Feed",
				Importance: model.Rarely, CreatedAt: now, EditedAt: now, NextFetchAt: now},
		},
		{
			name:  "importance is normalized",
			entry: Entry{FeedURL: "https://f.example/feed", Importance: 100},
			want: model.Follow{URL: "https://f.example/feed", FeedURL: "https://f.example/feed",
				Importance: model.Sometime, CreatedAt: now, EditedAt: now, NextFetchAt: now},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.entry.ToFollow(now)
			if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", tt.want) {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}

	t.Run("tags are copied", func(t *testing.T) {
		e := Entry{FeedURL: "https://f.example/feed", Tags: []string{"a"}}
		f := e.ToFollow(now)
		e.Tags[0] = "changed"
		if f.Tags[0] != "a" {
			t.Errorf("follow tags alias the entry tags: %q", f.Tags)
		}
	})
}

func TestWrite(t *testing.T) {
	now := time.Date(2026, 9, 24, 20, 6, 48, 0, time.FixedZone("EDT", -4*3600))
	follows := []model.Follow{
		{
			FeedURL: "https://b.example/feed", URL: "https://b.example/", FeedTitle: "beta & co", Importance: model.Occasional,
			Tags: []string{"dev", devTag}, CreatedAt: time.Date(2026, 6, 24, 8, 47, 26, 0, edt),
		},
		{
			FeedURL: "https://a.example/feed?x=1&y=2", URL: "https://a.example/", Title: ` Alpha <"mine"> `, FeedTitle: "Alpha feed",
			Importance: model.Frequent, EditedAt: time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC),
		},
		{FeedURL: "https://c.example/feed", Importance: model.Realtime},
	}
	want := `<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <head>
    <title>stalker &amp; follows</title>
    <dateCreated>Fri, 25 Sep 2026 00:06:48 GMT</dateCreated>
  </head>
  <body>
    <outline category="importance/1" created="Thu, 25 Jun 2026 00:00:00 +0000" text="Alpha &lt;&#34;mine&#34;&gt;" title="Alpha &lt;&#34;mine&#34;&gt;" type="rss" xmlUrl="https://a.example/feed?x=1&amp;y=2" htmlUrl="https://a.example/"></outline>
    <outline category="importance/7,dev,` + devTag + `" created="Wed, 24 Jun 2026 12:47:26 +0000" text="beta &amp; co" type="rss" xmlUrl="https://b.example/feed" htmlUrl="https://b.example/"></outline>
    <outline category="importance/0" text="c.example" type="rss" xmlUrl="https://c.example/feed"></outline>
  </body>
</opml>
`
	var buf bytes.Buffer
	if err := write(&buf, "stalker & follows", follows, now); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestWriteOrder(t *testing.T) {
	follows := []model.Follow{
		{Title: "b", FeedURL: "https://1.example/feed"},
		{Title: "A", FeedURL: "https://3.example/feed"},
		{Title: "a", FeedURL: "https://2.example/feed"},
		{FeedTitle: "Zed", FeedURL: "https://0.example/feed"},
	}
	var buf bytes.Buffer
	if err := Write(&buf, "t", follows); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range parseString(t, buf.String()) {
		got = append(got, e.FeedURL)
	}
	want := []string{"https://2.example/feed", "https://3.example/feed", "https://1.example/feed", "https://0.example/feed"}
	if !slices.Equal(got, want) {
		t.Errorf("order = %q, want %q", got, want)
	}
	if follows[0].Title != "b" {
		t.Error("Write reordered the caller's slice")
	}
}

func TestWriteEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, "empty", nil); err != nil {
		t.Fatal(err)
	}
	if got := parseString(t, buf.String()); len(got) != 0 {
		t.Errorf("got %d entries from an empty export", len(got))
	}
}

type failingWriter struct{ n int }

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.n <= 0 {
		return 0, errors.New("disk full")
	}
	w.n--
	return len(p), nil
}

func TestWriteError(t *testing.T) {
	follows := []model.Follow{{FeedURL: "https://a.example/feed"}}
	for n := range 3 {
		if err := Write(&failingWriter{n: n}, "t", follows); err == nil {
			t.Errorf("writer failing after %d writes: got nil error", n)
		}
	}
}

func TestWriteParseRoundTrip(t *testing.T) {
	created := time.Date(2026, 6, 24, 12, 47, 26, 999_000_000, time.UTC)
	follows := []model.Follow{
		{FeedURL: "https://rt.example/realtime", URL: "https://rt.example/", FeedTitle: "Realtime", Importance: model.Realtime,
			Tags: []string{"C++ & Rust", model.HomeTag, devTag}, CreatedAt: created},
		{FeedURL: "https://rt.example/frequent?a=1&b=<2>", URL: "https://rt.example/f", Title: "Tab\tand \"quotes\" 'n' <b>&amp;</b>",
			FeedTitle: "ignored", Importance: model.Frequent, CreatedAt: created},
		{FeedURL: "https://rt.example/occasional", FeedTitle: "Occasional", Importance: model.Occasional, Tags: []string{"📹"}},
		{FeedURL: "https://rt.example/sometime", URL: "https://sometime.example/", Importance: model.Sometime, CreatedAt: created},
		{FeedURL: "https://rt.example/rarely", Title: "גיקטיים", Importance: model.Rarely, Tags: []string{"a", "b"}, CreatedAt: created},
	}
	var buf bytes.Buffer
	if err := Write(&buf, "round trip", follows); err != nil {
		t.Fatal(err)
	}
	entries := parseString(t, buf.String())
	if len(entries) != len(follows) {
		t.Fatalf("got %d entries, want %d", len(entries), len(follows))
	}
	byFeed := map[string]Entry{}
	for _, e := range entries {
		byFeed[e.FeedURL] = e
	}
	for _, f := range follows {
		e, ok := byFeed[f.FeedURL]
		if !ok {
			t.Errorf("feed %q lost", f.FeedURL)
			continue
		}
		want := Entry{
			Title:      f.Title,
			Text:       f.DisplayTitle(),
			FeedURL:    f.FeedURL,
			SiteURL:    f.URL,
			Importance: f.Importance,
			Tags:       f.Tags,
			CreatedAt:  f.CreatedAt.Truncate(time.Second),
		}
		if !entryEqual(e, want) {
			t.Errorf("got  %+v\nwant %+v", e, want)
		}
		back := e.ToFollow(time.Now())
		if back.Title != f.Title || back.FeedURL != f.FeedURL || back.Importance != f.Importance || !slices.Equal(back.Tags, f.Tags) {
			t.Errorf("ToFollow after round trip = %+v, want settings of %+v", back, f)
		}
	}
}

func TestFraidycatExportRoundTrip(t *testing.T) {
	entries := parseFraidycatExport(t)
	now := time.Now()
	follows := make([]model.Follow, len(entries))
	for i, e := range entries {
		follows[i] = e.ToFollow(now)
	}
	var buf bytes.Buffer
	if err := Write(&buf, "Fraidycat Follows", follows); err != nil {
		t.Fatal(err)
	}
	again := parseString(t, buf.String())
	if len(again) != len(entries) {
		t.Fatalf("got %d entries after round trip, want %d", len(again), len(entries))
	}
	byFeed := map[string]Entry{}
	for _, e := range again {
		byFeed[e.FeedURL] = e
	}
	for _, want := range entries {
		if got := byFeed[want.FeedURL]; !entryEqual(got, want) {
			t.Errorf("got  %+v\nwant %+v", got, want)
		}
	}
}
