// Package web serves the HTML UI (server-rendered templates, htmx, SSE).
package web

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/oryanm/stalker/internal/discover"
	"github.com/oryanm/stalker/internal/events"
	"github.com/oryanm/stalker/internal/store"
)

// Fetcher is the part of *poller.Poller the UI uses.
type Fetcher interface {
	FetchNow(ctx context.Context, id int64) error
	Kick()
	IsFetching(id int64) bool
}

// Discoverer is the part of *discover.Discoverer the UI uses.
type Discoverer interface {
	Discover(ctx context.Context, input string) ([]discover.Candidate, error)
}

type Config struct {
	Username string // basic auth user
	Password string // basic auth password; empty requires NoAuth
	NoAuth   bool   // disables basic auth (local use only)
	// AllowedHosts are host names served besides localhost and IP addresses
	// when NoAuth is set; any other Host header is refused.
	AllowedHosts []string
}

const (
	defaultUsername = "stalker"
	// fetchWait bounds how long add and refresh wait for a fetch before responding.
	fetchWait = 20 * time.Second
	// fetchLimit bounds a fetch that outlives the request which started it.
	fetchLimit      = 2 * time.Minute
	discoverTimeout = 45 * time.Second
	pingEvery       = 25 * time.Second
	coalesceEvery   = 500 * time.Millisecond
	maxUploadBytes  = 5 << 20
	maxFormBytes    = 1 << 20
	latestPerFollow = 3
	recentPostLimit = 20
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

type Server struct {
	st    *store.Store
	disc  Discoverer
	fetch Fetcher
	ev    *events.Broker
	log   *slog.Logger

	noAuth       bool
	allowedHosts map[string]bool
	userHash     [sha256.Size]byte
	passHash     [sha256.Size]byte

	base    *template.Template            // layout-free partials, used for fragments
	pages   map[string]*template.Template // one per page, each defining "content"
	static  fs.FS
	version map[string]string // static file name -> content hash for cache busting
	handler http.Handler

	// overridable in tests
	now       func() time.Time
	fetchWait time.Duration
	pingEvery time.Duration
	coalesce  time.Duration

	done      chan struct{}
	closeOnce sync.Once
}

// New validates cfg (a password is required unless NoAuth) and parses templates.
func New(st *store.Store, disc Discoverer, f Fetcher, ev *events.Broker, cfg Config) (*Server, error) {
	switch {
	case st == nil:
		return nil, errors.New("web: nil store")
	case disc == nil:
		return nil, errors.New("web: nil discoverer")
	case f == nil:
		return nil, errors.New("web: nil fetcher")
	case !cfg.NoAuth && cfg.Password == "":
		return nil, errors.New("web: a password is required unless auth is disabled")
	}
	if ev == nil {
		ev = events.NewBroker()
	}
	if cfg.Username == "" {
		cfg.Username = defaultUsername
	}
	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}
	s := &Server{
		st:           st,
		disc:         disc,
		fetch:        f,
		ev:           ev,
		log:          slog.Default(),
		noAuth:       cfg.NoAuth,
		allowedHosts: map[string]bool{},
		userHash:     sha256.Sum256([]byte(cfg.Username)),
		passHash:     sha256.Sum256([]byte(cfg.Password)),
		static:       static,
		now:          time.Now,
		fetchWait:    fetchWait,
		pingEvery:    pingEvery,
		coalesce:     coalesceEvery,
		done:         make(chan struct{}),
	}
	for _, h := range cfg.AllowedHosts {
		if h = normalizeHost(h); h != "" {
			s.allowedHosts[h] = true
		}
	}
	if s.version, err = hashFiles(static); err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}
	if err := s.parseTemplates(); err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}
	s.handler = s.buildHandler()
	return s, nil
}

// Handler returns the full HTTP handler including auth, cross-origin
// protection and security headers. /healthz and /static/ are served without auth.
func (s *Server) Handler() http.Handler { return s.handler }

// CloseStreams ends every open /events stream and refuses new ones. Register
// it with http.Server.RegisterOnShutdown: Shutdown does not cancel request
// contexts, so it would otherwise wait for every SSE client to leave.
func (s *Server) CloseStreams() {
	s.closeOnce.Do(func() { close(s.done) })
}

func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handle(s.home))
	mux.HandleFunc("GET /follows/{id}/posts", s.handle(s.posts))
	mux.HandleFunc("GET /add", s.handle(s.showAdd))
	mux.HandleFunc("POST /follows", s.handle(s.add))
	mux.HandleFunc("POST /follows/choose", s.handle(s.choose))
	mux.HandleFunc("GET /follows/{id}/edit", s.handle(s.showEdit))
	mux.HandleFunc("POST /follows/{id}", s.handle(s.save))
	mux.HandleFunc("POST /follows/{id}/delete", s.handle(s.remove))
	mux.HandleFunc("POST /follows/{id}/refresh", s.handle(s.refresh))
	mux.HandleFunc("GET /settings", s.handle(s.settings))
	mux.HandleFunc("POST /settings", s.handle(s.saveSettings))
	mux.HandleFunc("POST /import", s.handle(s.importOPML))
	mux.HandleFunc("GET /export.opml", s.handle(s.exportOPML))
	mux.HandleFunc("GET /events", s.events)
	mux.HandleFunc("/", s.handle(func(http.ResponseWriter, *http.Request) error {
		return errNotFound
	}))
	return mux
}

// hashFiles fingerprints every static file so asset URLs change on deploy.
func hashFiles(fsys fs.FS) (map[string]string, error) {
	out := map[string]string{}
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		out[path] = hex.EncodeToString(sum[:5])
		return nil
	})
	return out, err
}
