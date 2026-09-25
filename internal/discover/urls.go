package discover

import (
	"fmt"
	"net/url"
	"strings"
)

// normalizeInput parses what the user typed into an absolute http(s) URL.
// schemeGiven reports whether the user chose the scheme themselves.
func normalizeInput(input string) (u *url.URL, schemeGiven bool, err error) {
	s := strings.TrimSpace(input)
	lower := strings.ToLower(s)
	// feed: is a pseudo-scheme browsers and podcast apps use for subscribe links
	switch {
	case strings.HasPrefix(lower, "feed://"):
		s = "https://" + s[len("feed://"):]
	case strings.HasPrefix(lower, "feed:"):
		s = s[len("feed:"):]
	}
	schemeGiven = true
	switch {
	case strings.HasPrefix(s, "//"):
		s, schemeGiven = "https:"+s, false
	case !strings.Contains(s, "://"):
		// "host:8080/x" carries a port, while "mailto:x" names a scheme that is not the web
		if head, rest, ok := strings.Cut(s, ":"); ok && !strings.Contains(head, "/") &&
			(rest == "" || rest[0] < '0' || rest[0] > '9') {
			return nil, false, fmt.Errorf("%w: %q", ErrInvalidURL, input)
		}
		s, schemeGiven = "https://"+s, false
	}

	u, err = url.Parse(s)
	if err != nil || u.Hostname() == "" || strings.ContainsAny(u.Host, " \t") {
		return nil, false, fmt.Errorf("%w: %q", ErrInvalidURL, input)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, false, fmt.Errorf("%w: %q", ErrInvalidURL, input)
	}
	u.Host = strings.ToLower(u.Host)
	u.User = nil
	u.Fragment, u.RawFragment = "", ""
	if u.Path == "" {
		u.Path = "/"
	}
	return u, schemeGiven, nil
}

// urlKey identifies a URL for deduplication: scheme, www., default ports,
// trailing slashes and fragments do not make two feeds different.
func urlKey(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	key := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	if port := u.Port(); port != "" && port != "80" && port != "443" {
		key += ":" + port
	}
	key += strings.TrimRight(u.EscapedPath(), "/")
	if u.RawQuery != "" {
		key += "?" + u.RawQuery
	}
	return key
}

// sameSite reports whether two URLs are on the same host, ignoring www.
func sameSite(a, b string) bool {
	ha, hb := hostOf(a), hostOf(b)
	return ha != "" && ha == hb
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
}

// siteHost is the host used to match site rules.
func siteHost(host string) string {
	host = strings.ToLower(host)
	for _, prefix := range []string{"www.", "m.", "old."} {
		if strings.HasPrefix(host, prefix) {
			return host[len(prefix):]
		}
	}
	return host
}

func pathSegments(p string) []string {
	var segs []string
	for seg := range strings.SplitSeq(p, "/") {
		if seg != "" {
			segs = append(segs, seg)
		}
	}
	return segs
}
