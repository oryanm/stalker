package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"runtime/debug"
	"strings"
	"time"
)

const contentSecurityPolicy = "default-src 'self'; img-src 'self' https: data:; style-src 'self'; " +
	"script-src 'self'; frame-ancestors 'none'"

// buildHandler chains: log and recover -> security headers -> public routes ->
// basic auth -> cross-origin protection -> mux.
func (s *Server) buildHandler() http.Handler {
	cop := http.NewCrossOriginProtection()
	cop.SetDenyHandler(s.handle(func(http.ResponseWriter, *http.Request) error {
		return httpError(http.StatusForbidden, "This request came from another site and was blocked.")
	}))
	app := s.requireAuth(cop.Handler(varyOnHTMX(s.routes())))

	public := http.NewServeMux()
	public.HandleFunc("GET /healthz", healthz)
	public.HandleFunc("GET /static/", s.serveStatic)
	public.Handle("/", app)

	return s.logAndRecover(securityHeaders(public))
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte("ok"))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		// not no-referrer: that makes browsers send "Origin: null" on form posts,
		// which cross-origin protection rejects over plain HTTP
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// varyOnHTMX marks app responses as depending on whether htmx asked for a fragment.
func varyOnHTMX(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "HX-Request")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	if s.noAuth {
		return s.requireLocalHost(next)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		// hashing first makes the comparisons constant-time regardless of length
		userSum, passSum := sha256.Sum256([]byte(user)), sha256.Sum256([]byte(pass))
		match := subtle.ConstantTimeCompare(userSum[:], s.userHash[:]) &
			subtle.ConstantTimeCompare(passSum[:], s.passHash[:])
		if !ok || match != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="stalker"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireLocalHost protects a server without authentication from DNS
// rebinding: a page that re-resolves its own name to this machine is
// same-origin with it, but still sends that name as Host.
func (s *Server) requireLocalHost(next http.Handler) http.Handler {
	deny := s.handle(func(http.ResponseWriter, *http.Request) error {
		return httpError(http.StatusForbidden, "Without a password, stalker only answers to localhost, an IP address or an allowed host name.")
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hostAllowed(r.Host) {
			deny.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostAllowed accepts names no other site can take over (localhost, IP
// addresses) and the configured AllowedHosts.
func (s *Server) hostAllowed(hostport string) bool {
	host := normalizeHost(hostport)
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || s.allowedHosts[host] {
		return true
	}
	_, err := netip.ParseAddr(host)
	return err == nil
}

// normalizeHost strips the port and IPv6 brackets from a Host header and lowercases it.
func normalizeHost(hostport string) string {
	host := strings.TrimSpace(hostport)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

// serveStatic serves embedded assets; versioned URLs are cached for a year.
func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/static/")
	if !fs.ValidPath(name) || name == "." {
		http.NotFound(w, r)
		return
	}
	if info, err := fs.Stat(s.static, name); err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	if v := r.URL.Query().Get("v"); v != "" && v == s.version[name] {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeFileFS(w, r, s.static, name)
}

// statusRecorder captures the status for logging. Unwrap lets
// http.ResponseController reach the underlying writer's Flush.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (s *Server) logAndRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("panic serving request", "method", r.Method, "path", r.URL.Path,
					"panic", v, "stack", string(debug.Stack()))
				if rec.status != 0 {
					// the response has started, so the connection is all that can be aborted
					panic(http.ErrAbortHandler)
				}
				s.renderError(rec, r, http.StatusInternalServerError, "Something went wrong.")
			}
			s.log.Debug("http request", "method", r.Method, "path", r.URL.Path,
				"status", rec.status, "duration", time.Since(start))
		}()
		next.ServeHTTP(rec, r)
	})
}
