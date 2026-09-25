package feed

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchRequestsRewrittenURL(t *testing.T) {
	body := readFixture(t, "youtube.xml")
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.Host + r.URL.RequestURI()
		w.Header().Set("Content-Type", "text/xml")
		w.Write(body)
	}))
	defer srv.Close()

	c := newTestClient(Options{Transport: hostRouter{srv}})
	res, err := c.Fetch(t.Context(), "https://www.youtube.com/channel/UCxyz", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := "www.youtube.com/feeds/videos.xml?channel_id=UCxyz"; gotURL != want {
		t.Errorf("requested %q, want %q", gotURL, want)
	}
	if want := "https://www.youtube.com/feeds/videos.xml?channel_id=UCxyz"; res.FinalURL != want {
		t.Errorf("FinalURL = %q, want %q", res.FinalURL, want)
	}
}

func TestFetchNotAFeedAfterRedirect(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/feed.rss", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/login?dest=feed", http.StatusFound)
	})
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write(readFixture(t, "page.html"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_, err := newTestClient(Options{}).Fetch(t.Context(), srv.URL+"/feed.rss", "", "")
	re, ok := errors.AsType[*RedirectError](err)
	if !ok || !errors.Is(err, ErrNotAFeed) {
		t.Fatalf("err = %v, want RedirectError wrapping ErrNotAFeed", err)
	}
	if want := srv.URL + "/login?dest=feed"; re.URL != want {
		t.Errorf("URL = %q, want %q", re.URL, want)
	}

	// without a redirect the plain verdict is returned
	_, err = newTestClient(Options{}).Fetch(t.Context(), srv.URL+"/login", "", "")
	if _, ok := errors.AsType[*RedirectError](err); ok || !errors.Is(err, ErrNotAFeed) {
		t.Errorf("err = %v, want plain ErrNotAFeed", err)
	}
}
