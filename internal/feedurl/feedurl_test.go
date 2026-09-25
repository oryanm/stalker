package feedurl

import "testing"

func TestCanonical(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"youtube channel page", "https://www.youtube.com/channel/UCYO_jab_esuFRV4b17AJtAw",
			"https://www.youtube.com/feeds/videos.xml?channel_id=UCYO_jab_esuFRV4b17AJtAw"},
		{"youtube channel tab", "https://m.youtube.com/channel/UC9-y-6csu5WGm29I7JiwpnA/videos",
			"https://www.youtube.com/feeds/videos.xml?channel_id=UC9-y-6csu5WGm29I7JiwpnA"},
		{"youtube playlist page", "https://youtube.com/playlist?list=PL1234",
			"https://www.youtube.com/feeds/videos.xml?playlist_id=PL1234"},
		{"youtube feed kept", "https://www.youtube.com/feeds/videos.xml?channel_id=UC1",
			"https://www.youtube.com/feeds/videos.xml?channel_id=UC1"},
		{"youtube video kept", "https://www.youtube.com/watch?v=dQw4w9WgXcQ", "https://www.youtube.com/watch?v=dQw4w9WgXcQ"},
		{"youtube non-UC channel kept", "https://www.youtube.com/channel/abc", "https://www.youtube.com/channel/abc"},
		{"old reddit feed", "https://old.reddit.com/r/golang/top/.rss?t=week", "https://www.reddit.com/r/golang/top/.rss?t=week"},
		{"old reddit http feed", "http://old.reddit.com/r/golang/.rss", "https://www.reddit.com/r/golang/.rss"},
		{"old reddit page kept", "https://old.reddit.com/r/golang/", "https://old.reddit.com/r/golang/"},
		{"www reddit kept", "https://www.reddit.com/r/golang/.rss", "https://www.reddit.com/r/golang/.rss"},
		{"lookalike host kept", "https://notyoutube.com/channel/UC1", "https://notyoutube.com/channel/UC1"},
		{"other site kept", "https://go.dev/blog/feed/", "https://go.dev/blog/feed/"},
		{"unparseable kept", "http://%zz", "http://%zz"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Canonical(tt.in); got != tt.want {
				t.Errorf("Canonical(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestIsYouTubeHost(t *testing.T) {
	for host, want := range map[string]bool{
		"youtube.com": true, "www.youtube.com": true, "M.YouTube.com": true,
		"notyoutube.com": false, "youtube.com.evil.example": false, "": false,
	} {
		if got := IsYouTubeHost(host); got != want {
			t.Errorf("IsYouTubeHost(%q) = %v, want %v", host, got, want)
		}
	}
}
