// Package discover turns a URL the user pasted into followable feed URLs.
package discover

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/PuerkitoBio/goquery"

	"github.com/oryanm/stalker/internal/feed"
)

// ErrNoFeed means nothing followable was found at the URL.
var ErrNoFeed = errors.New("no RSS, Atom or JSON feed found at this URL")

// ErrInvalidURL means the input could not be understood as an http(s) URL.
var ErrInvalidURL = errors.New("invalid URL")

const (
	// maxProbes caps candidate fetches per Discover call (the input page itself is not counted).
	maxProbes = 12
	// verifyParallel bounds concurrent candidate fetches; feed.Client throttles per host on top.
	verifyParallel = 4
	// maxCandidates is enough choices for the chooser; later link candidates are not fetched.
	maxCandidates = 5
	maxAnchors    = 5
)

// Candidate is a verified feed: it was fetched and parsed successfully.
type Candidate struct {
	FeedURL string
	Title   string // feed title, may be empty
	SiteURL string // home page of the feed, falls back to the input URL
}

// Discoverer finds feeds for pasted URLs. Safe for concurrent use.
type Discoverer struct {
	client *feed.Client
}

// New returns a Discoverer fetching through c (a default client when nil).
func New(c *feed.Client) *Discoverer {
	if c == nil {
		c = feed.NewClient(feed.Options{})
	}
	return &Discoverer{client: c}
}

// Discover normalizes input (adds https:// when the scheme is missing), applies
// site-specific rules (YouTube /channel/, /@handle, /c/, /user/, playlists;
// Reddit subreddits and users; GitHub users/repos; Bluesky profiles; Medium;
// Substack), treats the URL itself as a feed when it parses as one, then reads
// <link rel="alternate"> feeds from HTML, then <a> links mentioning rss/feed,
// then probes common paths (/feed, /rss, /rss.xml, /atom.xml, /feed.xml,
// /index.xml, /feed.json). Every returned candidate is verified, deduplicated
// by feed URL and ordered by confidence. Returns ErrNoFeed when none verify.
func (d *Discoverer) Discover(ctx context.Context, input string) ([]Candidate, error) {
	u, schemeGiven, err := normalizeInput(input)
	if err != nil {
		return nil, err
	}
	s := &session{
		client:   d.client,
		input:    u.String(),
		fetched:  map[string]bool{},
		accepted: map[string]bool{},
		budget:   maxProbes,
	}

	feeds, page := siteRule(u)
	if len(feeds) > 0 {
		if found := s.verify(ctx, feeds, len(feeds)); len(found) > 0 {
			return found, nil
		}
	}
	if page == "" {
		page = u.String()
	}
	found, err := s.generic(ctx, page, !schemeGiven)
	switch {
	case len(found) > 0:
		return found, nil
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case err != nil:
		return nil, err
	}
	return nil, ErrNoFeed
}

// session is the state of one Discover call.
type session struct {
	client   *feed.Client
	input    string          // normalized input, the SiteURL fallback
	fetched  map[string]bool // urlKey of everything already fetched or queued
	accepted map[string]bool // urlKey and content keys of returned candidates
	budget   int             // candidate fetches left
}

// generic discovers feeds from the page itself: the page as a feed, its
// <link> and <a> elements, then well-known paths.
func (s *session) generic(ctx context.Context, page string, tryHTTP bool) ([]Candidate, error) {
	s.fetched[urlKey(page)] = true
	resp, err := s.client.Get(ctx, page, nil)
	if err != nil && tryHTTP && ctx.Err() == nil && !isDNSError(err) && strings.HasPrefix(page, "https://") {
		// the user typed no scheme, and small sites still exist that only speak plain HTTP
		insecure := "http://" + strings.TrimPrefix(page, "https://")
		if r, httpErr := s.client.Get(ctx, insecure, nil); httpErr == nil {
			if s.input == page {
				s.input = insecure
			}
			resp, err, page = r, nil, insecure
		}
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNoFeed, err)
	}
	s.fetched[urlKey(resp.FinalURL)] = true

	var pageErr error
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// a mistyped YouTube handle or a dead page should say so rather than just "no feed"
		status := strings.TrimSpace(fmt.Sprintf("HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode)))
		pageErr = fmt.Errorf("%w: the page returned %s", ErrNoFeed, status)
	} else {
		if res, err := feed.ParseResponse(resp); err == nil {
			if c, ok := s.accept(page, res); ok {
				return []Candidate{c}, nil
			}
		}
		if isHTML(resp) {
			if found := s.fromHTML(ctx, resp); len(found) > 0 {
				return found, nil
			}
		}
	}
	// YouTube only has channel and playlist feeds, and its /feed/... pages would waste every guess
	if isYouTube(resp.FinalURL) {
		return nil, pageErr
	}
	if found := s.verify(ctx, probeURLs(resp.FinalURL), 1); len(found) > 0 {
		return found, nil
	}
	return nil, pageErr
}

func (s *session) fromHTML(ctx context.Context, resp *feed.Response) []Candidate {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(resp.Body))
	if err != nil {
		return nil
	}
	base := documentBase(doc, resp.FinalURL)

	var links []string
	youtube := isYouTube(resp.FinalURL)
	if youtube {
		if id := youtubeChannelID(doc, resp.Body); id != "" {
			links = append(links, youtubeChannelFeed(id))
		}
	}
	primary, comments := alternateLinks(doc, base)
	stages := [][]string{append(links, primary...), comments}
	if !youtube {
		stages = append(stages, feedAnchors(doc, base, resp.FinalURL))
	}
	for _, stage := range stages {
		if found := s.verify(ctx, stage, maxCandidates); len(found) > 0 {
			return found
		}
	}
	return nil
}

// verify fetches and parses urls concurrently and returns the ones that are
// feeds, in input order. It stops fetching once the first want entries that
// succeed are known, so lower-priority guesses are not fetched needlessly.
func (s *session) verify(ctx context.Context, urls []string, want int) []Candidate {
	var todo []string
	for _, raw := range urls {
		if s.budget <= 0 {
			break
		}
		k := urlKey(raw)
		if k == "" || s.fetched[k] {
			continue
		}
		s.fetched[k] = true
		s.budget--
		todo = append(todo, raw)
	}
	if len(todo) == 0 {
		return nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		results = make([]*feed.Result, len(todo))
		done    = make([]bool, len(todo))
		sem     = make(chan struct{}, verifyParallel)
	)
launch:
	for i, raw := range todo {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break launch
		}
		wg.Go(func() {
			defer func() { <-sem }()
			res, err := s.client.Fetch(ctx, raw, "", "")
			mu.Lock()
			defer mu.Unlock()
			done[i] = true
			if err == nil && !res.NotModified {
				results[i] = res
			}
			if settled(done, results) >= want {
				cancel()
			}
		})
	}
	wg.Wait()

	var found []Candidate
	for i, res := range results {
		if res == nil {
			continue
		}
		if c, ok := s.accept(todo[i], res); ok {
			found = append(found, c)
			if len(found) == want {
				break
			}
		}
	}
	return found
}

// settled counts successes in the leading run of finished fetches.
func settled(done []bool, results []*feed.Result) int {
	n := 0
	for i := range done {
		if !done[i] {
			break
		}
		if results[i] != nil {
			n++
		}
	}
	return n
}

// accept turns a verified feed into a Candidate unless an equivalent one was
// already accepted (same URL after redirects, or the same posts in another format).
func (s *session) accept(requested string, res *feed.Result) (Candidate, bool) {
	feedURL := requested
	// same-site redirects (https, www, trailing slash) are canonical; cross-site ones may be signed or temporary
	if res.FinalURL != "" && sameSite(requested, res.FinalURL) {
		feedURL = res.FinalURL
	}
	keys := []string{urlKey(requested), urlKey(res.FinalURL), urlKey(feedURL), contentKey(res)}
	for _, k := range keys {
		if k != "" && s.accepted[k] {
			return Candidate{}, false
		}
	}
	for _, k := range keys {
		if k != "" {
			s.accepted[k] = true
		}
	}
	site := res.SiteURL
	if site == "" {
		site = s.input
	}
	return Candidate{FeedURL: feedURL, Title: res.Title, SiteURL: site}, true
}

// contentKey identifies a feed by its title and posts, so the RSS and Atom
// renditions of one blog are offered once.
func contentKey(res *feed.Result) string {
	if len(res.Posts) == 0 {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(res.Title))
	for _, p := range res.Posts {
		h.Write([]byte{0})
		if p.URL != "" {
			h.Write([]byte(p.URL))
		} else {
			h.Write([]byte(p.GUID))
		}
	}
	return "content:" + hex.EncodeToString(h.Sum(nil))
}

func isHTML(resp *feed.Response) bool {
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "html") {
		return true
	}
	return strings.HasPrefix(http.DetectContentType(resp.Body), "text/html")
}

func isDNSError(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr)
}
