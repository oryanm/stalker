package feed

import (
	"bytes"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
)

// blockTags separate words when markup is flattened to text.
var blockTags = map[string]bool{
	"address": true, "article": true, "aside": true, "blockquote": true, "br": true, "dd": true, "div": true,
	"dl": true, "dt": true, "figcaption": true, "figure": true, "footer": true, "h1": true, "h2": true,
	"h3": true, "h4": true, "h5": true, "h6": true, "header": true, "hr": true, "img": true, "li": true,
	"main": true, "nav": true, "ol": true, "p": true, "pre": true, "section": true, "table": true,
	"td": true, "th": true, "tr": true, "ul": true,
}

// titleTags are the elements whose tags titleText removes: the markup feeds
// actually put in titles. script and style also lose their content.
var titleTags = map[string]bool{
	"a": true, "abbr": true, "b": true, "big": true, "br": true, "cite": true, "code": true, "del": true,
	"dfn": true, "div": true, "em": true, "font": true, "i": true, "img": true, "ins": true, "kbd": true,
	"mark": true, "p": true, "q": true, "s": true, "samp": true, "script": true, "small": true, "span": true,
	"strike": true, "strong": true, "style": true, "sub": true, "sup": true, "tt": true, "u": true, "wbr": true,
}

// plainText strips HTML tags, decodes entities and collapses whitespace.
// Feeds routinely put markup (or double-escaped entities) in text fields.
func plainText(s string) string { return flatten(s, false) }

// titleText is plainText for titles, which are text by definition (Atom
// type="text", RSS) yet often carry markup anyway. Only tags of titleTags
// written in lowercase, or in uppercase with two or more letters, count as
// markup; anything else in angle brackets is kept, so "List<String>",
// "Optional<T>" or "Map<K, V>" survive. A script or style start tag without
// its end tag is kept as text too.
func titleText(s string) string { return flatten(s, true) }

func flatten(s string, titles bool) string {
	if !strings.ContainsAny(s, "<&") {
		return collapseSpace(s)
	}
	var b bytes.Buffer
	z := html.NewTokenizer(strings.NewReader(s))
	var hidden string // the script or style element being skipped
	hiddenAt := 0     // b.Len() where it began, for titles, which keep it until its end tag
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		// copied first: TagName lowercases the token in place
		raw := string(z.Raw())
		switch tt {
		case html.TextToken:
			if hidden == "" || titles {
				b.Write(z.Text())
			}
		case html.StartTagToken, html.EndTagToken, html.SelfClosingTagToken:
			nameBytes, _ := z.TagName()
			name := string(nameBytes)
			if titles && !(titleTags[name] && htmlCase(raw, name)) {
				b.WriteString(raw)
				continue
			}
			switch {
			case name == "script" || name == "style":
				switch {
				case tt == html.StartTagToken && hidden == "":
					hidden, hiddenAt = name, b.Len()
					if titles {
						b.WriteString(raw)
					}
				case tt == html.EndTagToken && name == hidden:
					b.Truncate(hiddenAt)
					hidden = ""
				}
			case blockTags[name] && (hidden == "" || titles):
				b.WriteByte(' ')
			}
		}
	}
	return collapseSpace(b.String())
}

// htmlCase reports whether the tag name in raw, whose lowercase form is name,
// is written the way HTML is: lowercase, or uppercase with two or more letters.
// Type names such as <Object>, <T> or <B> are not.
func htmlCase(raw, name string) bool {
	rawName := strings.TrimPrefix(strings.TrimPrefix(raw, "<"), "/")
	if len(rawName) < len(name) {
		return false
	}
	rawName = rawName[:len(name)]
	return rawName == name || (len(name) > 1 && rawName == strings.ToUpper(name))
}

func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncate shortens s to at most n runes plus an ellipsis, preferring a word boundary.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	cut := s
	for i := range s {
		if n == 0 {
			cut = s[:i]
			break
		}
		n--
	}
	if i := strings.LastIndexByte(cut, ' '); i > len(cut)/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,.;:-") + "…"
}
