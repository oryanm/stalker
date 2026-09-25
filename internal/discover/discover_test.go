package discover

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/oryanm/stalker/internal/feed"
)

// page is a canned response keyed by "host/path?query".
type page struct {
	status   int
	ctype    string
	body     string
	location string
}

// fakeWeb routes every host to one httptest server through the client's
// transport, so site rules for youtube.com, reddit.com and friends are
// testable without network access.
type fakeWeb struct {
	srv *httptest.Server

	mu       sync.Mutex
	pages    map[string]page
	requests []string
	fail     func(*http.Request) error
}

func newFakeWeb(t *testing.T, pages map[string]page) *fakeWeb {
	t.Helper()
	w := &fakeWeb{pages: pages}
	w.srv = httptest.NewServer(http.HandlerFunc(w.serve))
	t.Cleanup(w.srv.Close)
	return w
}

func (w *fakeWeb) serve(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	p, ok := w.pages[r.Host+r.URL.RequestURI()]
	w.mu.Unlock()
	switch {
	case !ok:
		http.NotFound(rw, r)
	case p.location != "":
		http.Redirect(rw, r, p.location, cmpOr(p.status, http.StatusMovedPermanently))
	default:
		rw.Header().Set("Content-Type", cmpOr(p.ctype, "text/html; charset=utf-8"))
		rw.WriteHeader(cmpOr(p.status, http.StatusOK))
		io.WriteString(rw, p.body)
	}
}

func (w *fakeWeb) RoundTrip(req *http.Request) (*http.Response, error) {
	w.mu.Lock()
	w.requests = append(w.requests, req.URL.String())
	fail := w.fail
	w.mu.Unlock()
	if fail != nil {
		if err := fail(req); err != nil {
			return nil, err
		}
	}
	target, _ := url.Parse(w.srv.URL)
	r := req.Clone(req.Context())
	r.Host = req.URL.Host
	r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
	resp, err := w.srv.Client().Transport.RoundTrip(r)
	if resp != nil {
		resp.Request = req
	}
	return resp, err
}

func (w *fakeWeb) requested() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.requests)
}

func (w *fakeWeb) discoverer() *Discoverer {
	return New(feed.NewClient(feed.Options{Transport: w, HostSpacing: -1}))
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func cmpOr[T comparable](v, fallback T) T {
	var zero T
	if v == zero {
		return fallback
	}
	return v
}

// atomFeed renders a minimal Atom feed; the title makes its posts unique.
func atomFeed(title, site string) page {
	slug := url.PathEscape(strings.ToLower(strings.ReplaceAll(title, " ", "-")))
	link := ""
	if site != "" {
		link = fmt.Sprintf(`<link href="%s"/>`, html.EscapeString(site))
	}
	return page{ctype: "application/atom+xml", body: fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>%s</title>%s
  <id>urn:%s</id>
  <entry>
    <title>First</title>
    <link href="https://posts.example/%s/1"/>
    <id>urn:%s:1</id>
    <updated>2026-01-01T00:00:00Z</updated>
  </entry>
</feed>`, html.EscapeString(title), link, slug, slug, slug)}
}

func rssFeed(title, site string) page {
	slug := url.PathEscape(strings.ToLower(strings.ReplaceAll(title, " ", "-")))
	return page{ctype: "application/rss+xml", body: fmt.Sprintf(`<?xml version="1.0"?>
<rss version="2.0"><channel>
  <title>%s</title><link>%s</link>
  <item><title>First</title><link>https://posts.example/%s/1</link><pubDate>Thu, 01 Jan 2026 00:00:00 GMT</pubDate></item>
</channel></rss>`, html.EscapeString(title), html.EscapeString(site), slug)}
}

func htmlPage(head, body string) page {
	return page{body: "<!DOCTYPE html><html><head><title>A page</title>" + head + "</head><body>" + body + "</body></html>"}
}

func TestDiscover(t *testing.T) {
	const (
		chanA = "UCaaaaaaaaaaaaaaaaaaaaaa"
		chanB = "UCbbbbbbbbbbbbbbbbbbbbbb"
		chanC = "UCcccccccccccccccccccccc"
		chanD = "UCdddddddddddddddddddddd"
		chanE = "UCeeeeeeeeeeeeeeeeeeeeee"
		chanX = "UCsXVk37bltHxD1rDPwtNM8Q"
	)
	ytFeed := func(id string) string { return "www.youtube.com/feeds/videos.xml?channel_id=" + id }
	ytChannel := func(id string) string { return "https://www.youtube.com/channel/" + id }

	tests := []struct {
		name  string
		input string
		pages map[string]page
		want  []Candidate
		// maxRequests bounds the fetches made, proving later stages were skipped
		maxRequests int
	}{
		// YouTube
		{
			name:  "youtube channel",
			input: "https://www.youtube.com/channel/" + chanX + "/videos",
			pages: map[string]page{ytFeed(chanX): atomFeed("Channel X", ytChannel(chanX))},
			want:  []Candidate{{"https://" + ytFeed(chanX), "Channel X", ytChannel(chanX)}},
			// the channel page itself is never fetched
			maxRequests: 1,
		},
		{
			name:  "youtube handle via rss link",
			input: "youtube.com/@someone",
			pages: map[string]page{
				"www.youtube.com/@someone": htmlPage(
					`<link rel="alternate" type="application/rss+xml" title="RSS" href="https://www.youtube.com/feeds/videos.xml?channel_id=`+chanA+`">`, ""),
				ytFeed(chanA): atomFeed("Someone", ytChannel(chanA)),
			},
			want:        []Candidate{{"https://" + ytFeed(chanA), "Someone", ytChannel(chanA)}},
			maxRequests: 2,
		},
		{
			name:  "youtube custom url via canonical on mobile host",
			input: "https://m.youtube.com/c/CustomName",
			pages: map[string]page{
				"www.youtube.com/c/CustomName": htmlPage(`<link rel="canonical" href="https://www.youtube.com/channel/`+chanB+`">`, ""),
				ytFeed(chanB):                  atomFeed("Custom", ytChannel(chanB)),
			},
			want: []Candidate{{"https://" + ytFeed(chanB), "Custom", ytChannel(chanB)}},
		},
		{
			name:  "youtube legacy user via itemprop",
			input: "https://www.youtube.com/user/legacy",
			pages: map[string]page{
				"www.youtube.com/user/legacy": htmlPage(`<meta itemprop="identifier" content="`+chanC+`">`, ""),
				ytFeed(chanC):                 atomFeed("Legacy", ytChannel(chanC)),
			},
			want: []Candidate{{"https://" + ytFeed(chanC), "Legacy", ytChannel(chanC)}},
		},
		{
			name:  "youtube bare name via externalId script",
			input: "https://youtube.com/somename",
			pages: map[string]page{
				"www.youtube.com/somename": htmlPage("",
					`<script>var ytInitialData = {"metadata":{"channelMetadataRenderer":{"title":"x","externalId":"`+chanD+`"}}};</script>`),
				ytFeed(chanD): atomFeed("Bare", ytChannel(chanD)),
			},
			want: []Candidate{{"https://" + ytFeed(chanD), "Bare", ytChannel(chanD)}},
		},
		{
			name:  "youtu.be video via channelId script",
			input: "https://youtu.be/dQw4w9WgXcQ",
			pages: map[string]page{
				"www.youtube.com/watch?v=dQw4w9WgXcQ": htmlPage(
					`<link rel="canonical" href="https://www.youtube.com/watch?v=dQw4w9WgXcQ">`,
					`<script>var ytInitialPlayerResponse = {"videoDetails":{"videoId":"dQw4w9WgXcQ","channelId":"`+chanE+`"}};</script>`),
				ytFeed(chanE): atomFeed("Uploader", ytChannel(chanE)),
			},
			want: []Candidate{{"https://" + ytFeed(chanE), "Uploader", ytChannel(chanE)}},
		},
		{
			name:  "youtube playlist",
			input: "https://www.youtube.com/playlist?list=PLabc123",
			pages: map[string]page{
				"www.youtube.com/feeds/videos.xml?playlist_id=PLabc123": atomFeed("A Playlist", "https://www.youtube.com/playlist?list=PLabc123"),
			},
			want: []Candidate{{
				"https://www.youtube.com/feeds/videos.xml?playlist_id=PLabc123", "A Playlist", "https://www.youtube.com/playlist?list=PLabc123",
			}},
			maxRequests: 1,
		},
		{
			name:        "youtube feed url",
			input:       "http://youtube.com/feeds/videos.xml?channel_id=" + chanX,
			pages:       map[string]page{ytFeed(chanX): atomFeed("Channel X", ytChannel(chanX))},
			want:        []Candidate{{"https://" + ytFeed(chanX), "Channel X", ytChannel(chanX)}},
			maxRequests: 1,
		},

		// Reddit
		{
			name:        "reddit subreddit",
			input:       "reddit.com/r/golang",
			pages:       map[string]page{"www.reddit.com/r/golang/.rss": atomFeed("r/golang", "https://www.reddit.com/r/golang/")},
			want:        []Candidate{{"https://www.reddit.com/r/golang/.rss", "r/golang", "https://www.reddit.com/r/golang/"}},
			maxRequests: 1,
		},
		{
			name:  "old reddit sorted subreddit keeps sort on www",
			input: "https://old.reddit.com/r/golang/top/?t=week",
			pages: map[string]page{
				"www.reddit.com/r/golang/top/.rss?t=week": atomFeed("top in golang", "https://www.reddit.com/r/golang/top/"),
				"www.reddit.com/r/golang/.rss":            atomFeed("golang", "https://www.reddit.com/r/golang/"),
			},
			want: []Candidate{
				{"https://www.reddit.com/r/golang/top/.rss?t=week", "top in golang", "https://www.reddit.com/r/golang/top/"},
				{"https://www.reddit.com/r/golang/.rss", "golang", "https://www.reddit.com/r/golang/"},
			},
		},
		{
			name:  "old reddit feed url moves to www",
			input: "https://old.reddit.com/r/golang/top/.rss",
			pages: map[string]page{"www.reddit.com/r/golang/top/.rss": atomFeed("top in golang", "https://www.reddit.com/r/golang/top/")},
			want:  []Candidate{{"https://www.reddit.com/r/golang/top/.rss", "top in golang", "https://www.reddit.com/r/golang/top/"}},
		},
		{
			name:  "reddit mobile user",
			input: "https://m.reddit.com/u/spez",
			pages: map[string]page{"www.reddit.com/user/spez/.rss": atomFeed("spez", "https://www.reddit.com/user/spez")},
			want:  []Candidate{{"https://www.reddit.com/user/spez/.rss", "spez", "https://www.reddit.com/user/spez"}},
		},

		// GitHub
		{
			name:        "github user",
			input:       "github.com/octocat",
			pages:       map[string]page{"github.com/octocat.atom": atomFeed("octocat", "https://github.com/octocat")},
			want:        []Candidate{{"https://github.com/octocat.atom", "octocat", "https://github.com/octocat"}},
			maxRequests: 1,
		},
		{
			name:  "github repo offers releases then commits",
			input: "https://www.github.com/golang/go.git",
			pages: map[string]page{
				"github.com/golang/go/releases.atom": atomFeed("Release notes from go", "https://github.com/golang/go/releases"),
				"github.com/golang/go/commits.atom":  atomFeed("Recent Commits to go", "https://github.com/golang/go/commits"),
			},
			want: []Candidate{
				{"https://github.com/golang/go/releases.atom", "Release notes from go", "https://github.com/golang/go/releases"},
				{"https://github.com/golang/go/commits.atom", "Recent Commits to go", "https://github.com/golang/go/commits"},
			},
			maxRequests: 2,
		},
		{
			name:  "github repo without releases feed",
			input: "https://github.com/golang/go/commits/master",
			pages: map[string]page{
				"github.com/golang/go/commits.atom": atomFeed("Recent Commits to go", "https://github.com/golang/go/commits"),
			},
			want: []Candidate{{"https://github.com/golang/go/commits.atom", "Recent Commits to go", "https://github.com/golang/go/commits"}},
		},
		{
			name:  "github reserved path uses generic discovery",
			input: "https://github.com/topics/go",
			pages: map[string]page{
				"github.com/topics/go":      htmlPage(`<link rel="alternate" type="application/atom+xml" href="/topics/go.atom">`, ""),
				"github.com/topics/go.atom": atomFeed("Topic go", "https://github.com/topics/go"),
			},
			want: []Candidate{{"https://github.com/topics/go.atom", "Topic go", "https://github.com/topics/go"}},
		},
		{
			name:  "failed rule falls back to generic discovery",
			input: "https://github.com/ghost",
			pages: map[string]page{
				"github.com/ghost":               htmlPage(`<link rel="alternate" type="application/atom+xml" href="/ghost/activity.atom">`, ""),
				"github.com/ghost/activity.atom": atomFeed("Ghost activity", "https://github.com/ghost"),
			},
			want: []Candidate{{"https://github.com/ghost/activity.atom", "Ghost activity", "https://github.com/ghost"}},
		},

		// other site rules
		{
			name:        "bluesky profile",
			input:       "https://bsky.app/profile/someone.bsky.social/post/3kabc",
			pages:       map[string]page{"bsky.app/profile/someone.bsky.social/rss": rssFeed("@someone.bsky.social", "https://bsky.app/profile/someone.bsky.social")},
			want:        []Candidate{{"https://bsky.app/profile/someone.bsky.social/rss", "@someone.bsky.social", "https://bsky.app/profile/someone.bsky.social"}},
			maxRequests: 1,
		},
		{
			name:        "medium user",
			input:       "https://medium.com/@writer/some-post-1234",
			pages:       map[string]page{"medium.com/feed/@writer": rssFeed("Stories by Writer", "https://medium.com/@writer")},
			want:        []Candidate{{"https://medium.com/feed/@writer", "Stories by Writer", "https://medium.com/@writer"}},
			maxRequests: 1,
		},
		{
			name:        "medium subdomain",
			input:       "https://writer.medium.com/a-post-99",
			pages:       map[string]page{"writer.medium.com/feed": rssFeed("Writer", "https://writer.medium.com")},
			want:        []Candidate{{"https://writer.medium.com/feed", "Writer", "https://writer.medium.com"}},
			maxRequests: 1,
		},
		{
			name:        "substack",
			input:       "newsletter.substack.com/p/issue-1",
			pages:       map[string]page{"newsletter.substack.com/feed": rssFeed("Newsletter", "https://newsletter.substack.com")},
			want:        []Candidate{{"https://newsletter.substack.com/feed", "Newsletter", "https://newsletter.substack.com"}},
			maxRequests: 1,
		},
		{
			name:        "stack overflow user",
			input:       "https://stackoverflow.com/users/22656/someone",
			pages:       map[string]page{"stackoverflow.com/feeds/user/22656": atomFeed("User someone", "https://stackoverflow.com/users/22656")},
			want:        []Candidate{{"https://stackoverflow.com/feeds/user/22656", "User someone", "https://stackoverflow.com/users/22656"}},
			maxRequests: 1,
		},

		// generic discovery
		{
			name:  "direct feed input",
			input: "https://feeds.example.com/main.xml",
			pages: map[string]page{"feeds.example.com/main.xml": rssFeed("Main", "https://blog.example.com/")},
			want:  []Candidate{{"https://feeds.example.com/main.xml", "Main", "https://blog.example.com/"}},
			// no probing after a direct hit
			maxRequests: 1,
		},
		{
			name:  "direct feed without site link falls back to input",
			input: "https://feeds.example.com/bare.xml",
			pages: map[string]page{"feeds.example.com/bare.xml": atomFeed("Bare", "")},
			want:  []Candidate{{"https://feeds.example.com/bare.xml", "Bare", "https://feeds.example.com/bare.xml"}},
		},
		{
			name:  "direct json feed input",
			input: "https://micro.example.org/feed.json",
			pages: map[string]page{"micro.example.org/feed.json": {
				ctype: "application/feed+json",
				body:  `{"version":"https://jsonfeed.org/version/1.1","title":"Micro","home_page_url":"https://micro.example.org/","items":[]}`,
			}},
			want: []Candidate{{"https://micro.example.org/feed.json", "Micro", "https://micro.example.org/"}},
		},
		{
			name:  "scheme-less input",
			input: "  example.net/feed.xml ",
			pages: map[string]page{"example.net/feed.xml": rssFeed("Example Net", "https://example.net/")},
			want:  []Candidate{{"https://example.net/feed.xml", "Example Net", "https://example.net/"}},
		},
		{
			name:  "feed pseudo-scheme",
			input: "feed://example.net/feed.xml",
			pages: map[string]page{"example.net/feed.xml": rssFeed("Example Net", "https://example.net/")},
			want:  []Candidate{{"https://example.net/feed.xml", "Example Net", "https://example.net/"}},
		},
		{
			name:  "same-site redirect adopts final url",
			input: "http://example.com/feed",
			pages: map[string]page{
				"example.com/feed":      {location: "https://www.example.com/feed/"},
				"www.example.com/feed/": rssFeed("Example", "https://www.example.com/"),
			},
			want: []Candidate{{"https://www.example.com/feed/", "Example", "https://www.example.com/"}},
		},
		{
			name:  "cross-site redirect keeps requested url",
			input: "https://podcast.example.com/episodes.xml",
			pages: map[string]page{
				"podcast.example.com/episodes.xml": {status: http.StatusFound, location: "https://cdn.other.net/signed.xml?sig=abc"},
				"cdn.other.net/signed.xml?sig=abc": rssFeed("Podcast", "https://podcast.example.com/"),
			},
			want: []Candidate{{"https://podcast.example.com/episodes.xml", "Podcast", "https://podcast.example.com/"}},
		},
		{
			name:  "rel alternate links",
			input: "https://blog.example.com/",
			pages: map[string]page{
				"blog.example.com/": htmlPage(`
					<link rel="alternate" type="application/json+oembed" href="/oembed?url=x">
					<link rel="alternate" type="text/html" hreflang="fr" href="/fr/">
					<link rel="alternate" type="application/json" href="/wp-json/wp/v2/pages/2">
					<link rel="alternate" type="application/rss+xml" title="Blog &raquo; Feed" href="/feed/">
					<link rel="alternate" type="application/rss+xml" title="Blog &raquo; Comments Feed" href="/comments/feed/">
					<link rel="alternate" type="application/atom+xml; charset=utf-8" title="Broken" href="/broken.atom">
					<link rel="ALTERNATE feed" type="application/atom+xml" title="Podcast" href="https://media.example.net/podcast.atom">`, ""),
				"blog.example.com/feed/":          rssFeed("Blog", "https://blog.example.com/"),
				"blog.example.com/comments/feed/": rssFeed("Blog comments", "https://blog.example.com/"),
				"blog.example.com/broken.atom":    htmlPage("", "not a feed"),
				"media.example.net/podcast.atom":  atomFeed("Podcast", "https://blog.example.com/podcast"),
			},
			want: []Candidate{
				{"https://blog.example.com/feed/", "Blog", "https://blog.example.com/"},
				{"https://media.example.net/podcast.atom", "Podcast", "https://blog.example.com/podcast"},
			},
			// page plus three candidates; comments, oembed and wp-json are never fetched
			maxRequests: 4,
		},
		{
			name:  "comment feed offered when it is the only one",
			input: "https://forum.example.com/thread",
			pages: map[string]page{
				"forum.example.com/thread":              htmlPage(`<link rel="alternate" type="application/rss+xml" title="Comments" href="/thread/comments.rss">`, ""),
				"forum.example.com/thread/comments.rss": rssFeed("Thread comments", "https://forum.example.com/thread"),
			},
			want: []Candidate{{"https://forum.example.com/thread/comments.rss", "Thread comments", "https://forum.example.com/thread"}},
		},
		{
			name:  "relative href against base element",
			input: "https://example.org/section/page.html",
			pages: map[string]page{
				"example.org/section/page.html": htmlPage(
					`<base href="/assets/"><link rel="alternate" type="application/atom+xml" href="feeds/all.atom">`, ""),
				"example.org/assets/feeds/all.atom": atomFeed("All", "https://example.org/"),
			},
			want: []Candidate{{"https://example.org/assets/feeds/all.atom", "All", "https://example.org/"}},
		},
		{
			name:  "relative href against page url",
			input: "https://example.org/section/page.html",
			pages: map[string]page{
				"example.org/section/page.html": htmlPage(`<link rel="feed" href="../feed.xml">`, ""),
				"example.org/feed.xml":          rssFeed("Rel feed", "https://example.org/"),
			},
			want: []Candidate{{"https://example.org/feed.xml", "Rel feed", "https://example.org/"}},
		},
		{
			name:  "dedupe equivalent links and formats",
			input: "https://dup.example.com/",
			pages: map[string]page{
				"dup.example.com/": htmlPage(`
					<link rel="alternate" type="application/rss+xml" href="https://dup.example.com/feed">
					<link rel="alternate" type="application/rss+xml" href="http://www.dup.example.com/feed/">
					<link rel="alternate" type="application/atom+xml" href="/feed/atom">
					<link rel="alternate" type="application/rss+xml" href="/redirected">`, ""),
				"dup.example.com/feed":       rssFeed("Dup", "https://dup.example.com/"),
				"dup.example.com/feed/atom":  atomFeed("Dup", "https://dup.example.com/"),
				"dup.example.com/redirected": {location: "https://dup.example.com/feed"},
			},
			want: []Candidate{{"https://dup.example.com/feed", "Dup", "https://dup.example.com/"}},
		},
		{
			name:  "anchor heuristics prefer the same site",
			input: "https://anchors.example.com/",
			pages: map[string]page{
				"anchors.example.com/": htmlPage("", `
					<a href="https://other.example/feed.xml">Subscribe via RSS</a>
					<a href="/feedback">Feedback</a>
					<a href="/about">About</a>
					<a href="javascript:void(0)">RSS</a>
					<a href="/rss-info">What is RSS?</a>
					<a href="/posts/atom.xml">Posts</a>`),
				"anchors.example.com/rss-info":       htmlPage("", "explainer"),
				"anchors.example.com/posts/atom.xml": atomFeed("Anchored", "https://anchors.example.com/"),
				"other.example/feed.xml":             rssFeed("Other", "https://other.example/"),
			},
			want: []Candidate{
				{"https://anchors.example.com/posts/atom.xml", "Anchored", "https://anchors.example.com/"},
				{"https://other.example/feed.xml", "Other", "https://other.example/"},
			},
			maxRequests: 4,
		},
		{
			name:  "probe beneath input path",
			input: "https://site.example/blog/",
			pages: map[string]page{
				"site.example/blog/":          htmlPage("", "<p>No feed links here.</p>"),
				"site.example/blog/index.xml": rssFeed("Hugo section", "https://site.example/blog/"),
			},
			want: []Candidate{{"https://site.example/blog/index.xml", "Hugo section", "https://site.example/blog/"}},
		},
		{
			name:  "probe site root",
			input: "https://site.example/blog/post.html",
			pages: map[string]page{
				"site.example/blog/post.html": htmlPage("", ""),
				"site.example/feed.xml":       atomFeed("Root", ""),
			},
			want: []Candidate{{"https://site.example/feed.xml", "Root", "https://site.example/blog/post.html"}},
		},
		{
			name:  "probe json feed",
			input: "https://site.example/",
			pages: map[string]page{
				"site.example/": htmlPage("", ""),
				"site.example/feed.json": {
					ctype: "application/feed+json",
					body:  `{"version":"https://jsonfeed.org/version/1","title":"Root JSON","items":[]}`,
				},
			},
			want: []Candidate{{"https://site.example/feed.json", "Root JSON", "https://site.example/"}},
		},
		{
			name:  "probe after error page",
			input: "https://gone.example/old-page",
			pages: map[string]page{
				"gone.example/rss.xml": rssFeed("Still here", "https://gone.example/"),
			},
			want: []Candidate{{"https://gone.example/rss.xml", "Still here", "https://gone.example/"}},
		},
		{
			name:  "first probe wins",
			input: "https://multi.example/",
			pages: map[string]page{
				"multi.example/":         htmlPage("", ""),
				"multi.example/rss":      rssFeed("Multi RSS", "https://multi.example/"),
				"multi.example/atom.xml": atomFeed("Multi Atom", "https://multi.example/"),
			},
			want: []Candidate{{"https://multi.example/rss", "Multi RSS", "https://multi.example/"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			web := newFakeWeb(t, tt.pages)
			got, err := web.discoverer().Discover(t.Context(), tt.input)
			if err != nil {
				t.Fatalf("Discover(%q): %v\nrequests: %q", tt.input, err, web.requested())
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("Discover(%q)\n got %+v\nwant %+v\nrequests: %q", tt.input, got, tt.want, web.requested())
			}
			if n := len(web.requested()); tt.maxRequests > 0 && n > tt.maxRequests {
				t.Errorf("made %d requests, want <= %d: %q", n, tt.maxRequests, web.requested())
			}
		})
	}
}

func TestDiscoverNoFeed(t *testing.T) {
	web := newFakeWeb(t, map[string]page{
		"nothing.example/": htmlPage(
			`<link rel="alternate" type="application/rss+xml" href="/broken.xml">`,
			`<a href="/feed-me">RSS</a><a href="/about">About</a>`),
		"nothing.example/broken.xml": {ctype: "application/rss+xml", body: "<rss><channel><title>trunc"},
		"nothing.example/feed-me":    {ctype: "application/json", body: `{"not":"a feed"}`},
	})
	got, err := web.discoverer().Discover(t.Context(), "nothing.example")
	if !errors.Is(err, ErrNoFeed) {
		t.Fatalf("err = %v, candidates %+v, want ErrNoFeed", err, got)
	}
	if err.Error() != ErrNoFeed.Error() {
		t.Errorf("message = %q", err)
	}
	if n := len(web.requested()); n > 1+maxProbes {
		t.Errorf("made %d requests, want <= %d", n, 1+maxProbes)
	}
}

func TestDiscoverFeedInHeaderCharset(t *testing.T) {
	// windows-1255 bytes whose charset only the Content-Type header names
	rss := "<?xml version=\"1.0\"?><rss version=\"2.0\"><channel><title>\xe7\xe3\xf9\xe5\xfa</title>" +
		"<link>https://hebrew.example/</link><item><title>\xf9\xec\xe5\xed</title><guid>1</guid></item></channel></rss>"
	feedPage := page{ctype: "application/rss+xml; charset=windows-1255", body: rss}
	for name, input := range map[string]string{"the URL itself": "https://hebrew.example/feeds/hebrew.xml", "a linked feed": "https://hebrew.example/"} {
		t.Run(name, func(t *testing.T) {
			web := newFakeWeb(t, map[string]page{
				"hebrew.example/":                 htmlPage(`<link rel="alternate" type="application/rss+xml" href="/feeds/hebrew.xml">`, ""),
				"hebrew.example/feeds/hebrew.xml": feedPage,
			})
			got, err := web.discoverer().Discover(t.Context(), input)
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			if len(got) != 1 || got[0].FeedURL != "https://hebrew.example/feeds/hebrew.xml" || got[0].Title != "חדשות" {
				t.Errorf("candidates = %+v", got)
			}
		})
	}
}

func TestDiscoverAnchorLimit(t *testing.T) {
	var links strings.Builder
	links.WriteString(`<a href="https://elsewhere.example/feed.xml">RSS elsewhere</a>`)
	for i := range maxAnchors + 1 {
		fmt.Fprintf(&links, `<a href="/feeds/%d">feed %d</a>`, i, i)
	}
	web := newFakeWeb(t, map[string]page{
		"cap.example/": htmlPage("", links.String()),
		// valid, but beyond the anchor cap
		fmt.Sprintf("cap.example/feeds/%d", maxAnchors): rssFeed("Too far", "https://cap.example/"),
		"elsewhere.example/feed.xml":                    rssFeed("Elsewhere", "https://elsewhere.example/"),
	})
	_, err := web.discoverer().Discover(t.Context(), "https://cap.example/")
	if !errors.Is(err, ErrNoFeed) {
		t.Fatalf("err = %v, want ErrNoFeed", err)
	}
	for _, r := range web.requested() {
		if strings.Contains(r, "elsewhere.example") || strings.HasSuffix(r, fmt.Sprintf("/feeds/%d", maxAnchors)) {
			t.Errorf("fetched %s beyond the same-site anchor cap", r)
		}
	}
}

func TestDiscoverProbeBudget(t *testing.T) {
	var head strings.Builder
	for i := range maxProbes + 5 {
		fmt.Fprintf(&head, `<link rel="alternate" type="application/rss+xml" href="/broken/%d.xml">`, i)
	}
	web := newFakeWeb(t, map[string]page{"budget.example/": htmlPage(head.String(), "")})
	if _, err := web.discoverer().Discover(t.Context(), "https://budget.example/"); !errors.Is(err, ErrNoFeed) {
		t.Fatalf("err = %v, want ErrNoFeed", err)
	}
	if n := len(web.requested()); n != 1+maxProbes {
		t.Errorf("made %d requests, want exactly %d", n, 1+maxProbes)
	}
}

func TestDiscoverHTTPFallbackForSchemelessInput(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	pages := map[string]page{
		"plain.example/":        htmlPage(`<link rel="alternate" type="application/rss+xml" href="/rss.xml">`, ""),
		"plain.example/rss.xml": atomFeed("Plain", ""),
	}
	failHTTPS := func(r *http.Request) error {
		if r.URL.Scheme == "https" {
			return refused
		}
		return nil
	}

	t.Run("scheme-less input retries over http", func(t *testing.T) {
		web := newFakeWeb(t, pages)
		web.fail = failHTTPS
		got, err := web.discoverer().Discover(t.Context(), "plain.example")
		if err != nil {
			t.Fatalf("Discover: %v (requests %q)", err, web.requested())
		}
		want := []Candidate{{"http://plain.example/rss.xml", "Plain", "http://plain.example/"}}
		if !slices.Equal(got, want) {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})

	t.Run("explicit https is not downgraded", func(t *testing.T) {
		web := newFakeWeb(t, pages)
		web.fail = failHTTPS
		_, err := web.discoverer().Discover(t.Context(), "https://plain.example")
		if !errors.Is(err, ErrNoFeed) || !errors.Is(err, syscall.ECONNREFUSED) {
			t.Fatalf("err = %v, want ErrNoFeed wrapping connection refused", err)
		}
		for _, r := range web.requested() {
			if strings.HasPrefix(r, "http://") {
				t.Errorf("downgraded to %s", r)
			}
		}
	})
}

func TestDiscoverNetworkError(t *testing.T) {
	web := newFakeWeb(t, nil)
	web.fail = func(r *http.Request) error {
		return &net.DNSError{Err: "no such host", Name: r.URL.Hostname(), IsNotFound: true}
	}
	_, err := web.discoverer().Discover(t.Context(), "nxdomain.example")
	if !errors.Is(err, ErrNoFeed) || !strings.Contains(err.Error(), "host not found: nxdomain.example") {
		t.Fatalf("err = %v", err)
	}
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) {
		t.Errorf("errors.As(err, *net.DNSError) = false")
	}
	// DNS failures are not retried over http, and nothing is probed
	if n := len(web.requested()); n != 1 {
		t.Errorf("made %d requests, want 1: %q", n, web.requested())
	}
}

func TestDiscoverInvalidInput(t *testing.T) {
	d := New(feed.NewClient(feed.Options{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("unexpected request to %s", r.URL)
		return nil, errors.New("unexpected request")
	})}))
	inputs := []string{
		"", "   ", "ftp://example.com/feed", "https://", "http://exa mple.com/", "mailto:me@example.com",
		"javascript:alert(1)", "http://[::1", "feed:", "user:pw@example.com",
	}
	for _, input := range inputs {
		if _, err := d.Discover(t.Context(), input); !errors.Is(err, ErrInvalidURL) {
			t.Errorf("Discover(%q) err = %v, want ErrInvalidURL", input, err)
		}
	}
}

func TestDiscoverCancelled(t *testing.T) {
	web := newFakeWeb(t, map[string]page{"slow.example/": htmlPage("", "")})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := web.discoverer().Discover(ctx, "https://slow.example/"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestNewNilClient(t *testing.T) {
	if d := New(nil); d.client == nil {
		t.Fatal("New(nil) left the client nil")
	}
}

func TestNormalizeInput(t *testing.T) {
	tests := []struct {
		in          string
		want        string
		schemeGiven bool
	}{
		{"example.com", "https://example.com/", false},
		{"Example.COM/Path?q=1#frag", "https://example.com/Path?q=1", false},
		{"//example.com/x", "https://example.com/x", false},
		{"HTTP://example.com", "http://example.com/", true},
		{"https://user:pw@example.com/", "https://example.com/", true},
		{"feed:https://example.com/rss", "https://example.com/rss", true},
		{"feed://example.com/rss", "https://example.com/rss", true},
		{"localhost:8080/feed", "https://localhost:8080/feed", false},
	}
	for _, tt := range tests {
		u, schemeGiven, err := normalizeInput(tt.in)
		if err != nil {
			t.Errorf("normalizeInput(%q): %v", tt.in, err)
			continue
		}
		if u.String() != tt.want || schemeGiven != tt.schemeGiven {
			t.Errorf("normalizeInput(%q) = %q, %v; want %q, %v", tt.in, u, schemeGiven, tt.want, tt.schemeGiven)
		}
	}
}

func TestURLKey(t *testing.T) {
	same := [][]string{
		{"https://example.com/feed", "http://www.example.com/feed/", "https://EXAMPLE.com:443/feed#x"},
		{"https://example.com/?a=1", "https://example.com?a=1"},
	}
	for _, group := range same {
		for _, u := range group[1:] {
			if urlKey(u) != urlKey(group[0]) {
				t.Errorf("urlKey(%q) = %q, want %q", u, urlKey(u), urlKey(group[0]))
			}
		}
	}
	different := [][2]string{
		{"https://example.com/feed", "https://example.com/rss"},
		{"https://example.com/?a=1", "https://example.com/?a=2"},
		{"https://example.com/", "https://example.com:8443/"},
		{"https://a.example.com/", "https://b.example.com/"},
	}
	for _, pair := range different {
		if urlKey(pair[0]) == urlKey(pair[1]) {
			t.Errorf("urlKey(%q) == urlKey(%q)", pair[0], pair[1])
		}
	}
	if urlKey("not a url") != "" || urlKey("/relative") != "" {
		t.Error("urlKey accepted a URL without host")
	}
}

func TestSiteRule(t *testing.T) {
	tests := []struct {
		in    string
		feeds []string
		page  string
	}{
		{"https://www.youtube.com/channel/UCsXVk37bltHxD1rDPwtNM8Q", []string{youtubeFeeds + "?channel_id=UCsXVk37bltHxD1rDPwtNM8Q"}, ""},
		{"https://m.youtube.com/@handle/videos", nil, "https://www.youtube.com/@handle/videos"},
		{"https://www.youtube.com/watch?v=abc&t=10", nil, "https://www.youtube.com/watch?v=abc&t=10"},
		{"https://www.youtube.com/feeds/videos.xml?user=legacy", []string{youtubeFeeds + "?user=legacy"}, ""},
		{"https://youtu.be/abc", nil, "https://www.youtube.com/watch?v=abc"},
		{"https://www.reddit.com/r/golang/comments/abc/some_title/", []string{"https://www.reddit.com/r/golang/comments/abc/some_title/.rss"}, ""},
		{"https://www.reddit.com/r/golang/wiki", []string{"https://www.reddit.com/r/golang/.rss"}, ""},
		{"https://reddit.com/user/someone/comments", []string{"https://www.reddit.com/user/someone/.rss"}, ""},
		{"https://www.reddit.com/", nil, ""},
		{"https://github.com/", nil, ""},
		{"https://github.com/octocat.atom", []string{"https://github.com/octocat.atom"}, ""},
		{"https://bsky.app/", nil, ""},
		{"https://medium.com/feed/@writer", []string{"https://medium.com/feed/@writer"}, ""},
		{"https://medium.com/tag/golang", []string{"https://medium.com/feed/tag/golang"}, ""},
		{"https://medium.com/some-publication/a-post", []string{"https://medium.com/feed/some-publication"}, ""},
		{"https://medium.com/p/abc123", nil, ""},
		{"https://superuser.com/users/42/name", []string{"https://superuser.com/feeds/user/42"}, ""},
		{"https://unix.stackexchange.com/users/7", []string{"https://unix.stackexchange.com/feeds/user/7"}, ""},
		{"https://stackoverflow.com/users/login", nil, ""},
		{"https://substack.com/@someone", nil, ""},
		{"https://example.com/r/golang", nil, ""},
	}
	for _, tt := range tests {
		u, _, err := normalizeInput(tt.in)
		if err != nil {
			t.Fatal(err)
		}
		feeds, page := siteRule(u)
		if !slices.Equal(feeds, tt.feeds) || page != tt.page {
			t.Errorf("siteRule(%q) = %q, %q; want %q, %q", tt.in, feeds, page, tt.feeds, tt.page)
		}
	}
}

func TestProbeURLs(t *testing.T) {
	tests := []struct {
		in    string
		first []string
		n     int
	}{
		{"https://example.com/", []string{"https://example.com/feed", "https://example.com/rss"}, len(probePaths)},
		{"https://example.com/index.html", []string{"https://example.com/feed"}, len(probePaths)},
		{"https://example.com/blog", []string{"https://example.com/blog/feed", "https://example.com/feed"}, 2 * len(probePaths)},
		{"https://example.com/blog/post.html?x=1", []string{"https://example.com/blog/feed", "https://example.com/feed"}, 2 * len(probePaths)},
	}
	for _, tt := range tests {
		got := probeURLs(tt.in)
		if len(got) != tt.n || !slices.Equal(got[:len(tt.first)], tt.first) {
			t.Errorf("probeURLs(%q) = %q", tt.in, got)
		}
	}
}

func TestDiscoverYouTubeWithoutChannelDoesNotGuess(t *testing.T) {
	web := newFakeWeb(t, map[string]page{
		"www.youtube.com/@ghost": htmlPage("", `<a href="/feed/subscriptions">Subscriptions</a><a href="/feed/history">History</a>`),
	})
	_, err := web.discoverer().Discover(t.Context(), "https://www.youtube.com/@ghost")
	if !errors.Is(err, ErrNoFeed) {
		t.Fatalf("err = %v, want ErrNoFeed", err)
	}
	if got := web.requested(); len(got) != 1 {
		t.Errorf("requests = %q, want only the channel page", got)
	}
}

func TestDiscoverConcurrentCalls(t *testing.T) {
	web := newFakeWeb(t, map[string]page{
		"www.reddit.com/r/golang/.rss": atomFeed("r/golang", "https://www.reddit.com/r/golang/"),
		"github.com/octocat.atom":      atomFeed("octocat", "https://github.com/octocat"),
	})
	d := web.discoverer()
	var wg sync.WaitGroup
	for range 4 {
		for _, input := range []string{"reddit.com/r/golang", "github.com/octocat"} {
			wg.Go(func() {
				if got, err := d.Discover(t.Context(), input); err != nil || len(got) != 1 {
					t.Errorf("Discover(%q) = %+v, %v", input, got, err)
				}
			})
		}
	}
	wg.Wait()
}

func TestDiscoverReportsMissingPage(t *testing.T) {
	tests := []struct {
		name, input string
		pages       map[string]page
		maxRequests int
	}{
		{
			name:        "mistyped youtube handle",
			input:       "https://www.youtube.com/@AccursedFarms",
			pages:       map[string]page{"www.youtube.com/@AccursedFarms": {status: http.StatusNotFound, ctype: "text/html", body: "<title>404 Not Found</title>"}},
			maxRequests: 1,
		},
		{
			name:  "dead blog page",
			input: "https://gone.example/blog/post",
			pages: map[string]page{"gone.example/blog/post": {status: http.StatusGone, ctype: "text/html", body: "gone"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			web := newFakeWeb(t, tt.pages)
			_, err := web.discoverer().Discover(t.Context(), tt.input)
			if !errors.Is(err, ErrNoFeed) {
				t.Fatalf("err = %v, want ErrNoFeed", err)
			}
			status := tt.pages[strings.TrimPrefix(tt.input, "https://")].status
			if want := fmt.Sprintf("the page returned HTTP %d %s", status, http.StatusText(status)); !strings.Contains(err.Error(), want) {
				t.Errorf("err = %q, want it to mention %q", err, want)
			}
			if n := len(web.requested()); tt.maxRequests > 0 && n > tt.maxRequests {
				t.Errorf("made %d requests, want <= %d", n, tt.maxRequests)
			}
		})
	}
}
