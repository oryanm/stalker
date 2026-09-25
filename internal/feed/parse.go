package feed

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"
	ext "github.com/mmcdole/gofeed/extensions"

	"github.com/oryanm/stalker/internal/model"
)

// ErrNotAFeed means the document could not be parsed as RSS, Atom or JSON Feed.
var ErrNotAFeed = fmt.Errorf("not an RSS, Atom or JSON feed")

// ErrTooManyEntries means a document holds more entries than any real feed,
// which the parser would need hundreds of megabytes to read.
var ErrTooManyEntries = errors.New("feed has too many entries")

const (
	maxTitleRunes       = 300
	maxExcerptRunes     = 140
	maxDescriptionRunes = 500
	untitled            = "Untitled"
	// maxPosts bounds what one document yields; the store keeps even fewer.
	maxPosts = 200
	// maxEntries is far beyond the longest real feeds (podcasts with thousands
	// of episodes) and bounds the parser's memory, which peaks at up to 2 KB per entry.
	maxEntries = 10000
	// jsonObjectsPerEntry allows for the author and attachment objects of a JSON Feed item.
	jsonObjectsPerEntry = 4
)

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// Parse parses a feed body. baseURL resolves relative links. Posts get a GUID
// (item GUID, else link, else a hash of title and date), a trimmed title (falls
// back to text from the description, then "Untitled"), absolute URLs and
// PublishedAt/UpdatedAt (zero when absent; PublishedAt falls back to UpdatedAt).
//
// Only the newest 200 posts are returned, undated ones counting as oldest.
// Documents with more than 10000 entries fail with ErrTooManyEntries.
func Parse(body []byte, baseURL string) (res *Result, err error) {
	// a parser bug triggered by a hostile document must not take the poller down
	defer func() {
		if r := recover(); r != nil {
			res, err = nil, fmt.Errorf("%w: parser panic: %v", ErrNotAFeed, r)
		}
	}()

	// gofeed skips a BOM when sniffing but encoding/json rejects it
	body = bytes.TrimPrefix(body, utf8BOM)
	if tooManyEntries(body) {
		return nil, fmt.Errorf("%w (more than %d)", ErrTooManyEntries, maxEntries)
	}
	f, err := gofeed.NewParser().Parse(bytes.NewReader(body))
	if err != nil {
		if errors.Is(err, gofeed.ErrFeedTypeNotDetected) {
			return nil, ErrNotAFeed
		}
		return nil, fmt.Errorf("%w: %w", ErrNotAFeed, err)
	}
	// gofeed accepts any JSON object, so an API response would otherwise pass as an empty feed
	if f.FeedType == "json" && !strings.Contains(strings.ToLower(f.FeedVersion), "jsonfeed.org/version") {
		return nil, ErrNotAFeed
	}

	base := parseBase(baseURL)
	res = &Result{
		Title:       truncate(titleText(f.Title), maxTitleRunes),
		Description: truncate(plainText(f.Description), maxDescriptionRunes),
		SiteURL:     resolve(base, f.Link),
	}
	if f.Image != nil {
		res.ImageURL = resolve(base, f.Image.URL)
	}
	if res.ImageURL == "" && f.ITunesExt != nil {
		res.ImageURL = resolve(base, f.ITunesExt.Image)
	}

	itemBase := base
	if res.SiteURL != "" {
		itemBase, _ = url.Parse(res.SiteURL)
	}
	// Atom and JSON Feed define feed authors as the default for entries; RSS has no such rule
	var feedAuthor string
	if f.FeedType == "atom" || f.FeedType == "json" {
		feedAuthor = personName(f.Authors, f.Author)
	}
	res.Posts = make([]model.Post, 0, len(f.Items))
	seen := make(map[string]bool, len(f.Items))
	for _, it := range f.Items {
		if it == nil {
			continue
		}
		p := post(it, itemBase)
		if p.Author == "" {
			p.Author = feedAuthor
		}
		if seen[p.GUID] {
			continue
		}
		seen[p.GUID] = true
		res.Posts = append(res.Posts, p)
	}
	slices.SortStableFunc(res.Posts, func(a, b model.Post) int {
		switch az, bz := a.PublishedAt.IsZero(), b.PublishedAt.IsZero(); {
		case az && bz:
			return 0
		case az:
			return 1
		case bz:
			return -1
		}
		return b.PublishedAt.Compare(a.PublishedAt)
	})
	if len(res.Posts) > maxPosts {
		res.Posts = slices.Clip(res.Posts[:maxPosts])
	}
	return res, nil
}

// tooManyEntries counts, cheaply and generously, the entries of an XML
// document (item and entry elements in any case or namespace, as gofeed reads
// them) or the objects of a JSON one, and reports whether there are too many.
func tooManyEntries(body []byte) bool {
	if trimmed := bytes.TrimLeft(body, " \t\r\n"); len(trimmed) > 0 && trimmed[0] == '{' {
		objects, inString, escaped := 0, false, false
		for _, c := range trimmed {
			switch {
			case escaped:
				escaped = false
			case inString:
				escaped = c == '\\'
				inString = c != '"'
			case c == '"':
				inString = true
			case c == '{':
				if objects++; objects > maxEntries*jsonObjectsPerEntry {
					return true
				}
			}
		}
		return false
	}
	entries := 0
	for rest := body; ; {
		i := bytes.IndexByte(rest, '<')
		if i < 0 {
			return false
		}
		rest = rest[i+1:]
		end := bytes.IndexAny(rest, " \t\r\n/>")
		if end < 0 {
			end = len(rest)
		}
		name := rest[:end]
		if j := bytes.LastIndexByte(name, ':'); j >= 0 {
			name = name[j+1:]
		}
		if bytes.EqualFold(name, []byte("item")) || bytes.EqualFold(name, []byte("entry")) {
			if entries++; entries > maxEntries {
				return true
			}
		}
	}
}

func post(it *gofeed.Item, base *url.URL) model.Post {
	p := model.Post{
		URL:       itemURL(it, base),
		Author:    author(it),
		UpdatedAt: postDate(it.UpdatedParsed, it.Updated),
	}
	p.PublishedAt = postDate(it.PublishedParsed, it.Published)
	if p.PublishedAt.IsZero() {
		p.PublishedAt = p.UpdatedAt
	}

	p.Title = truncate(titleText(it.Title), maxTitleRunes)
	if p.Title == "" {
		p.Title = truncate(titleText(mediaGroup(it.Extensions, "title")), maxTitleRunes)
	}
	for _, fallback := range []string{it.Description, mediaGroup(it.Extensions, "description"), it.Content} {
		if p.Title != "" {
			break
		}
		p.Title = truncate(plainText(fallback), maxExcerptRunes)
	}
	if p.Title == "" {
		p.Title = untitled
	}

	// YouTube's yt:video:ID entry ids are kept verbatim so they match what other readers stored
	p.GUID = strings.TrimSpace(it.GUID)
	if p.GUID == "" {
		p.GUID = p.URL
	}
	if p.GUID == "" {
		date := strings.TrimSpace(it.Published)
		if !p.PublishedAt.IsZero() {
			date = p.PublishedAt.Format(time.RFC3339Nano)
		}
		parts := []string{it.Title, date}
		if strings.TrimSpace(it.Title) == "" {
			// without a title the hash would collide for every undated item
			parts = append(parts, it.Description, it.Content)
		}
		p.GUID = hashGUID(parts...)
	}
	return p
}

func itemURL(it *gofeed.Item, base *url.URL) string {
	if u := resolve(base, it.Link); u != "" {
		return u
	}
	for _, l := range it.Links {
		if u := resolve(base, l); u != "" {
			return u
		}
	}
	if id := extValue(it.Extensions, "yt", "videoId"); id != "" {
		return "https://www.youtube.com/watch?v=" + url.QueryEscape(id)
	}
	for _, e := range it.Enclosures {
		if e != nil {
			if u := resolve(base, e.URL); u != "" {
				return u
			}
		}
	}
	return ""
}

func author(it *gofeed.Item) string {
	if name := personName(it.Authors, it.Author); name != "" {
		return name
	}
	if it.DublinCoreExt != nil {
		for _, c := range it.DublinCoreExt.Creator {
			if name := plainText(c); name != "" {
				return name
			}
		}
	}
	return ""
}

func personName(people []*gofeed.Person, fallback *gofeed.Person) string {
	for _, p := range slices.Concat(people, []*gofeed.Person{fallback}) {
		if p != nil {
			if name := plainText(p.Name); name != "" {
				return name
			}
		}
	}
	return ""
}

func extValue(e ext.Extensions, ns, name string) string {
	if vs := e[ns][name]; len(vs) > 0 {
		return strings.TrimSpace(vs[0].Value)
	}
	return ""
}

// mediaGroup reads a child of <media:group>, where YouTube keeps titles and descriptions.
func mediaGroup(e ext.Extensions, child string) string {
	groups := e["media"]["group"]
	if len(groups) == 0 {
		return ""
	}
	if vs := groups[0].Children[child]; len(vs) > 0 {
		return vs[0].Value
	}
	return ""
}

func hashGUID(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)[:16])
}

// unsignedZone matches an RFC 822 time whose numeric zone lost its sign ("00:00:00 0100").
var unsignedZone = regexp.MustCompile(`(\d{2}:\d{2}(?::\d{2})?)\s+(\d{4})$`)

var looseLayouts = []string{time.RFC1123Z, "Mon, 2 Jan 2006 15:04:05 -0700", "Mon, 02 Jan 2006 15:04 -0700", "2 Jan 2006 15:04:05 -0700"}

// postDate is the parsed date gofeed found, else raw repaired where it is still
// unambiguous; an undated post would otherwise be stored as brand new.
func postDate(parsed *time.Time, raw string) time.Time {
	if t := utc(parsed); !t.IsZero() {
		return t
	}
	raw = unsignedZone.ReplaceAllString(strings.TrimSpace(raw), "$1 +$2")
	for _, layout := range looseLayouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func utc(t *time.Time) time.Time {
	if t == nil || t.IsZero() {
		return time.Time{}
	}
	return t.UTC()
}

func parseBase(raw string) *url.URL {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !u.IsAbs() {
		return nil
	}
	return u
}

// resolve makes ref absolute against base and keeps only http(s) URLs, so a
// javascript: link in a feed can never reach an href in the UI.
func resolve(base *url.URL, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	u, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	if base != nil {
		u = base.ResolveReference(u)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return u.String()
}
