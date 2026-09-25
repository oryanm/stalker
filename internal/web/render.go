package web

import (
	"bytes"
	"context"
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/oryanm/stalker/internal/model"
	"github.com/oryanm/stalker/internal/store"
)

var pageNames = []string{"home", "add", "choose", "edit", "posts", "settings", "error"}

func (s *Server) parseTemplates() error {
	funcs := template.FuncMap{
		"timeAgo":     timeAgo,
		"ageClass":    ageClass,
		"sparkline":   sparkline,
		"dial":        dial,
		"tiers":       func() []model.Tier { return model.Tiers },
		"sortOptions": func() []sortOption { return sortOptions },
		"safeURL":     safeURL,
		"static":      s.staticURL,
		"homeURL":     func(tag string) string { return homeURL(tag, nil) },
		"tierURL":     func(tag string, tier model.Importance) string { return homeURL(tag, &tier) },
		"errorText":   errorSummary,
		"withNow":     withNow,
		"tierSelect":  tierSelect,
		"theme":       s.currentTheme,
		"themes":      func() []theme { return themes },
	}
	base, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/partials.html")
	if err != nil {
		return err
	}
	s.pages = make(map[string]*template.Template, len(pageNames))
	for _, name := range pageNames {
		t, err := base.Clone()
		if err != nil {
			return err
		}
		if s.pages[name], err = t.ParseFS(templateFS, "templates/"+name+".html"); err != nil {
			return err
		}
	}
	s.base = base
	return nil
}

func (s *Server) staticURL(name string) string {
	if v, ok := s.version[name]; ok {
		return "/static/" + name + "?v=" + v
	}
	return "/static/" + name
}

// layout is the data every full page shares.
type layout struct {
	Title  string
	Tabs   []tagTab
	AddURL string
}

// newLayout loads the tag tabs for the header. A store failure only costs the tabs.
func (s *Server) newLayout(ctx context.Context, title, tag string) layout {
	l := layout{Title: title, AddURL: addURL(tag, nil)}
	follows, err := s.st.ListFollows(ctx)
	if err != nil {
		s.log.Warn("load tag tabs", "err", err)
		return l
	}
	if len(follows) > 0 {
		l.Tabs = buildTagTabs(follows, tag, s.now())
	}
	return l
}

// isFragment reports whether htmx asked for a partial (not a boosted page,
// which needs the whole document, nor a history restore).
func isFragment(r *http.Request) bool {
	h := r.Header
	return h.Get("HX-Request") == "true" && h.Get("HX-Boosted") != "true" &&
		h.Get("HX-History-Restore-Request") != "true"
}

func (s *Server) page(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		// a boosted POST that re-renders a form must not put its URL in the history,
		// where a reload would GET a route that only accepts POST
		w.Header().Set("HX-Push-Url", "false")
	}
	s.execute(w, status, s.pages[name], "layout", data)
}

func (s *Server) fragment(w http.ResponseWriter, status int, name string, data any) {
	s.execute(w, status, s.base, name, data)
}

// execute renders into a buffer first so a template error becomes a clean 500.
func (s *Server) execute(w http.ResponseWriter, status int, t *template.Template, name string, data any) {
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		s.log.Error("render template", "template", name, "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// statusError is an error with a user-facing message and HTTP status.
type statusError struct {
	status int
	msg    string
}

func (e *statusError) Error() string { return strconv.Itoa(e.status) + " " + e.msg }

func httpError(status int, msg string) error { return &statusError{status, msg} }

var (
	errNotFound  = httpError(http.StatusNotFound, "There is nothing here.")
	errBadID     = httpError(http.StatusBadRequest, "That is not a valid follow id.")
	errEmptyURL  = errors.New("enter a URL")
	errLongURL   = errors.New("that URL is too long")
	errBadURL    = errors.New("enter a full http:// or https:// URL")
	errNoFollows = httpError(http.StatusNotFound, "That follow does not exist (it may have been deleted).")
	// errUnreadableForm is a malformed or oversized form body, which a browser does not send
	errUnreadableForm = httpError(http.StatusBadRequest, "The form could not be read.")
)

const maxURLLen = 2048

// handle adapts a handler that returns an error, rendering the error page.
func (s *Server) handle(h func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			s.fail(w, r, err)
		}
	}
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		err = errNoFollows
	}
	if se, ok := errors.AsType[*statusError](err); ok {
		s.renderError(w, r, se.status, se.msg)
		return
	}
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		s.renderError(w, r, http.StatusRequestEntityTooLarge, "That upload is too large.")
		return
	}
	if r.Context().Err() != nil {
		// the client went away; nobody is left to read an error page
		s.log.Debug("request cancelled", "path", r.URL.Path, "err", err)
		return
	}
	s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	s.renderError(w, r, http.StatusInternalServerError, "Something went wrong. The details are in the server log.")
}

type errorPage struct {
	layout
	Status     int
	StatusText string
	Message    string
}

// renderError shows a full error page, or a one-line message where htmx swaps a fragment.
func (s *Server) renderError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	data := errorPage{
		layout:     layout{Title: http.StatusText(status), AddURL: "/add"},
		Status:     status,
		StatusText: http.StatusText(status),
		Message:    msg,
	}
	if isFragment(r) {
		s.fragment(w, status, "flash", data)
		return
	}
	s.page(w, r, status, "error", data)
}

// sentence capitalizes an error message for display.
func sentence(msg string) string {
	if msg == "" {
		return msg
	}
	r := []rune(msg)
	return strings.ToUpper(string(r[0])) + string(r[1:])
}
