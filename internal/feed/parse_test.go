package feed

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oryanm/stalker/internal/model"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func date(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func TestParse(t *testing.T) {
	const base = "https://base.example.net/dir/feed.xml"
	tests := []struct {
		fixture   string
		want      Result
		wantPosts []model.Post
	}{
		{
			fixture: "rss2.xml",
			want: Result{
				Title:       "xkcd",
				Description: "Painless software management",
				SiteURL:     "https://xkcd.com",
				ImageURL:    "https://xkcd.com/logo.png",
			},
			wantPosts: []model.Post{
				{
					GUID: "https://xkcd.com/?p=300", URL: "https://xkcd.com/2023/05/02/newest/",
					Title: "Newest post", Author: "Joel Spolsky", PublishedAt: date("2023-05-02T13:30:00Z"),
				},
				{
					GUID: "https://xkcd.com/?p=200", URL: "https://xkcd.com/2022/03/03/middle/",
					Title: "Middle post", PublishedAt: date("2022-03-03T08:00:00Z"),
				},
				{
					GUID: "https://xkcd.com/?p=100", URL: "https://xkcd.com/2021/01/01/older/",
					Title: "Older post", Author: "Joel Spolsky", PublishedAt: date("2021-01-01T12:00:00Z"),
				},
			},
		},
		{
			fixture: "rdf.xml",
			want: Result{
				Title:       "Pinboard (someone)",
				Description: "recent bookmarks from someone",
				SiteURL:     "https://pinboard.in/u:someone/",
			},
			wantPosts: []model.Post{
				{
					GUID: "https://example.com/b", URL: "https://example.com/b", Title: "Bookmark B",
					PublishedAt: date("2024-02-03T10:00:00Z"), UpdatedAt: date("2024-02-03T10:00:00Z"),
				},
				{
					GUID: "https://example.com/a", URL: "https://example.com/a", Title: "Bookmark A", Author: "someone",
					PublishedAt: date("2024-02-01T10:00:00Z"), UpdatedAt: date("2024-02-01T10:00:00Z"),
				},
			},
		},
		{
			fixture: "atom.xml",
			want: Result{
				Title:       "Clean Coder Blog",
				Description: "Russ Cox's blog",
				SiteURL:     "http://research.swtch.com/",
				ImageURL:    "https://base.example.net/logo.png",
			},
			wantPosts: []model.Post{
				{
					GUID:  "http://research.swtch.com/rsc/2024/05/01/both",
					URL:   "http://research.swtch.com/rsc/2024/05/01/both.html",
					Title: "Published and updated", Author: "rsc",
					PublishedAt: date("2024-05-01T00:00:00Z"), UpdatedAt: date("2024-05-02T00:00:00Z"),
				},
				{
					GUID:  "http://research.swtch.com/rsc/2024/04/01/updated",
					URL:   "http://research.swtch.com/rsc/2024/04/01/updated.html",
					Title: "Updated only", Author: "Russ Cox",
					PublishedAt: date("2024-04-01T00:00:00Z"), UpdatedAt: date("2024-04-01T00:00:00Z"),
				},
			},
		},
		{
			fixture: "youtube.xml",
			want: Result{
				Title:   "Some Channel",
				SiteURL: "https://www.youtube.com/channel/UCsXVk37bltHxD1rDPwtNM8Q",
			},
			wantPosts: []model.Post{
				{
					GUID: "yt:video:dQw4w9WgXcQ", URL: "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
					Title: "Building a thing & breaking it", Author: "Some Channel",
					PublishedAt: date("2026-09-20T15:00:06Z"), UpdatedAt: date("2026-09-23T02:11:45Z"),
				},
				{
					GUID: "yt:video:abcDEF12345", URL: "https://www.youtube.com/watch?v=abcDEF12345",
					Title: "Only in media title", Author: "Some Channel",
					PublishedAt: date("2026-09-10T15:00:00Z"), UpdatedAt: date("2026-09-11T00:00:00Z"),
				},
			},
		},
		{
			fixture: "jsonfeed.json",
			want: Result{
				Title:       "Micro & Blog",
				Description: "A tiny blog",
				SiteURL:     "https://micro.example.org/",
				ImageURL:    "https://micro.example.org/icon.png",
			},
			wantPosts: []model.Post{
				{
					GUID: "2", URL: "https://micro.example.org/2", Title: "A note without a title that is short.",
					Author: "Item Author", PublishedAt: date("2025-01-02T10:00:00Z"),
				},
				{
					GUID: "1", URL: "https://micro.example.org/1", Title: "First post", Author: "Feed Author",
					PublishedAt: date("2025-01-01T09:00:00Z"), UpdatedAt: date("2025-01-05T10:00:00Z"),
				},
			},
		},
		{
			fixture: "relative.xml",
			want: Result{
				Title:    "Relative Links",
				SiteURL:  "https://base.example.net/blog/",
				ImageURL: "https://base.example.net/dir/images/logo.png",
			},
			wantPosts: []model.Post{
				{
					GUID: "https://base.example.net/blog/posts/one.html", URL: "https://base.example.net/blog/posts/one.html",
					Title: "Relative to site", PublishedAt: date("2025-01-06T10:00:00Z"),
				},
				{
					GUID: "https://base.example.net/about", URL: "https://base.example.net/about",
					Title: "Root relative", PublishedAt: date("2025-01-05T10:00:00Z"),
				},
				{
					GUID: "https://cdn.example.org/video", URL: "https://cdn.example.org/video",
					Title: "Protocol relative", PublishedAt: date("2025-01-04T10:00:00Z"),
				},
				{
					GUID:  hashGUID("Script link", "2025-01-03T10:00:00Z"),
					Title: "Script link", PublishedAt: date("2025-01-03T10:00:00Z"),
				},
			},
		},
		{
			fixture: "dateless.xml",
			want:    Result{Title: "Dateless", SiteURL: "https://dateless.example.com/"},
			wantPosts: []model.Post{
				{GUID: "https://dateless.example.com/4", URL: "https://dateless.example.com/4", Title: "Dated new", PublishedAt: date("2023-01-01T00:00:00Z")},
				{GUID: "https://dateless.example.com/2", URL: "https://dateless.example.com/2", Title: "Dated old", PublishedAt: date("2020-01-01T00:00:00Z")},
				{GUID: "https://dateless.example.com/1", URL: "https://dateless.example.com/1", Title: "No date one"},
				{GUID: "https://dateless.example.com/3", URL: "https://dateless.example.com/3", Title: "No date two"},
				{GUID: "https://dateless.example.com/5", URL: "https://dateless.example.com/5", Title: "Garbage date"},
			},
		},
		{
			fixture: "htmltitles.xml",
			want:    Result{Title: "Markup in titles", SiteURL: "https://html.example.com/"},
			wantPosts: []model.Post{
				{GUID: "1", Title: "Linked title"},
				{GUID: "2", Title: `AT&T "quoted"`},
				{GUID: "3", Title: "Fish & Chips < 5"},
				{GUID: "4", Title: "Line break para"},
				{GUID: "5", Title: "Café \u2014 nbsp here"},
				{GUID: "6", Title: "Why Optional<T> is not Serializable"},
				{GUID: "7", Title: "Map<String, Object> vs <B>"},
			},
		},
		{
			fixture: "atomtext.xml",
			want:    Result{Title: "User Jon Skeet - Stack Overflow", SiteURL: "https://stackoverflow.com/users/22656"},
			wantPosts: []model.Post{
				{
					GUID: "https://stackoverflow.com/questions/1#1", URL: "https://stackoverflow.com/questions/1#1",
					Title: "Comment by Jon Skeet on Cast List<List<String>> to List<String>", PublishedAt: date("2026-09-20T12:00:00Z"),
				},
				{
					GUID: "https://stackoverflow.com/questions/2#2", URL: "https://stackoverflow.com/questions/2#2",
					Title: "Answer by Jon Skeet for Sort a Map<Key, Value> by values", PublishedAt: date("2026-09-19T12:00:00Z"),
				},
				{
					GUID: "https://stackoverflow.com/questions/3#3", URL: "https://stackoverflow.com/questions/3#3",
					Title: "List<int[]> is a correct statement, List<Object> too", PublishedAt: date("2026-09-18T12:00:00Z"),
				},
				{
					GUID: "https://stackoverflow.com/questions/4#4", URL: "https://stackoverflow.com/questions/4#4",
					Title: "Why <script> tags block rendering", PublishedAt: date("2026-09-17T12:00:00Z"),
				},
				{
					GUID: "https://stackoverflow.com/questions/5#5", URL: "https://stackoverflow.com/questions/5#5",
					Title: "Emphasis in HTML & Optional<T>", PublishedAt: date("2026-09-16T12:00:00Z"),
				},
			},
		},
		{
			fixture: "notitles.xml",
			want:    Result{Title: "No Titles", SiteURL: "https://notitles.example.com/"},
			wantPosts: []model.Post{
				{
					GUID: "https://notitles.example.com/long", URL: "https://notitles.example.com/long",
					Title: "This is a rather long description that goes on and on, well past the one hundred and forty " +
						"character limit that we use for excerpts in…",
					PublishedAt: date("2025-01-06T10:00:00Z"),
				},
				{
					GUID: "https://notitles.example.com/content", URL: "https://notitles.example.com/content",
					Title: "Only content here", PublishedAt: date("2025-01-05T10:00:00Z"),
				},
				{
					GUID: "https://notitles.example.com/empty", URL: "https://notitles.example.com/empty",
					Title: "Untitled", PublishedAt: date("2025-01-04T10:00:00Z"),
				},
				{
					GUID:  hashGUID("No guid no link", "2025-01-03T10:00:00Z"),
					Title: "No guid no link", PublishedAt: date("2025-01-03T10:00:00Z"),
				},
				{GUID: "dup", Title: "Duplicate", PublishedAt: date("2025-01-02T10:00:00Z")},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			got, err := Parse(readFixture(t, tt.fixture), base)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got.Title != tt.want.Title || got.Description != tt.want.Description ||
				got.SiteURL != tt.want.SiteURL || got.ImageURL != tt.want.ImageURL {
				t.Errorf("feed = {%q %q %q %q}, want {%q %q %q %q}",
					got.Title, got.Description, got.SiteURL, got.ImageURL,
					tt.want.Title, tt.want.Description, tt.want.SiteURL, tt.want.ImageURL)
			}
			if got.NotModified || got.ETag != "" || got.LastModified != "" || got.FinalURL != "" {
				t.Errorf("Parse set HTTP fields: %+v", got)
			}
			assertPosts(t, got.Posts, tt.wantPosts)
		})
	}
}

func assertPosts(t *testing.T, got, want []model.Post) {
	t.Helper()
	if len(got) != len(want) {
		for _, p := range got {
			t.Logf("got %+v", p)
		}
		t.Fatalf("got %d posts, want %d", len(got), len(want))
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.GUID != w.GUID || g.URL != w.URL || g.Title != w.Title || g.Author != w.Author ||
			!g.PublishedAt.Equal(w.PublishedAt) || !g.UpdatedAt.Equal(w.UpdatedAt) {
			t.Errorf("post %d:\n got %+v\nwant %+v", i, g, w)
		}
		if g.ID != 0 || g.FollowID != 0 || !g.FirstSeenAt.IsZero() {
			t.Errorf("post %d has store fields set: %+v", i, g)
		}
		if loc := g.PublishedAt.Location(); !g.PublishedAt.IsZero() && loc != time.UTC {
			t.Errorf("post %d PublishedAt location = %v, want UTC", i, loc)
		}
	}
}

func TestParseNoBaseURL(t *testing.T) {
	got, err := Parse(readFixture(t, "relative.xml"), "")
	if err != nil {
		t.Fatal(err)
	}
	// nothing can be made absolute, and relative URLs must never leak out
	if got.SiteURL != "" || got.ImageURL != "" {
		t.Errorf("SiteURL = %q, ImageURL = %q, want empty", got.SiteURL, got.ImageURL)
	}
	for _, p := range got.Posts {
		if p.URL != "" {
			t.Errorf("post %q URL = %q, want empty", p.Title, p.URL)
		}
	}
}

func TestParseNotAFeed(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{"html page", readFixture(t, "page.html")},
		{"json that is not a json feed", readFixture(t, "notjsonfeed.json")},
		{"empty", nil},
		{"whitespace", []byte("  \n\t ")},
		{"plain text", []byte("hello world")},
		{"broken json", []byte(`{"version": "https://jsonfeed.org/version/1.1", "items": [`)},
		{"svg", []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"></svg>`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := Parse(tt.body, "https://example.com/")
			if !errors.Is(err, ErrNotAFeed) {
				t.Fatalf("err = %v, want ErrNotAFeed", err)
			}
			if res != nil {
				t.Errorf("result = %+v, want nil", res)
			}
		})
	}
}

func TestParseBOM(t *testing.T) {
	body := append([]byte{0xEF, 0xBB, 0xBF}, readFixture(t, "jsonfeed.json")...)
	got, err := Parse(body, "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Title != "Micro & Blog" || len(got.Posts) != 2 {
		t.Errorf("got title %q with %d posts", got.Title, len(got.Posts))
	}
}

func TestParseLongTitle(t *testing.T) {
	long := strings.Repeat("word ", 100)
	body := `<rss version="2.0"><channel><title>t</title><item><title>` + long + `</title><guid>1</guid></item></channel></rss>`
	got, err := Parse([]byte(body), "")
	if err != nil {
		t.Fatal(err)
	}
	title := got.Posts[0].Title
	if n := len([]rune(title)); n > maxTitleRunes+1 || !strings.HasSuffix(title, "…") {
		t.Errorf("title has %d runes: %q", n, title)
	}
}

func TestParseKeepsNewestPosts(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<rss version="2.0"><channel><title>t</title>`)
	start := date("2026-01-01T00:00:00Z")
	// oldest first, with undated items in between, to show the cut follows dates rather than document order
	for i := range maxPosts + 50 {
		fmt.Fprintf(&b, `<item><guid>%d</guid><pubDate>%s</pubDate></item><item><guid>undated-%d</guid></item>`,
			i, start.Add(time.Duration(i)*time.Hour).Format(time.RFC1123Z), i)
	}
	b.WriteString(`</channel></rss>`)
	got, err := Parse([]byte(b.String()), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Posts) != maxPosts {
		t.Fatalf("got %d posts, want %d", len(got.Posts), maxPosts)
	}
	if first, last := got.Posts[0].GUID, got.Posts[maxPosts-1].GUID; first != "249" || last != "50" {
		t.Errorf("kept posts %s to %s, want the newest, 249 to 50", first, last)
	}
}

func TestParseTooManyEntries(t *testing.T) {
	repeat := func(head, entry, tail string, n int) []byte {
		return []byte(head + strings.Repeat(entry, n) + tail)
	}
	const (
		rssHead  = `<rss version="2.0"><channel><title>t</title>`
		rssTail  = `</channel></rss>`
		atomHead = `<feed xmlns="http://www.w3.org/2005/Atom"><title>t</title>`
		jsonHead = `{"version":"https://jsonfeed.org/version/1.1","title":"t","items":[`
	)
	tests := []struct {
		name    string
		body    []byte
		tooMany bool
	}{
		{"rss at the limit", repeat(rssHead, "<item><guid>g</guid></item>", rssTail, maxEntries), false},
		{"rss over the limit", repeat(rssHead, "<item/>", rssTail, maxEntries+1), true},
		{"prefixed and uppercase items count", repeat(rssHead+`<x:ITEM xmlns:x="urn:x"/>`, "<item/>", rssTail, maxEntries), true},
		{"atom over the limit", repeat(atomHead, "<entry><id>i</id></entry>", "</feed>", maxEntries+1), true},
		{"end tags and other elements do not count", repeat(rssHead, "<items></items><entryway/>", rssTail, maxEntries+1), false},
		{"json over the limit", repeat(jsonHead+`{"id":"0"}`, `,{"id":"i"}`, "]}", maxEntries*jsonObjectsPerEntry), true},
		{"braces in json strings do not count", repeat(jsonHead+`{"id":"0","content_text":"`, `{\"}`, `"}]}`, maxEntries*jsonObjectsPerEntry+1), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.body, "")
			if got := errors.Is(err, ErrTooManyEntries); got != tt.tooMany {
				t.Errorf("Parse = %v, want too many entries %v", err, tt.tooMany)
			}
		})
	}
}

func TestPlainText(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", ""},
		{"  plain \n\t text  ", "plain text"},
		{"<b>bold</b> and <i>italic</i>", "bold and italic"},
		{"a<br>b<br/>c", "a b c"},
		{"<p>one</p><p>two</p>", "one two"},
		{"x<script>alert(1)</script>y", "xy"},
		{"<style>p{}</style>styled", "styled"},
		{"Tom &amp; Jerry", "Tom & Jerry"},
		{"AT&T", "AT&T"},
		{"5 < 6 & 7 > 3", "5 < 6 & 7 > 3"},
		{"&lt;not a tag&gt;", "<not a tag>"},
		{"caf&eacute;&nbsp;au&#160;lait", "café au lait"},
		{"<!-- hidden -->shown", "shown"},
		{"un<b>broken</b>word", "unbrokenword"},
	}
	for _, tt := range tests {
		if got := plainText(tt.in); got != tt.want {
			t.Errorf("plainText(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestTitleText(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", ""},
		{"  plain \n\t text  ", "plain text"},
		{"<b>bold</b> and <i>italic</i>", "bold and italic"},
		{`<a href="https://x">link</a>`, "link"},
		{"<B>legacy</B> <STRONG>shout</STRONG>", "<B>legacy</B> shout"},
		{"a<br>b<br/>c<BR>d", "a b c d"},
		{"<p>one</p><div>two</div>", "one two"},
		{"x<script>alert(1)</script>y", "xy"},
		{"<style>p{}</style>styled", "styled"},
		{"Why <script> tags block rendering", "Why <script> tags block rendering"},
		{"List<String>", "List<String>"},
		{"Cast List<List<String>> to List<String>", "Cast List<List<String>> to List<String>"},
		{"Map<K, V> and Map<String, Object>", "Map<K, V> and Map<String, Object>"},
		{"Optional<T>, Box<dyn Error>, Vec<u8>", "Optional<T>, Box<dyn Error>, Vec<u8>"},
		{"List<Object> and Optional<Map>", "List<Object> and Optional<Map>"},
		{"<code>List</code><Object>", "List<Object>"},
		{"Tom &amp; Jerry", "Tom & Jerry"},
		{"5 < 6 & 7 > 3", "5 < 6 & 7 > 3"},
		{"&lt;b&gt;escaped&lt;/b&gt;", "<b>escaped</b>"},
		{"caf&eacute;&nbsp;au&#160;lait", "café au lait"},
		{"<!-- hidden -->shown", "shown"},
		{`<img src="e.png" alt=":)">emoji`, "emoji"},
	}
	for _, tt := range tests {
		if got := titleText(tt.in); got != tt.want {
			t.Errorf("titleText(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"exactly ten", 11, "exactly ten"},
		{"hello brave new world", 15, "hello brave…"},
		{"supercalifragilistic", 5, "super…"},
		{"héllo wörld ünïcode", 13, "héllo wörld…"},
		{"trailing, punctuation here", 11, "trailing…"},
	}
	for _, tt := range tests {
		if got := truncate(tt.in, tt.n); got != tt.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
		}
	}
}

func TestResolve(t *testing.T) {
	base := parseBase("https://example.com/blog/index.html")
	tests := []struct {
		ref, want string
	}{
		{"", ""},
		{"  ", ""},
		{"post.html", "https://example.com/blog/post.html"},
		{"/root", "https://example.com/root"},
		{"//cdn.example.org/a.png", "https://cdn.example.org/a.png"},
		{"http://other.example/x", "http://other.example/x"},
		{"javascript:alert(1)", ""},
		{"data:text/html,hi", ""},
		{"mailto:me@example.com", ""},
		{"https://exa mple.com/", ""},
	}
	for _, tt := range tests {
		if got := resolve(base, tt.ref); got != tt.want {
			t.Errorf("resolve(%q) = %q, want %q", tt.ref, got, tt.want)
		}
	}
}

func TestParseRepairsDates(t *testing.T) {
	tests := []struct {
		name, pubDate string
		want          time.Time
	}{
		{"zone without sign", "Thu, 02 Nov 2017 00:00:00 0100", date("2017-11-01T23:00:00Z")},
		{"zone without sign, no seconds", "Thu, 02 Nov 2017 10:30 0000", date("2017-11-02T10:30:00Z")},
		{"valid date untouched", "Thu, 02 Nov 2017 00:00:00 +0100", date("2017-11-01T23:00:00Z")},
		{"garbage stays undated", "sometime last week", time.Time{}},
		{"bare year stays undated", "2017", time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := `<?xml version="1.0"?><rss version="2.0"><channel><title>T</title>` +
				`<item><title>A</title><link>https://example.com/a</link><pubDate>` + tt.pubDate + `</pubDate></item>` +
				`</channel></rss>`
			res, err := Parse([]byte(body), "https://example.com/feed")
			if err != nil {
				t.Fatal(err)
			}
			if got := res.Posts[0].PublishedAt; !got.Equal(tt.want) {
				t.Errorf("PublishedAt = %v, want %v", got, tt.want)
			}
		})
	}
}
