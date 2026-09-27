package discover

import (
	"errors"
	"net"
	"net/http"
	"testing"

	"github.com/oryanm/stalker/internal/feed"
)

func TestIcon(t *testing.T) {
	ico := page{ctype: "image/x-icon", body: "\x00\x00\x01\x00\x01\x00"}
	tests := []struct {
		name  string
		pages map[string]page
		page  string
		want  string
	}{
		{
			name: "only icon, relative to the base",
			pages: map[string]page{"blog.example/posts/": htmlPage(
				`<base href="/assets/"><link href="logo.png" rel="icon" type="image/png">`, "")},
			page: "https://blog.example/posts/",
			want: "https://blog.example/assets/logo.png",
		},
		{
			name: "smallest icon of at least 32px",
			pages: map[string]page{"blog.example/": htmlPage(`
				<link rel="shortcut icon" href="/favicon.ico">
				<link rel="icon" href="/16.png" sizes="16x16">
				<link rel="icon" href="/192.png" sizes="192x192">
				<link rel="apple-touch-icon" href="/touch.png">
				<link rel="icon" href="/48.png" sizes="32x32 48x48">
				<link rel="mask-icon" href="/mask.svg">`, "")},
			page: "https://blog.example/",
			want: "https://blog.example/48.png",
		},
		{
			name: "vector icon",
			pages: map[string]page{"blog.example/": htmlPage(`
				<link rel="icon" href="/192.png" sizes="192x192"><link rel="icon" href="/icon.svg">`, "")},
			page: "https://blog.example/",
			want: "https://blog.example/icon.svg",
		},
		{
			name: "largest of the small ones",
			pages: map[string]page{"blog.example/": htmlPage(`
				<link rel="icon" href="/16.png" sizes="16x16"><link rel="icon" href="/24.png" sizes="24x24">`, "")},
			page: "https://blog.example/",
			want: "https://blog.example/24.png",
		},
		{
			name: "non-http icons are ignored",
			pages: map[string]page{
				"blog.example/":            htmlPage(`<link rel="icon" href="data:image/png;base64,AAAA">`, ""),
				"blog.example/favicon.ico": ico,
			},
			page: "https://blog.example/",
			want: "https://blog.example/favicon.ico",
		},
		{
			name: "favicon.ico of the site the page redirected to",
			pages: map[string]page{
				"old.example/":            {location: "https://new.example/blog/"},
				"new.example/blog/":       htmlPage("", ""),
				"new.example/favicon.ico": ico,
			},
			page: "https://old.example/",
			want: "https://new.example/favicon.ico",
		},
		{
			name: "favicon.ico sniffed as an image",
			pages: map[string]page{
				"blog.example/":            {status: http.StatusNotFound, body: "gone"},
				"blog.example/favicon.ico": {ctype: "application/octet-stream", body: ico.body},
			},
			page: "https://blog.example/",
			want: "https://blog.example/favicon.ico",
		},
		{
			name: "favicon.ico that is an HTML page",
			pages: map[string]page{
				"blog.example/":            htmlPage("", ""),
				"blog.example/favicon.ico": htmlPage("", "not found"),
			},
			page: "https://blog.example/",
		},
		{
			name:  "no icon at all",
			pages: map[string]page{"blog.example/": htmlPage("", "")},
			page:  "https://blog.example/",
		},
		{
			name: "YouTube channel avatar, resized",
			pages: map[string]page{"www.youtube.com/channel/UCaaaaaaaaaaaaaaaaaaaaaa": htmlPage(`
				<link rel="icon" href="https://www.youtube.com/s/favicon_48x48.png" sizes="48x48">
				<meta property="og:image" content="https://yt3.googleusercontent.com/ytc/AIdro_abc=s900-c-k-c0x00ffffff-no-rj">`, "")},
			page: "https://www.youtube.com/channel/UCaaaaaaaaaaaaaaaaaaaaaa",
			want: "https://yt3.googleusercontent.com/ytc/AIdro_abc=s88-c-k-c0x00ffffff-no-rj",
		},
		{
			name: "YouTube page without an avatar",
			pages: map[string]page{"www.youtube.com/playlist?list=PL1": htmlPage(
				`<link rel="icon" href="https://www.youtube.com/s/favicon_48x48.png" sizes="48x48">`, "")},
			page: "https://www.youtube.com/playlist?list=PL1",
			want: "https://www.youtube.com/s/favicon_48x48.png",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			web := newFakeWeb(t, tt.pages)
			got, err := Icon(t.Context(), feed.NewClient(feed.Options{Transport: web, HostSpacing: -1}), tt.page)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("Icon = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIconNetworkError(t *testing.T) {
	web := newFakeWeb(t, nil)
	web.fail = func(r *http.Request) error { return &net.DNSError{Err: "no such host", Name: r.URL.Hostname()} }
	_, err := Icon(t.Context(), feed.NewClient(feed.Options{Transport: web, HostSpacing: -1}), "https://gone.example/")
	if dnsErr := (*net.DNSError)(nil); !errors.As(err, &dnsErr) {
		t.Errorf("err = %v, want the DNS error", err)
	}
}
