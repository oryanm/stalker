package discover

import (
	"net/url"
	"slices"
	"strings"
)

const youtubeFeeds = "https://www.youtube.com/feeds/videos.xml"

var (
	redditSorts = map[string]bool{
		"best": true, "controversial": true, "gilded": true, "hot": true, "new": true, "rising": true, "top": true,
	}
	githubReserved = map[string]bool{
		"about": true, "apps": true, "collections": true, "customer-stories": true, "enterprise": true,
		"events": true, "explore": true, "features": true, "issues": true, "login": true, "marketplace": true,
		"new": true, "notifications": true, "orgs": true, "pricing": true, "pulls": true, "search": true,
		"security": true, "settings": true, "site": true, "sponsors": true, "topics": true, "trending": true,
	}
	mediumReserved = map[string]bool{
		"about": true, "creators": true, "m": true, "me": true, "membership": true, "p": true, "plans": true,
		"search": true, "topics": true,
	}
	stackExchangeSites = map[string]bool{
		"askubuntu.com": true, "mathoverflow.net": true, "serverfault.com": true, "stackapps.com": true,
		"stackoverflow.com": true, "superuser.com": true,
	}
)

// siteRule maps URLs of sites with predictable feeds to candidate feed URLs,
// most likely first. page, when set, replaces the input for generic discovery.
func siteRule(u *url.URL) (feeds []string, page string) {
	host := strings.ToLower(u.Hostname())
	site := siteHost(host)
	segs := pathSegments(u.Path)
	switch {
	case site == "youtube.com":
		return youtubeRule(u, segs)
	case site == "youtu.be":
		if len(segs) == 1 {
			return nil, "https://www.youtube.com/watch?v=" + url.QueryEscape(segs[0])
		}
	case site == "reddit.com":
		return redditRule(u, segs), ""
	case site == "github.com":
		return githubRule(u, segs), ""
	case site == "bsky.app":
		if len(segs) >= 2 && segs[0] == "profile" {
			return []string{"https://bsky.app/profile/" + url.PathEscape(segs[1]) + "/rss"}, ""
		}
	case site == "medium.com":
		return mediumRule(u, segs), ""
	case strings.HasSuffix(site, ".medium.com"), strings.HasSuffix(site, ".substack.com"):
		return []string{"https://" + site + "/feed"}, ""
	case stackExchangeSites[site] || strings.HasSuffix(site, ".stackexchange.com") || strings.HasSuffix(site, ".stackoverflow.com"):
		if len(segs) >= 2 && segs[0] == "users" && isDigits(segs[1]) {
			return []string{"https://" + site + "/feeds/user/" + segs[1]}, ""
		}
	}
	return nil, ""
}

func youtubeRule(u *url.URL, segs []string) (feeds []string, page string) {
	switch {
	case len(segs) >= 2 && segs[0] == "channel" && strings.HasPrefix(segs[1], "UC"):
		return []string{youtubeChannelFeed(segs[1])}, ""
	case len(segs) == 1 && segs[0] == "playlist" && u.Query().Get("list") != "":
		return []string{youtubeFeeds + "?playlist_id=" + url.QueryEscape(u.Query().Get("list"))}, ""
	case len(segs) == 2 && segs[0] == "feeds" && segs[1] == "videos.xml" && u.RawQuery != "":
		return []string{youtubeFeeds + "?" + u.RawQuery}, ""
	}
	// handles, custom URLs, legacy usernames and videos need the page to learn the channel id;
	// the mobile site serves different markup, so always read the desktop page
	p := *u
	p.Scheme, p.Host = "https", "www.youtube.com"
	return nil, p.String()
}

func youtubeChannelFeed(id string) string {
	return youtubeFeeds + "?channel_id=" + url.QueryEscape(id)
}

// redditRule always uses www.reddit.com: old.reddit.com redirects feed requests to a login page.
func redditRule(u *url.URL, segs []string) []string {
	const base = "https://www.reddit.com"
	if strings.HasSuffix(u.Path, ".rss") {
		return []string{base + u.EscapedPath() + query(u)}
	}
	if len(segs) < 2 {
		return nil
	}
	name := url.PathEscape(segs[1])
	switch segs[0] {
	case "r":
		sub := base + "/r/" + name
		switch {
		case len(segs) >= 4 && segs[2] == "comments":
			return []string{base + strings.TrimRight(u.EscapedPath(), "/") + "/.rss"}
		case len(segs) >= 3 && redditSorts[segs[2]]:
			// the sorted listing is what the user looked at; the plain subreddit is offered too
			return []string{sub + "/" + segs[2] + "/.rss" + query(u), sub + "/.rss"}
		}
		return []string{sub + "/.rss"}
	case "user", "u":
		return []string{base + "/user/" + name + "/.rss"}
	}
	return nil
}

func githubRule(u *url.URL, segs []string) []string {
	if len(segs) == 0 || githubReserved[strings.ToLower(segs[0])] {
		return nil
	}
	if strings.HasSuffix(u.Path, ".atom") {
		return []string{"https://github.com" + u.EscapedPath() + query(u)}
	}
	user := "https://github.com/" + url.PathEscape(segs[0])
	if len(segs) == 1 {
		return []string{user + ".atom"}
	}
	repo := user + "/" + url.PathEscape(strings.TrimSuffix(segs[1], ".git"))
	feeds := []string{repo + "/releases.atom", repo + "/commits.atom"}
	if len(segs) >= 3 && segs[2] == "commits" {
		slices.Reverse(feeds)
	}
	return feeds
}

func mediumRule(u *url.URL, segs []string) []string {
	switch {
	case len(segs) == 0:
		return nil
	case segs[0] == "feed":
		return []string{"https://medium.com" + u.EscapedPath()}
	case strings.HasPrefix(segs[0], "@"):
		return []string{"https://medium.com/feed/" + url.PathEscape(segs[0])}
	case segs[0] == "tag" && len(segs) >= 2:
		return []string{"https://medium.com/feed/tag/" + url.PathEscape(segs[1])}
	case !mediumReserved[segs[0]]:
		// publications live at medium.com/NAME
		return []string{"https://medium.com/feed/" + url.PathEscape(segs[0])}
	}
	return nil
}

func query(u *url.URL) string {
	if u.RawQuery == "" {
		return ""
	}
	return "?" + u.RawQuery
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
