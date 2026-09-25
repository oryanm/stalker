package web

import (
	"context"
	"net/http"
)

// settingTheme is the store setting holding the selected theme id.
const settingTheme = "theme"

// theme is a stylesheet under static/themes/. Every theme styles the same
// markup, so adding one means adding a CSS file and an entry here.
type theme struct {
	ID          string
	Name        string
	Description string
	ColorScheme string // the color-scheme meta value: "dark", "light" or "light dark"
}

// themes lists the selectable themes; the first is the default.
var themes = []theme{
	{"receiver", "Receiver", "A 1970s stereo receiver in the Darcula Forest colours, with a tuning dial for your follows.", "dark"},
	{"classic", "Classic", "The original Fraidycat-style list. Follows your system’s light or dark setting.", "light dark"},
}

func findTheme(id string) (theme, bool) {
	for _, t := range themes {
		if t.ID == id {
			return t, true
		}
	}
	return theme{}, false
}

// CSS is the theme's stylesheet path under static/.
func (t theme) CSS() string { return "themes/" + t.ID + ".css" }

// currentTheme is the selected theme, cached so rendering a page does not read the store.
func (s *Server) currentTheme() theme {
	s.themeMu.RLock()
	defer s.themeMu.RUnlock()
	return s.theme
}

// loadTheme reads the stored theme; a missing or unknown one falls back to the default.
func (s *Server) loadTheme(ctx context.Context) {
	t := themes[0]
	id, err := s.st.GetSetting(ctx, settingTheme, t.ID)
	if err != nil {
		s.log.Warn("read theme setting", "err", err)
	} else if found, ok := findTheme(id); ok {
		t = found
	}
	s.themeMu.Lock()
	s.theme = t
	s.themeMu.Unlock()
}

func (s *Server) saveTheme(w http.ResponseWriter, r *http.Request) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		return errUnreadableForm
	}
	t, ok := findTheme(r.PostForm.Get("theme"))
	if !ok {
		return httpError(http.StatusBadRequest, "That is not a theme.")
	}
	if err := s.st.SetSetting(r.Context(), settingTheme, t.ID); err != nil {
		return err
	}
	s.themeMu.Lock()
	s.theme = t
	s.themeMu.Unlock()
	http.Redirect(w, r, "/settings?saved=1#theme", http.StatusSeeOther)
	return nil
}
