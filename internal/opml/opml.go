// Package opml reads and writes OPML, compatible with Fraidycat's export.
package opml

import (
	"cmp"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html/charset"

	"github.com/oryanm/stalker/internal/model"
)

// Entry is one followable outline.
type Entry struct {
	Title      string // explicit `title` attribute: a user-set title in Fraidycat exports
	Text       string // `text` attribute: the display title at export time
	FeedURL    string // `xmlUrl`, else `htmlUrl`
	SiteURL    string // `htmlUrl`, may be empty
	Importance model.Importance
	Tags       []string
	CreatedAt  time.Time // zero when absent or unparseable
}

// DefaultImportance is used when an outline has no importance/N category.
const DefaultImportance = model.Frequent

const (
	// maxDepth bounds outline nesting so a hostile file cannot grow the tag stack quadratically.
	maxDepth = 64
	// maxTags and maxTagRunes bound what every outline copies from its parent;
	// later and longer tags are ignored.
	maxTags     = 32
	maxTagRunes = 100
	// maxEntries bounds the follows one file can hold.
	maxEntries = 10000
)

// dateCreatedLayout is RFC 1123 with the GMT zone name RFC 822 requires (time.RFC1123 would print "UTC").
const dateCreatedLayout = "Mon, 02 Jan 2006 15:04:05 GMT"

// frame is the context an outline hands down to its children.
type frame struct {
	tags       []string
	importance model.Importance
}

// outline holds the attributes of one <outline> element.
type outline struct {
	text, title, xmlURL, htmlURL, category, created string
}

// Parse reads outlines at any depth. An outline without a URL is a folder whose
// text becomes a tag of its descendants. The comma-separated `category`
// attribute holds `importance/N` (normalized with model.NormalizeImportance,
// inherited by descendants like Fraidycat does) and tags; a leading "/" on a
// category is stripped. `created` accepts JavaScript Date.toString() output
// ("Wed Jun 24 2026 08:47:26 GMT-0400 (Eastern Daylight Time)"), RFC 1123/822
// and RFC 3339. Duplicate feed URLs keep the first entry.
//
// An outline has at most 32 tags of at most 100 characters (the rest are
// ignored), and a file with more than 10000 feeds is rejected.
func Parse(r io.Reader) ([]Entry, error) {
	d := xml.NewDecoder(r)
	// hand-made and legacy exports often contain bare ampersands and HTML entities
	d.Strict = false
	d.Entity = xml.HTMLEntity
	d.CharsetReader = charset.NewReaderLabel

	var (
		entries []Entry
		seen    = make(map[string]bool)
		stack   = []frame{{importance: DefaultImportance}}
		depth   int // element depth, 0 outside the root element
	)
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("opml: not an OPML document: no root element")
		}
		if err != nil {
			return nil, fmt.Errorf("opml: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth == 1 {
				if !strings.EqualFold(t.Name.Local, "opml") {
					return nil, fmt.Errorf("opml: not an OPML document: root element is <%s>", t.Name.Local)
				}
				continue
			}
			if !strings.EqualFold(t.Name.Local, "outline") {
				continue
			}
			if len(stack) > maxDepth {
				return nil, fmt.Errorf("opml: outlines nested deeper than %d levels", maxDepth)
			}
			f, e, ok := readOutline(t.Attr).resolve(stack[len(stack)-1])
			stack = append(stack, f)
			if ok && !seen[e.FeedURL] {
				if len(entries) == maxEntries {
					return nil, fmt.Errorf("opml: more than %d feeds", maxEntries)
				}
				seen[e.FeedURL] = true
				entries = append(entries, e)
			}
		case xml.EndElement:
			depth--
			if depth == 0 {
				// anything after the root element is ignored
				return entries, nil
			}
			// the non-strict decoder repairs mismatched end tags, so starts and ends stay balanced
			if strings.EqualFold(t.Name.Local, "outline") && len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
		}
	}
}

// readOutline reads the attributes Parse understands, matching names case-insensitively.
func readOutline(attrs []xml.Attr) outline {
	var o outline
	for _, a := range attrs {
		v := strings.TrimSpace(a.Value)
		if v == "" {
			continue
		}
		switch strings.ToLower(a.Name.Local) {
		case "text":
			o.text = v
		case "title":
			o.title = v
		case "xmlurl":
			o.xmlURL = v
		case "htmlurl":
			o.htmlURL = v
		case "category":
			o.category = v
		case "created":
			o.created = v
		}
	}
	return o
}

// resolve derives the context for the outline's children and, when the outline
// has a URL, the entry it describes.
func (o outline) resolve(parent frame) (frame, Entry, bool) {
	f := frame{tags: slices.Clone(parent.tags), importance: parent.importance}
	feedURL := cmp.Or(o.xmlURL, o.htmlURL)
	if feedURL == "" {
		f.tags = appendTag(f.tags, cmp.Or(o.text, o.title))
	}
	for c := range strings.SplitSeq(o.category, ",") {
		c = strings.TrimPrefix(strings.TrimSpace(c), "/")
		if imp, ok := parseImportance(c); ok {
			f.importance = imp
			continue
		}
		f.tags = appendTag(f.tags, c)
	}
	if feedURL == "" {
		return f, Entry{}, false
	}
	return f, Entry{
		Title:      o.title,
		Text:       o.text,
		FeedURL:    feedURL,
		SiteURL:    o.htmlURL,
		Importance: f.importance,
		Tags:       normalizeTags(f.tags),
		CreatedAt:  parseCreated(o.created),
	}, true
}

// appendTag appends tag with its whitespace collapsed, unless it is blank,
// already present, too long or over the maxTags limit. Only Unicode spaces
// are touched, so emoji ZWJ sequences survive byte for byte.
func appendTag(tags []string, tag string) []string {
	if len(tags) >= maxTags {
		return tags
	}
	tag = strings.Join(strings.Fields(tag), " ")
	if tag == "" || utf8.RuneCountInString(tag) > maxTagRunes || slices.Contains(tags, tag) {
		return tags
	}
	return append(tags, tag)
}

// normalizeTags returns the tags sorted and deduplicated, nil when there are none.
func normalizeTags(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	out := slices.Clone(tags)
	slices.Sort(out)
	return slices.Compact(out)
}

// parseImportance recognizes Fraidycat's "importance/N" category.
func parseImportance(c string) (model.Importance, bool) {
	prefix, digits, ok := strings.Cut(c, "/")
	if !ok || !strings.EqualFold(prefix, "importance") || !isDigits(digits) {
		return 0, false
	}
	// the only possible error is overflow, where Atoi saturates to MaxInt (Rarely)
	n, _ := strconv.Atoi(digits)
	return model.NormalizeImportance(n), true
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

// ToFollow converts an entry to a new follow: Title = e.Title (user override),
// FeedTitle = e.Text (replaced by the real feed title on the first fetch),
// URL = SiteURL, falling back to FeedURL. Zero CreatedAt becomes now; EditedAt
// = CreatedAt; NextFetchAt = now.
func (e Entry) ToFollow(now time.Time) model.Follow {
	created := e.CreatedAt
	if created.IsZero() {
		created = now
	}
	return model.Follow{
		URL:         cmp.Or(e.SiteURL, e.FeedURL),
		FeedURL:     e.FeedURL,
		Title:       e.Title,
		FeedTitle:   e.Text,
		Importance:  model.NormalizeImportance(int(e.Importance)),
		Tags:        slices.Clone(e.Tags),
		CreatedAt:   created,
		EditedAt:    created,
		NextFetchAt: now,
	}
}

type document struct {
	XMLName xml.Name `xml:"opml"`
	Version string   `xml:"version,attr"`
	Head    head     `xml:"head"`
	Body    body     `xml:"body"`
}

type head struct {
	Title       string `xml:"title"`
	DateCreated string `xml:"dateCreated"`
}

type body struct {
	Outlines []outlineElem `xml:"outline"`
}

// outlineElem is an exported outline, attributes in Fraidycat's order.
type outlineElem struct {
	Category string `xml:"category,attr"`
	Created  string `xml:"created,attr,omitempty"`
	Text     string `xml:"text,attr"`
	Title    string `xml:"title,attr,omitempty"`
	Type     string `xml:"type,attr"`
	XMLURL   string `xml:"xmlUrl,attr"`
	HTMLURL  string `xml:"htmlUrl,attr,omitempty"`
}

// Write emits OPML 2.0 in Fraidycat's format: one flat outline per follow with
// category="importance/N,tag1,tag2", created (RFC 1123Z), text (display title),
// title (only when the user set one), xmlUrl and htmlUrl.
func Write(w io.Writer, title string, follows []model.Follow) error {
	return write(w, title, follows, time.Now())
}

func write(w io.Writer, title string, follows []model.Follow, now time.Time) error {
	type sortable struct {
		key string
		f   model.Follow
	}
	sorted := make([]sortable, len(follows))
	for i, f := range follows {
		sorted[i] = sortable{strings.ToLower(f.DisplayTitle()), f}
	}
	slices.SortStableFunc(sorted, func(a, b sortable) int {
		return cmp.Or(strings.Compare(a.key, b.key), strings.Compare(a.f.FeedURL, b.f.FeedURL))
	})

	doc := document{
		Version: "2.0",
		Head:    head{Title: title, DateCreated: now.UTC().Format(dateCreatedLayout)},
		Body:    body{Outlines: make([]outlineElem, len(sorted))},
	}
	for i, s := range sorted {
		doc.Body.Outlines[i] = toOutline(s.f)
	}

	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return fmt.Errorf("opml: %w", err)
	}
	_, err := io.WriteString(w, "\n")
	return err
}

func toOutline(f model.Follow) outlineElem {
	category := []string{"importance/" + strconv.Itoa(int(model.NormalizeImportance(int(f.Importance))))}
	for _, t := range f.Tags {
		if t = strings.TrimSpace(t); t != "" {
			category = append(category, t)
		}
	}
	var created string
	t := f.CreatedAt
	if t.IsZero() {
		t = f.EditedAt
	}
	if !t.IsZero() {
		created = t.UTC().Format(time.RFC1123Z)
	}
	return outlineElem{
		Category: strings.Join(category, ","),
		Created:  created,
		Text:     f.DisplayTitle(),
		Title:    strings.TrimSpace(f.Title),
		// OPML 2.0 requires type on subscription outlines; Fraidycat ignores it
		Type:    "rss",
		XMLURL:  f.FeedURL,
		HTMLURL: f.URL,
	}
}
