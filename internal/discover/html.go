package discover

import (
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// probePaths are where blog engines (WordPress, Hugo, Jekyll, Ghost, ...) put their feeds.
var probePaths = []string{"feed", "rss", "rss.xml", "atom.xml", "feed.xml", "index.xml", "feed.json"}

var (
	// feedWord matches rss/atom/feed(s) as a word, so "feedback" and "atomic" do not count
	feedWord = regexp.MustCompile(`(?i)(?:^|[^a-z])(?:rss|atom|feeds?)(?:[^a-z]|$)`)

	ytChannelPath = regexp.MustCompile(`/channel/(UC[\w-]+)`)
	ytExternalID  = regexp.MustCompile(`"externalId"\s*:\s*"(UC[\w-]{22})"`)
	ytChannelID   = regexp.MustCompile(`"channelId"\s*:\s*"(UC[\w-]{22})"`)
)

// documentBase is the URL relative links resolve against: <base href> if present, else the page URL.
func documentBase(doc *goquery.Document, pageURL string) *url.URL {
	base, err := url.Parse(pageURL)
	if err != nil {
		return nil
	}
	if href, ok := doc.Find("base[href]").First().Attr("href"); ok {
		if b, err := base.Parse(strings.TrimSpace(href)); err == nil && (b.Scheme == "http" || b.Scheme == "https") {
			return b
		}
	}
	return base
}

// absURL resolves href against base and keeps only http(s) URLs.
func absURL(base *url.URL, href string) string {
	href = strings.TrimSpace(href)
	if href == "" || base == nil {
		return ""
	}
	u, err := base.Parse(href)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	u.Fragment, u.RawFragment = "", ""
	return u.String()
}

func isFeedType(t string) bool {
	t, _, _ = strings.Cut(strings.ToLower(t), ";")
	switch strings.TrimSpace(t) {
	case "application/rss+xml", "application/atom+xml", "application/rdf+xml", "application/feed+json",
		"application/json", "application/xml", "text/xml", "application/x-rss+xml", "application/x-atom+xml",
		"application/x.atom+xml", "text/rss+xml", "text/atom+xml":
		return true
	}
	return false
}

// alternateLinks returns the feeds a page advertises with <link rel="alternate">
// or rel="feed". Comment feeds are split off so they are only offered when
// the page has nothing else.
func alternateLinks(doc *goquery.Document, base *url.URL) (primary, comments []string) {
	doc.Find("link[href]").Each(func(_ int, sel *goquery.Selection) {
		rels := strings.Fields(strings.ToLower(sel.AttrOr("rel", "")))
		typ := sel.AttrOr("type", "")
		switch {
		case slices.Contains(rels, "alternate") && isFeedType(typ):
		case slices.Contains(rels, "feed") && (strings.TrimSpace(typ) == "" || isFeedType(typ)):
		default:
			return
		}
		href := absURL(base, sel.AttrOr("href", ""))
		// WordPress advertises REST API documents as application/json alternates
		if href == "" || strings.Contains(href, "/wp-json/") {
			return
		}
		if strings.Contains(strings.ToLower(sel.AttrOr("title", "")+" "+href), "comment") {
			comments = append(comments, href)
		} else {
			primary = append(primary, href)
		}
	})
	return primary, comments
}

// feedAnchors returns up to maxAnchors links whose URL or text mentions a
// feed, links on the page's own site first.
func feedAnchors(doc *goquery.Document, base *url.URL, pageURL string) []string {
	type anchor struct {
		href     string
		sameSite bool
	}
	var anchors []anchor
	seen := map[string]bool{}
	doc.Find("a[href]").Each(func(_ int, sel *goquery.Selection) {
		href := absURL(base, sel.AttrOr("href", ""))
		if href == "" || seen[href] {
			return
		}
		u, err := url.Parse(href)
		if err != nil {
			return
		}
		host := strings.ToLower(u.Hostname())
		mentions := feedWord.MatchString(u.EscapedPath()+"?"+u.RawQuery) ||
			strings.HasPrefix(host, "feeds.") || strings.HasPrefix(host, "feed.") || strings.HasPrefix(host, "rss.") ||
			feedWord.MatchString(sel.Text()) || feedWord.MatchString(sel.AttrOr("title", ""))
		if !mentions {
			return
		}
		seen[href] = true
		anchors = append(anchors, anchor{href: href, sameSite: sameSite(href, pageURL)})
	})
	slices.SortStableFunc(anchors, func(a, b anchor) int {
		switch {
		case a.sameSite == b.sameSite:
			return 0
		case a.sameSite:
			return -1
		}
		return 1
	})
	var out []string
	for _, a := range anchors[:min(len(anchors), maxAnchors)] {
		out = append(out, a.href)
	}
	return out
}

// probeURLs guesses feed locations beneath the page's directory and the site
// root, interleaved so the most common names are tried first at both.
func probeURLs(pageURL string) []string {
	u, err := url.Parse(pageURL)
	if err != nil || u.Host == "" {
		return nil
	}
	dir := u.Path
	if !strings.HasSuffix(dir, "/") && strings.Contains(path.Base(dir), ".") {
		dir = path.Dir(dir)
	}
	dirs := []string{"/"}
	if dir = strings.Trim(dir, "/"); dir != "" {
		dirs = []string{"/" + dir + "/", "/"}
	}
	var out []string
	for _, p := range probePaths {
		for _, d := range dirs {
			probe := url.URL{Scheme: u.Scheme, Host: u.Host, Path: d + p}
			out = append(out, probe.String())
		}
	}
	return out
}

func isYouTube(pageURL string) bool {
	return siteHost(hostOf(pageURL)) == "youtube.com"
}

// youtubeChannelID finds the channel of a YouTube channel or video page.
func youtubeChannelID(doc *goquery.Document, body []byte) string {
	var id string
	doc.Find(`link[rel~="alternate"][href]`).EachWithBreak(func(_ int, sel *goquery.Selection) bool {
		if u, err := url.Parse(sel.AttrOr("href", "")); err == nil && strings.HasSuffix(u.Path, "/feeds/videos.xml") {
			id = u.Query().Get("channel_id")
		}
		return id == ""
	})
	if strings.HasPrefix(id, "UC") {
		return id
	}
	for _, sel := range []string{`link[rel="canonical"]`, `meta[property="og:url"]`} {
		attr := "href"
		if strings.HasPrefix(sel, "meta") {
			attr = "content"
		}
		if m := ytChannelPath.FindStringSubmatch(doc.Find(sel).AttrOr(attr, "")); m != nil {
			return m[1]
		}
	}
	for _, sel := range []string{`meta[itemprop="identifier"]`, `meta[itemprop="channelId"]`} {
		if v := strings.TrimSpace(doc.Find(sel).AttrOr("content", "")); strings.HasPrefix(v, "UC") {
			return v
		}
	}
	for _, re := range []*regexp.Regexp{ytExternalID, ytChannelID} {
		if m := re.FindSubmatch(body); m != nil {
			return string(m[1])
		}
	}
	return ""
}
