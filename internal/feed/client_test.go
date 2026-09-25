package feed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// newTestClient returns a Client without host spacing so tests stay fast,
// allowed to reach httptest servers on loopback.
func newTestClient(opts Options) *Client {
	if opts.HostSpacing == 0 {
		opts.HostSpacing = -1
	}
	opts.AllowPrivateNetworks = true
	return NewClient(opts)
}

// hostRouter sends every request to srv while keeping the original Host
// header, so tests can pretend to be youtube.com without touching the network.
type hostRouter struct {
	srv *httptest.Server
}

func (h hostRouter) RoundTrip(req *http.Request) (*http.Response, error) {
	target, _ := url.Parse(h.srv.URL)
	r := req.Clone(req.Context())
	r.Host = req.URL.Host
	r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
	resp, err := h.srv.Client().Transport.RoundTrip(r)
	if resp != nil {
		resp.Request = req
	}
	return resp, err
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFetchParsesFeed(t *testing.T) {
	body := readFixture(t, "rss2.xml")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Last-Modified", "Tue, 02 May 2023 13:30:00 GMT")
		w.Write(body)
	}))
	defer srv.Close()

	res, err := newTestClient(Options{}).Fetch(t.Context(), srv.URL+"/feed", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.NotModified || res.ETag != `"v1"` || res.LastModified != "Tue, 02 May 2023 13:30:00 GMT" ||
		res.FinalURL != srv.URL+"/feed" || res.Title != "xkcd" || len(res.Posts) != 3 {
		t.Errorf("unexpected result: %+v", res)
	}
}

func TestFetchConditional(t *testing.T) {
	const (
		etag    = `"abc"`
		lastMod = "Mon, 02 Jan 2006 15:04:05 GMT"
	)
	tests := []struct {
		name           string
		etag, lastMod  string
		respETag       string
		respLastMod    string
		wantETag       string
		wantLastMod    string
		wantConditonal bool
	}{
		{"validators kept when 304 omits them", etag, lastMod, "", "", etag, lastMod, true},
		{"new validators echoed", etag, lastMod, `"def"`, "Tue, 03 Jan 2006 15:04:05 GMT", `"def"`, "Tue, 03 Jan 2006 15:04:05 GMT", true},
		{"etag only", etag, "", "", "", etag, "", true},
		{"last-modified only", "", lastMod, "", "", "", lastMod, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("If-None-Match"); got != tt.etag {
					t.Errorf("If-None-Match = %q, want %q", got, tt.etag)
				}
				if got := r.Header.Get("If-Modified-Since"); got != tt.lastMod {
					t.Errorf("If-Modified-Since = %q, want %q", got, tt.lastMod)
				}
				if tt.respETag != "" {
					w.Header().Set("ETag", tt.respETag)
				}
				if tt.respLastMod != "" {
					w.Header().Set("Last-Modified", tt.respLastMod)
				}
				w.WriteHeader(http.StatusNotModified)
			}))
			defer srv.Close()

			res, err := newTestClient(Options{}).Fetch(t.Context(), srv.URL, tt.etag, tt.lastMod)
			if err != nil {
				t.Fatal(err)
			}
			if !res.NotModified || res.ETag != tt.wantETag || res.LastModified != tt.wantLastMod ||
				res.FinalURL != srv.URL || len(res.Posts) != 0 {
				t.Errorf("got %+v", res)
			}
		})
	}
}

func TestFetchNoConditionalHeadersWhenUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, h := range []string{"If-None-Match", "If-Modified-Since"} {
			if _, ok := r.Header[h]; ok {
				t.Errorf("unexpected %s header", h)
			}
		}
		w.Write(readFixture(t, "atom.xml"))
	}))
	defer srv.Close()
	if _, err := newTestClient(Options{}).Fetch(t.Context(), srv.URL, "", ""); err != nil {
		t.Fatal(err)
	}
}

func TestFetchHTTPError(t *testing.T) {
	future := time.Now().Add(2 * time.Hour).UTC().Format(http.TimeFormat)
	tests := []struct {
		name       string
		status     int
		retryAfter string
		wantMin    time.Duration
		wantMax    time.Duration
	}{
		{"not found", http.StatusNotFound, "", 0, 0},
		{"gone", http.StatusGone, "", 0, 0},
		{"rate limited with seconds", http.StatusTooManyRequests, "120", 120 * time.Second, 120 * time.Second},
		{"unavailable with date", http.StatusServiceUnavailable, future, 2*time.Hour - time.Minute, 2 * time.Hour},
		{"server error with junk", http.StatusInternalServerError, "soon", 0, 0},
		{"redirect without location", http.StatusMultipleChoices, "", 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.retryAfter != "" {
					w.Header().Set("Retry-After", tt.retryAfter)
				}
				w.WriteHeader(tt.status)
				io.WriteString(w, "<html>error page</html>")
			}))
			defer srv.Close()

			res, err := newTestClient(Options{}).Fetch(t.Context(), srv.URL, "", "")
			var he *HTTPError
			if !errors.As(err, &he) {
				t.Fatalf("err = %v, want *HTTPError", err)
			}
			if res != nil {
				t.Errorf("result = %+v, want nil", res)
			}
			if he.StatusCode != tt.status {
				t.Errorf("StatusCode = %d, want %d", he.StatusCode, tt.status)
			}
			if he.RetryAfter < tt.wantMin || he.RetryAfter > tt.wantMax {
				t.Errorf("RetryAfter = %v, want within [%v, %v]", he.RetryAfter, tt.wantMin, tt.wantMax)
			}
			if want := "HTTP " + strconv.Itoa(tt.status); err.Error() != want {
				t.Errorf("Error() = %q, want %q", err, want)
			}
		})
	}
}

func TestFetchNotAFeed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write(readFixture(t, "page.html"))
	}))
	defer srv.Close()
	if _, err := newTestClient(Options{}).Fetch(t.Context(), srv.URL, "", ""); !errors.Is(err, ErrNotAFeed) {
		t.Fatalf("err = %v, want ErrNotAFeed", err)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"0", 0},
		{"-5", 0},
		{"30", 30 * time.Second},
		{" 3600 ", time.Hour},
		{"99999999999999", maxRetryAfter},
		{"Thu, 24 Sep 2026 12:10:00 GMT", 10 * time.Minute},
		{"Thursday, 24-Sep-26 12:10:00 GMT", 10 * time.Minute},
		{"Thu Sep 24 12:10:00 2026", 10 * time.Minute},
		{"Thu, 24 Sep 2026 11:00:00 GMT", 0},
		{"Thu, 01 Oct 2026 12:00:00 GMT", maxRetryAfter},
		{"tomorrow", 0},
		{"1.5", 0},
	}
	for _, tt := range tests {
		if got := parseRetryAfter(tt.in, now); got != tt.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestGetRedirects(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/hop/{n}", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.PathValue("n"))
		if n == 0 {
			io.WriteString(w, "arrived")
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/hop/%d", n-1), http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tests := []struct {
		hops    int
		wantErr bool
	}{
		{0, false},
		{1, false},
		{maxRedirects, false},
		{maxRedirects + 1, true},
	}
	for _, tt := range tests {
		t.Run(strconv.Itoa(tt.hops), func(t *testing.T) {
			resp, err := newTestClient(Options{}).Get(t.Context(), fmt.Sprintf("%s/hop/%d", srv.URL, tt.hops), nil)
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "too many redirects") || !errors.Is(err, errTooManyRedirects) {
					t.Fatalf("err = %v, want too many redirects", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusOK || string(resp.Body) != "arrived" || resp.FinalURL != srv.URL+"/hop/0" {
				t.Errorf("got %d %q final %q", resp.StatusCode, resp.Body, resp.FinalURL)
			}
		})
	}
}

func TestGetBodyLimit(t *testing.T) {
	const limit = 1024
	tests := []struct {
		name    string
		size    int
		chunked bool
		wantErr bool
	}{
		{"under", limit - 1, false, false},
		{"exact", limit, false, false},
		{"exact chunked", limit, true, false},
		{"over by content length", limit + 1, false, true},
		{"over chunked", limit * 4, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body := strings.Repeat("x", tt.size)
				if !tt.chunked {
					w.Header().Set("Content-Length", strconv.Itoa(tt.size))
					io.WriteString(w, body)
					return
				}
				// flushing before the body is complete forces chunked encoding
				io.WriteString(w, body[:1])
				w.(http.Flusher).Flush()
				io.WriteString(w, body[1:])
			}))
			defer srv.Close()

			resp, err := newTestClient(Options{MaxBodyBytes: limit}).Get(t.Context(), srv.URL, nil)
			if tt.wantErr {
				if !errors.Is(err, ErrBodyTooLarge) {
					t.Fatalf("err = %v, want ErrBodyTooLarge", err)
				}
				if !strings.Contains(err.Error(), "1024 bytes") {
					t.Errorf("error %q does not mention the limit", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(resp.Body) != tt.size {
				t.Errorf("body length = %d, want %d", len(resp.Body), tt.size)
			}
		})
	}
}

func TestGetOversizedFeedFailsFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(readFixture(t, "rss2.xml"))
	}))
	defer srv.Close()
	_, err := newTestClient(Options{MaxBodyBytes: 100}).Fetch(t.Context(), srv.URL, "", "")
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("err = %v, want ErrBodyTooLarge", err)
	}
}

func TestGetHeaders(t *testing.T) {
	type seen struct {
		host, path, ua, accept, cookie, custom string
	}
	var (
		mu   sync.Mutex
		hits []seen
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, seen{r.Host, r.URL.Path, r.UserAgent(), r.Header.Get("Accept"), r.Header.Get("Cookie"), r.Header.Get("X-Custom")})
		mu.Unlock()
		if r.URL.Path == "/to-youtube" {
			http.Redirect(w, r, "https://www.youtube.com/landing", http.StatusMovedPermanently)
		}
	}))
	defer srv.Close()

	tests := []struct {
		name   string
		url    string
		opts   Options
		header http.Header
		want   []seen
	}{
		{
			name: "defaults",
			url:  "https://example.com/feed",
			want: []seen{{"example.com", "/feed", DefaultUserAgent, acceptHeader, "", ""}},
		},
		{
			name: "custom user agent and caller headers win",
			url:  "https://example.com/feed",
			opts: Options{UserAgent: "custom/1.0"},
			header: http.Header{
				"Accept":   {"text/plain"},
				"X-Custom": {"yes"},
			},
			want: []seen{{"example.com", "/feed", "custom/1.0", "text/plain", "", "yes"}},
		},
		{
			name: "youtube gets consent cookie",
			url:  "https://www.youtube.com/@someone",
			want: []seen{{"www.youtube.com", "/@someone", DefaultUserAgent, acceptHeader, youtubeConsent, ""}},
		},
		{
			name: "bare youtube host",
			url:  "https://YouTube.com/feeds/videos.xml",
			want: []seen{{"youtube.com", "/feeds/videos.xml", DefaultUserAgent, acceptHeader, youtubeConsent, ""}},
		},
		{
			name: "lookalike host gets no cookie",
			url:  "https://notyoutube.com/",
			want: []seen{{"notyoutube.com", "/", DefaultUserAgent, acceptHeader, "", ""}},
		},
		{
			name: "redirect onto youtube gets cookie",
			url:  "https://short.example/to-youtube",
			want: []seen{
				{"short.example", "/to-youtube", DefaultUserAgent, acceptHeader, "", ""},
				{"www.youtube.com", "/landing", DefaultUserAgent, acceptHeader, youtubeConsent, ""},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mu.Lock()
			hits = nil
			mu.Unlock()
			tt.opts.Transport = hostRouter{srv}
			if _, err := newTestClient(tt.opts).Get(t.Context(), tt.url, tt.header); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(hits) != len(tt.want) {
				t.Fatalf("hits = %+v, want %+v", hits, tt.want)
			}
			for i := range hits {
				if hits[i] != tt.want[i] {
					t.Errorf("hit %d = %+v, want %+v", i, hits[i], tt.want[i])
				}
			}
		})
	}
}

func TestGetInvalidURL(t *testing.T) {
	c := newTestClient(Options{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("transport must not be reached")
		return nil, errors.New("unreachable")
	})})
	for _, raw := range []string{"", "example.com/feed", "ftp://example.com/", "file:///etc/passwd", "https://", "http://[::1"} {
		if _, err := c.Get(t.Context(), raw, nil); err == nil || !strings.Contains(err.Error(), "invalid URL") {
			t.Errorf("Get(%q) err = %v, want invalid URL", raw, err)
		}
	}
}

func TestGetTransportErrors(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	reset := &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}
	tests := []struct {
		name    string
		err     error
		wantMsg string
		target  error
	}{
		{"dns not found", &net.DNSError{Err: "no such host", Name: "nope.example", IsNotFound: true}, "host not found: nope.example", nil},
		{"dns failure", &net.DNSError{Err: "server misbehaving", Name: "flaky.example"}, "DNS lookup failed: flaky.example", nil},
		{"dns timeout", &net.DNSError{Err: "timeout", Name: "slow.example", IsTimeout: true}, "DNS lookup timed out: slow.example", nil},
		{"refused", refused, "connection refused by example.com", syscall.ECONNREFUSED},
		{"reset", reset, "connection reset by example.com", syscall.ECONNRESET},
		{"eof", io.ErrUnexpectedEOF, "connection closed unexpectedly by example.com", io.ErrUnexpectedEOF},
		{"other", errors.New("boom"), "request failed: boom", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestClient(Options{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, tt.err
			})})
			_, err := c.Get(t.Context(), "https://example.com/feed", nil)
			if err == nil || err.Error() != tt.wantMsg {
				t.Fatalf("err = %v, want %q", err, tt.wantMsg)
			}
			if !errors.Is(err, tt.err) {
				t.Errorf("errors.Is(err, original) = false")
			}
			if tt.target != nil && !errors.Is(err, tt.target) {
				t.Errorf("errors.Is(err, %v) = false", tt.target)
			}
			var urlErr *url.Error
			if !errors.As(err, &urlErr) {
				t.Errorf("errors.As(err, *url.Error) = false")
			}
			var dnsErr *net.DNSError
			if _, isDNS := tt.err.(*net.DNSError); isDNS && !errors.As(err, &dnsErr) {
				t.Errorf("errors.As(err, *net.DNSError) = false")
			}
		})
	}
}

func TestGetConnectionRefused(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()

	_, err = newTestClient(Options{}).Get(t.Context(), "http://"+addr+"/", nil)
	if !errors.Is(err, syscall.ECONNREFUSED) || err.Error() != "connection refused by 127.0.0.1" {
		t.Fatalf("err = %v", err)
	}
}

func TestGetTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	_, err := newTestClient(Options{Timeout: 50 * time.Millisecond}).Get(t.Context(), srv.URL, nil)
	var ne net.Error
	if err == nil || err.Error() != "request timed out" || !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("err = %v, want timeout", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("errors.Is(err, context.DeadlineExceeded) = false")
	}
}

func TestGetBodyReadTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "<rss")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	_, err := newTestClient(Options{Timeout: 100 * time.Millisecond}).Get(t.Context(), srv.URL, nil)
	if err == nil || err.Error() != "request timed out" {
		t.Fatalf("err = %v, want request timed out", err)
	}
}

func TestGetContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(20*time.Millisecond, cancel)
	_, err := newTestClient(Options{}).Get(ctx, srv.URL, nil)
	if !errors.Is(err, context.Canceled) || err.Error() != "request cancelled" {
		t.Fatalf("err = %v, want cancelled", err)
	}
}

func TestNewClientDefaults(t *testing.T) {
	c := NewClient(Options{})
	if c.userAgent != DefaultUserAgent || c.maxBody != defaultMaxBodyBytes || c.hc.Timeout != defaultTimeout ||
		c.throttle.parallel != defaultHostParallel || c.throttle.spacing != defaultHostSpacing {
		t.Errorf("unexpected defaults: ua=%q max=%d timeout=%v parallel=%d spacing=%v",
			c.userAgent, c.maxBody, c.hc.Timeout, c.throttle.parallel, c.throttle.spacing)
	}
	if _, ok := c.hc.Transport.(*http.Transport); !ok || c.hc.Transport == http.DefaultTransport {
		t.Errorf("transport should be a private clone of http.DefaultTransport, got %T", c.hc.Transport)
	}
	if c := NewClient(Options{HostSpacing: -1}); c.throttle.spacing != 0 {
		t.Errorf("negative spacing = %v, want disabled", c.throttle.spacing)
	}
}

func TestDecodeCharset(t *testing.T) {
	// "שלום עולם" in windows-1255 and "Café" in ISO-8859-1
	hebrew := "\xf9\xec\xe5\xed \xf2\xe5\xec\xed"
	tests := []struct {
		name, body, contentType, want string
	}{
		{"header charset", `<?xml version="1.0"?><t>` + hebrew + `</t>`, "application/rss+xml; charset=windows-1255",
			`<?xml version="1.0"?><t>שלום עולם</t>`},
		{"label is case-insensitive", "<t>Caf\xe9</t>", `text/xml; Charset="ISO-8859-1"`, "<t>Café</t>"},
		{"declared encoding wins", `<?xml version="1.0" encoding="windows-1255"?><t>` + hebrew + `</t>`,
			"text/xml; charset=iso-8859-1", `<?xml version="1.0" encoding="windows-1255"?><t>` + hebrew + `</t>`},
		{"valid UTF-8 is kept", "<t>Café</t>", "text/xml; charset=iso-8859-1", "<t>Café</t>"},
		{"no charset", "<t>Caf\xe9</t>", "text/xml", "<t>Caf\xe9</t>"},
		{"unknown charset", "<t>Caf\xe9</t>", "text/xml; charset=klingon", "<t>Caf\xe9</t>"},
		{"malformed header", "<t>Caf\xe9</t>", "text/xml; charset", "<t>Caf\xe9</t>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(decodeCharset([]byte(tt.body), tt.contentType)); got != tt.want {
				t.Errorf("decodeCharset = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFetchUsesHeaderCharset(t *testing.T) {
	body := "<?xml version=\"1.0\"?><rss version=\"2.0\"><channel><title>\xe7\xe3\xf9\xe5\xfa</title>" +
		"<item><title>\xf9\xec\xe5\xed \xf2\xe5\xec\xed</title><guid>1</guid></item></channel></rss>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml; charset=windows-1255")
		io.WriteString(w, body)
	}))
	defer srv.Close()

	res, err := newTestClient(Options{}).Fetch(t.Context(), srv.URL, "", "")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.Title != "חדשות" || len(res.Posts) != 1 || res.Posts[0].Title != "שלום עולם" {
		t.Errorf("got title %q, posts %+v", res.Title, res.Posts)
	}
	if res.FinalURL != srv.URL {
		t.Errorf("FinalURL = %q, want %q", res.FinalURL, srv.URL)
	}
}
