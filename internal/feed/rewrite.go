package feed

import (
	"net/url"
	"strings"
)

// redirected reports whether final, as returned by Get, differs from the requested URL.
func redirected(requested, final string) bool {
	u, err := url.Parse(strings.TrimSpace(requested))
	if err != nil {
		return false
	}
	u.Host = strings.ToLower(u.Host)
	return final != "" && final != u.String()
}
