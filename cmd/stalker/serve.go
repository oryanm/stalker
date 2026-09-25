package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/oryanm/stalker/internal/discover"
	"github.com/oryanm/stalker/internal/events"
	"github.com/oryanm/stalker/internal/poller"
	"github.com/oryanm/stalker/internal/store"
	"github.com/oryanm/stalker/internal/web"
)

const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 120 * time.Second
	shutdownTimeout   = 10 * time.Second
	// drainGrace lets ordinary requests finish before SSE streams are cancelled.
	drainGrace = time.Second
)

// serve runs the web UI and the poller until ctx is cancelled.
func (a *app) serve(ctx context.Context, s settings) error {
	if s.Password == "" && !s.NoAuth {
		return errors.New("STALKER_PASSWORD is not set; set it, or set STALKER_NO_AUTH=1 to run without authentication on a trusted machine")
	}
	st, err := store.Open(s.DB)
	if err != nil {
		return err
	}
	defer st.Close()

	fc := a.feedClient(s)
	ev := events.NewBroker()
	pl := poller.New(st, fc, ev, poller.Options{})
	cfg := web.Config{Username: s.Username, Password: s.Password, NoAuth: s.NoAuth, AllowedHosts: s.AllowedHosts}
	h, closeStreams, err := a.handler(st, discover.New(fc), pl, ev, cfg)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}

	// SSE handlers only return when their request context ends, which Shutdown alone never causes
	base, cancelBase := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelBase()
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		// no ReadTimeout or WriteTimeout: either would cut off long-lived SSE streams
		BaseContext: func(net.Listener) context.Context { return base },
		ErrorLog:    slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
	}
	srv.RegisterOnShutdown(closeStreams)
	srv.RegisterOnShutdown(func() { time.AfterFunc(drainGrace, cancelBase) })

	pollCtx, stopPolling := context.WithCancel(ctx)
	defer stopPolling()
	polled := make(chan error, 1)
	go func() { polled <- pl.Run(pollCtx) }()
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	slog.Info("stalker listening", "addr", ln.Addr().String(), "db", s.DB, "auth", !s.NoAuth, "timezone", time.Now().Format("MST -07:00"))
	if a.listening != nil {
		a.listening(ln.Addr())
	}

	var serveErr error
	select {
	case <-ctx.Done():
		slog.Info("shutting down")
	case serveErr = <-served:
		serveErr = fmt.Errorf("serve: %w", serveErr)
	}

	stopPolling()
	if err := <-polled; err != nil {
		slog.Error("poller stopped", "err", err)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("forcing connections closed", "err", err)
		_ = srv.Close()
	}
	return serveErr
}

// handler builds the web UI and a func that ends its SSE streams at shutdown.
func (a *app) handler(st *store.Store, d web.Discoverer, f web.Fetcher, ev *events.Broker, cfg web.Config) (http.Handler, func(), error) {
	if a.newHandler != nil {
		h, err := a.newHandler(st, d, f, ev, cfg)
		return h, func() {}, err
	}
	s, err := web.New(st, d, f, ev, cfg)
	if err != nil {
		return nil, nil, err
	}
	return s.Handler(), s.CloseStreams, nil
}
