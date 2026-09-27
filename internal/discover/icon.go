package discover

import (
	"bytes"
	"context"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"github.com/oryanm/stalker/internal/feed"
)

const (
	// iconSize is the smallest icon that stays sharp where the UI shows it on a high-density screen.
	iconSize = 32
	// unsizedIcon is what a <link rel="icon"> without sizes is assumed to be: usually a favicon.ico.
	unsizedIcon = 16
	// touchIconSize is the size iOS asks apple-touch-icons to be.
	touchIconSize = 180
)

// ytAvatarSize matches the size option of a YouTube avatar URL (…=s900-c-k-…).
var ytAvatarSize = regexp.MustCompile(`=s\d+(-|$)`)

// Icon finds a small picture for the site whose home page is pageURL: the
// channel avatar of a YouTube page, else the page's <link rel="icon"> or
// apple-touch-icon nearest above 32px, else the site's /favicon.ico when it
// serves an image there. It returns "" without an error when the site has no
// icon, and an error when the page could not be fetched.
func Icon(ctx context.Context, c *feed.Client, pageURL string) (string, error) {
	resp, err := c.Get(ctx, pageURL, nil)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 && isHTML(resp) {
		if doc, err := goquery.NewDocumentFromReader(bytes.NewReader(resp.Body)); err == nil {
			base := documentBase(doc, resp.FinalURL)
			if isYouTube(resp.FinalURL) {
				if u := youtubeAvatar(doc, base); u != "" {
					return u, nil
				}
			}
			if u := linkIcon(doc, base); u != "" {
				return u, nil
			}
		}
	}
	return favicon(ctx, c, resp.FinalURL)
}

// youtubeAvatar is the og:image of a channel page, resized to what the UI needs.
func youtubeAvatar(doc *goquery.Document, base *url.URL) string {
	raw := absURL(base, doc.Find(`meta[property="og:image"]`).AttrOr("content", ""))
	u, err := url.Parse(raw)
	if err != nil || raw == "" {
		return ""
	}
	// the page asks for 900px; the image server scales to any size
	if host := u.Hostname(); strings.HasSuffix(host, ".googleusercontent.com") || strings.HasSuffix(host, ".ggpht.com") {
		u.Path = ytAvatarSize.ReplaceAllString(u.Path, "=s88${1}")
	}
	return u.String()
}

// linkIcon picks among the icons a page declares the smallest that is at least
// iconSize, else the largest.
func linkIcon(doc *goquery.Document, base *url.URL) string {
	best, bestSize := "", 0
	better := func(size int) bool {
		switch {
		case best == "":
			return true
		case bestSize >= iconSize:
			return size >= iconSize && size < bestSize
		}
		return size > bestSize
	}
	doc.Find("link[href]").Each(func(_ int, sel *goquery.Selection) {
		rels := strings.Fields(strings.ToLower(sel.AttrOr("rel", "")))
		touch := slices.Contains(rels, "apple-touch-icon") || slices.Contains(rels, "apple-touch-icon-precomposed")
		if !touch && !slices.Contains(rels, "icon") {
			return
		}
		href := absURL(base, sel.AttrOr("href", ""))
		if href == "" {
			return
		}
		size := declaredSize(sel.AttrOr("sizes", ""), sel.AttrOr("type", ""), href)
		if size == 0 {
			size = unsizedIcon
			if touch {
				size = touchIconSize
			}
		}
		if better(size) {
			best, bestSize = href, size
		}
	})
	return best
}

// declaredSize is the largest width in a sizes attribute ("16x16 32x32"),
// iconSize for vector icons, and 0 when unknown.
func declaredSize(sizes, typ, href string) int {
	if strings.EqualFold(strings.TrimSpace(typ), "image/svg+xml") || strings.HasSuffix(strings.ToLower(href), ".svg") {
		return iconSize
	}
	size := 0
	for _, s := range strings.Fields(strings.ToLower(sizes)) {
		if s == "any" {
			return iconSize
		}
		w, _, _ := strings.Cut(s, "x")
		if n, err := strconv.Atoi(w); err == nil {
			size = max(size, n)
		}
	}
	return size
}

// favicon returns the site root's /favicon.ico when it is an image; sites
// without one often answer with an HTML page instead of a 404.
func favicon(ctx context.Context, c *feed.Client, pageURL string) (string, error) {
	u, err := url.Parse(pageURL)
	if err != nil || u.Host == "" {
		return "", nil
	}
	ico := (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/favicon.ico"}).String()
	resp, err := c.Get(ctx, ico, nil)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 || len(resp.Body) == 0 {
		return "", nil
	}
	typ, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if !strings.HasPrefix(typ, "image/") && !strings.HasPrefix(http.DetectContentType(resp.Body), "image/") {
		return "", nil
	}
	return ico, nil
}
