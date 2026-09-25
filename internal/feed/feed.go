// Package feed fetches and parses RSS, Atom and JSON Feed documents.
package feed

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html/charset"

	"github.com/oryanm/stalker/internal/feedurl"
	"github.com/oryanm/stalker/internal/model"
)

// DefaultUserAgent identifies stalker to the sites it polls.
const DefaultUserAgent = "stalker/0.1 (+https://github.com/oryanm/stalker; personal feed reader)"

const (
	defaultTimeout      = 30 * time.Second
	defaultMaxBodyBytes = 10 << 20
	defaultHostSpacing  = 500 * time.Millisecond
	defaultHostParallel = 2
	maxRedirects        = 5

	acceptHeader = "application/rss+xml, application/atom+xml, application/feed+json, application/json;q=0.9, " +
		"application/xml;q=0.8, text/xml;q=0.8, text/html;q=0.7, */*;q=0.5"

	// youtubeConsent pre-accepts the cookie banner YouTube shows EU and datacenter IPs instead of the page.
	youtubeConsent = "SOCS=CAI; CONSENT=YES+"

	// maxRetryAfter bounds absurd server hints; a day is also the poller's longest backoff.
	maxRetryAfter = 24 * time.Hour
)

// Client is the only HTTP client stalker uses for outbound requests (fetching
// and discovery). It sets a User-Agent, bounds body size, time and redirects,
// and throttles per host: at most 2 concurrent requests and 500ms spacing per
// host, so 80+ YouTube feeds are not fetched in a burst. Safe for concurrent use.
type Client struct {
	hc        *http.Client
	userAgent string
	maxBody   int64
	throttle  *throttle
}

// Options configures a Client. Zero values pick defaults.
type Options struct {
	UserAgent    string            // default "stalker/0.1 (+https://github.com/oryanm/stalker; personal feed reader)"
	Timeout      time.Duration     // default 30s per request
	MaxBodyBytes int64             // default 10 MiB
	HostSpacing  time.Duration     // default 500ms, negative disables
	HostParallel int               // default 2
	Transport    http.RoundTripper // tests inject httptest transports

	// AllowPrivateNetworks lets the default transport connect to loopback,
	// private, link-local and other non-public addresses, which it otherwise
	// refuses (after DNS resolution, on every redirect) so that feeds and
	// pasted URLs cannot reach the server's own network or cloud metadata.
	// An injected Transport is used as is.
	AllowPrivateNetworks bool
}

// NewClient returns a Client configured by opts.
func NewClient(opts Options) *Client {
	if opts.UserAgent == "" {
		opts.UserAgent = DefaultUserAgent
	}
	if opts.Timeout <= 0 {
		opts.Timeout = defaultTimeout
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = defaultMaxBodyBytes
	}
	switch {
	case opts.HostSpacing == 0:
		opts.HostSpacing = defaultHostSpacing
	case opts.HostSpacing < 0:
		opts.HostSpacing = 0
	}
	if opts.HostParallel <= 0 {
		opts.HostParallel = defaultHostParallel
	}
	rt := opts.Transport
	if rt == nil {
		rt = http.DefaultTransport
		if t, ok := rt.(*http.Transport); ok {
			t = t.Clone()
			if !opts.AllowPrivateNetworks {
				// the same dialer settings as http.DefaultTransport, plus the address check
				d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Control: refusePrivate}
				t.DialContext = d.DialContext
			}
			rt = t
		}
	}
	return &Client{
		hc: &http.Client{
			Transport: rt,
			Timeout:   opts.Timeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) > maxRedirects {
					return errTooManyRedirects
				}
				addSiteHeaders(req)
				return nil
			},
		},
		userAgent: opts.UserAgent,
		maxBody:   opts.MaxBodyBytes,
		throttle:  newThrottle(opts.HostParallel, opts.HostSpacing),
	}
}

// Response is a bounded, fully read GET response.
type Response struct {
	StatusCode int
	Header     http.Header
	FinalURL   string // after redirects
	Body       []byte
}

// Get performs a throttled GET with extra headers (may be nil) and reads the
// whole body up to MaxBodyBytes. Non-2xx responses are returned, not errors.
func (c *Client) Get(ctx context.Context, rawURL string, header http.Header) (*Response, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, fmt.Errorf("invalid URL %q", rawURL)
	}
	u.Host = strings.ToLower(u.Host)
	host := u.Hostname()

	release, err := c.throttle.acquire(ctx, host)
	if err != nil {
		return nil, describe(err, host)
	}
	defer release()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("invalid URL %q: %w", rawURL, err)
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", acceptHeader)
	for k, vs := range header {
		req.Header.Del(k)
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	addSiteHeaders(req)

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, describe(err, host)
	}
	defer resp.Body.Close()

	finalURL := u.String()
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
		host = strings.ToLower(resp.Request.URL.Hostname())
	}
	if resp.ContentLength > c.maxBody {
		return nil, c.tooLarge()
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBody+1))
	if err != nil {
		return nil, describe(err, host)
	}
	if int64(len(body)) > c.maxBody {
		return nil, c.tooLarge()
	}
	return &Response{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		FinalURL:   finalURL,
		Body:       body,
	}, nil
}

func (c *Client) tooLarge() error {
	limit := fmt.Sprintf("%d bytes", c.maxBody)
	if c.maxBody%(1<<20) == 0 {
		limit = fmt.Sprintf("%d MiB", c.maxBody>>20)
	}
	return fmt.Errorf("%w (limit %s)", ErrBodyTooLarge, limit)
}

// addSiteHeaders adds per-site headers; it also runs on every redirect hop.
func addSiteHeaders(req *http.Request) {
	if feedurl.IsYouTubeHost(req.URL.Hostname()) && req.Header.Get("Cookie") == "" {
		req.Header.Set("Cookie", youtubeConsent)
	}
}

// Result is a parsed feed.
type Result struct {
	NotModified  bool
	ETag         string
	LastModified string
	FinalURL     string
	Title        string
	Description  string
	SiteURL      string       // absolute
	ImageURL     string       // absolute
	Posts        []model.Post // newest first, at most 200; FollowID, ID and FirstSeenAt unset
}

// RedirectError reports a failure at the URL redirects led to, such as a login
// page served instead of the requested feed.
type RedirectError struct {
	URL string // final URL after redirects
	Err error
}

func (e *RedirectError) Error() string { return fmt.Sprintf("%v (redirected to %s)", e.Err, e.URL) }
func (e *RedirectError) Unwrap() error { return e.Err }

// HTTPError is returned by Fetch for non-2xx, non-304 responses.
type HTTPError struct {
	StatusCode int
	RetryAfter time.Duration // parsed Retry-After, zero when absent
}

func (e *HTTPError) Error() string { return fmt.Sprintf("HTTP %d", e.StatusCode) }

// Fetch GETs feedURL as a conditional request (If-None-Match /
// If-Modified-Since from etag and lastModified) and parses it. A 304 yields
// Result{NotModified: true}. Bodies that are not a feed yield ErrNotAFeed,
// wrapped in *RedirectError when redirects led to another URL. YouTube channel
// and playlist pages are fetched as their feeds, and old.reddit.com feeds from
// www.reddit.com.
func (c *Client) Fetch(ctx context.Context, feedURL, etag, lastModified string) (*Result, error) {
	header := http.Header{}
	if etag != "" {
		header.Set("If-None-Match", etag)
	}
	if lastModified != "" {
		header.Set("If-Modified-Since", lastModified)
	}
	target := feedurl.Canonical(feedURL)
	resp, err := c.Get(ctx, target, header)
	if err != nil {
		return nil, err
	}

	switch {
	case resp.StatusCode == http.StatusNotModified:
		return &Result{
			NotModified:  true,
			ETag:         firstNonEmpty(resp.Header.Get("ETag"), etag),
			LastModified: firstNonEmpty(resp.Header.Get("Last-Modified"), lastModified),
			FinalURL:     resp.FinalURL,
		}, nil
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, &HTTPError{
			StatusCode: resp.StatusCode,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
		}
	}

	res, err := ParseResponse(resp)
	if err != nil {
		if redirected(target, resp.FinalURL) {
			return nil, &RedirectError{URL: resp.FinalURL, Err: err}
		}
		return nil, err
	}
	res.ETag = resp.Header.Get("ETag")
	res.LastModified = resp.Header.Get("Last-Modified")
	return res, nil
}

// ParseResponse parses a response body as a feed, like Parse with the final
// URL as base, and sets Result.FinalURL. A body that is not UTF-8 and names no
// encoding in its XML declaration is first decoded with the charset of the
// Content-Type header, as RFC 7303 prescribes.
func ParseResponse(resp *Response) (*Result, error) {
	res, err := Parse(decodeCharset(resp.Body, resp.Header.Get("Content-Type")), resp.FinalURL)
	if err != nil {
		return nil, err
	}
	res.FinalURL = resp.FinalURL
	return res, nil
}

// xmlEncoding matches an XML declaration naming an encoding, which the parser honours itself.
var xmlEncoding = regexp.MustCompile(`^\s*<\?xml\s[^>]*\bencoding\s*=`)

// decodeCharset converts body to UTF-8 using the charset parameter of
// contentType, unless body is valid UTF-8 already or declares its encoding.
func decodeCharset(body []byte, contentType string) []byte {
	head := bytes.TrimPrefix(body[:min(len(body), 512)], utf8BOM)
	if utf8.Valid(body) || xmlEncoding.Match(head) {
		return body
	}
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return body
	}
	enc, name := charset.Lookup(params["charset"])
	if enc == nil || name == "utf-8" {
		return body
	}
	out, err := enc.NewDecoder().Bytes(body)
	if err != nil {
		return body
	}
	return out
}

// firstNonEmpty returns a unless it is empty.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// parseRetryAfter accepts both forms from RFC 9110: delay seconds and an HTTP-date.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		switch {
		case secs <= 0:
			return 0
		case secs >= int64(maxRetryAfter/time.Second):
			return maxRetryAfter
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return min(max(t.Sub(now), 0), maxRetryAfter)
	}
	return 0
}
