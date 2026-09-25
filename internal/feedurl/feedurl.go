// Package feedurl maps the feed URLs stalker stores to the addresses it
// fetches, so every package agrees on when two follows are the same feed.
package feedurl

import (
	"net/url"
	"strings"
)

// Canonical returns the address a stored feed URL is fetched from; follows
// whose feed URLs share it are the same feed. Fraidycat follows YouTube
// channel and playlist pages directly (its scraper reads their feeds), so its
// exports contain them as feed URLs, and old.reddit.com redirects every feed
// request to a login page while www.reddit.com still serves the same feed.
// Any other URL is returned unchanged.
func Canonical(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return raw
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case IsYouTubeHost(host):
		segs := strings.Split(strings.Trim(u.Path, "/"), "/")
		switch {
		case len(segs) >= 2 && segs[0] == "channel" && strings.HasPrefix(segs[1], "UC"):
			return "https://www.youtube.com/feeds/videos.xml?channel_id=" + url.QueryEscape(segs[1])
		case len(segs) == 1 && segs[0] == "playlist" && u.Query().Get("list") != "":
			return "https://www.youtube.com/feeds/videos.xml?playlist_id=" + url.QueryEscape(u.Query().Get("list"))
		}
	case host == "old.reddit.com" && strings.HasSuffix(u.Path, ".rss"):
		u.Scheme, u.Host = "https", "www.reddit.com"
		return u.String()
	}
	return raw
}

// IsYouTubeHost reports whether host is youtube.com or one of its subdomains.
func IsYouTubeHost(host string) bool {
	host = strings.ToLower(host)
	return host == "youtube.com" || strings.HasSuffix(host, ".youtube.com")
}
