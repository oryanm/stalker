// Command stalker is a personal, self-hosted follow tracker in the spirit of
// Fraidycat. Run "stalker help" for usage.
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	// dates are shown in the TZ timezone, which must work in images without zoneinfo too
	_ "time/tzdata"

	"github.com/oryanm/stalker/internal/events"
	"github.com/oryanm/stalker/internal/feed"
	"github.com/oryanm/stalker/internal/store"
	"github.com/oryanm/stalker/internal/web"
)

const (
	defaultAddr        = "127.0.0.1:8080"
	defaultDB          = "data/stalker.db"
	defaultUsername    = "stalker"
	defaultConcurrency = 8

	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

const usage = `stalker: a personal, self-hosted follow tracker

Usage:
  stalker serve    [-addr 127.0.0.1:8080] [-db data/stalker.db]
  stalker import   [-db path] file.opml          import follows ("-" reads stdin)
  stalker export   [-db path] > file.opml        write every follow as OPML
  stalker check    [-db path] [-concurrency 8]   fetch every follow now, exit 1 if any failed
  stalker discover URL                           list the feeds found at URL
  stalker help [command]

Environment:
  STALKER_ADDR           listen address for serve (default 127.0.0.1:8080)
  STALKER_DB             database path (default data/stalker.db)
  STALKER_USERNAME       basic auth user (default stalker)
  STALKER_PASSWORD       basic auth password, required by serve
  STALKER_NO_AUTH        1 serves without authentication (local use only)
  STALKER_ALLOWED_HOSTS  host names served without authentication besides localhost
                         and IP addresses, comma-separated
  STALKER_ALLOW_PRIVATE  1 lets fetches reach loopback and private network addresses
  STALKER_LOG_LEVEL      debug, info, warn or error (default info for serve, error otherwise)
  TZ                     timezone of the dates shown, such as America/Toronto (default UTC)

Flags take precedence over the environment.
`

var (
	// errUsage means the command line was wrong; the problem and usage are already printed.
	errUsage = errors.New("usage error")
	// errReported means the command printed its own failure report.
	errReported = errors.New("failure reported")
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	a := &app{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, getenv: os.Getenv}
	code := a.run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}

// app is one invocation of the command with its environment.
type app struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	getenv         func(string) string

	// test seams; zero values use the real implementations
	feedOptions feed.Options
	newHandler  func(*store.Store, web.Discoverer, web.Fetcher, *events.Broker, web.Config) (http.Handler, error)
	listening   func(net.Addr)
}

// run executes the command in args and returns the process exit code.
func (a *app) run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(a.stderr, usage)
		return exitUsage
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "help", "-h", "-help", "--help":
		if len(args) > 0 && isCommand(args[0]) {
			return a.exit(args[0], a.dispatch(ctx, args[0], []string{"-h"}))
		}
		fmt.Fprint(a.stdout, usage)
		return exitOK
	}
	if !isCommand(cmd) {
		fmt.Fprintf(a.stderr, "stalker: unknown command %q\n\n%s", cmd, usage)
		return exitUsage
	}
	return a.exit(cmd, a.dispatch(ctx, cmd, args))
}

func isCommand(name string) bool {
	switch name {
	case "serve", "import", "export", "check", "discover":
		return true
	}
	return false
}

func (a *app) dispatch(ctx context.Context, cmd string, args []string) error {
	s, err := a.parse(cmd, args)
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(a.stderr, &slog.HandlerOptions{Level: s.LogLevel})))
	switch cmd {
	case "serve":
		return a.serve(ctx, s)
	case "import":
		return a.importOPML(ctx, s)
	case "export":
		return a.export(ctx, s)
	case "check":
		return a.check(ctx, s)
	default:
		return a.discover(ctx, s)
	}
}

// feedClient returns the HTTP client for fetching and discovery.
func (a *app) feedClient(s settings) *feed.Client {
	opts := a.feedOptions
	opts.AllowPrivateNetworks = opts.AllowPrivateNetworks || s.AllowPrivate
	return feed.NewClient(opts)
}

func (a *app) exit(cmd string, err error) int {
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return exitOK
	case errors.Is(err, errUsage):
		return exitUsage
	case errors.Is(err, errReported):
		return exitFail
	}
	fmt.Fprintf(a.stderr, "stalker %s: %v\n", cmd, err)
	return exitFail
}

// settings are what a command runs with: flags over environment over defaults.
type settings struct {
	Addr         string
	DB           string
	Username     string
	Password     string
	NoAuth       bool
	AllowedHosts []string
	AllowPrivate bool
	LogLevel     slog.Level
	Concurrency  int
	Args         []string
}

// parse reads the flags of cmd from args and the rest from the environment.
func (a *app) parse(cmd string, args []string) (settings, error) {
	s := settings{
		Addr:        cmp.Or(a.getenv("STALKER_ADDR"), defaultAddr),
		DB:          cmp.Or(a.getenv("STALKER_DB"), defaultDB),
		Username:    cmp.Or(a.getenv("STALKER_USERNAME"), defaultUsername),
		Password:    a.getenv("STALKER_PASSWORD"),
		Concurrency: defaultConcurrency,
		// other commands print their own results, so routine logging would only be noise
		LogLevel: slog.LevelError,
	}

	fs := flag.NewFlagSet("stalker "+cmd, flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	dbFlag := func() { fs.StringVar(&s.DB, "db", s.DB, "database `path` (env STALKER_DB)") }
	var synopsis string
	var nargs int
	switch cmd {
	case "serve":
		synopsis = "[-addr address] [-db path]"
		fs.StringVar(&s.Addr, "addr", s.Addr, "listen `address` (env STALKER_ADDR)")
		dbFlag()
		s.LogLevel = slog.LevelInfo
	case "import":
		synopsis, nargs = "[-db path] file.opml", 1
		dbFlag()
	case "export":
		synopsis = "[-db path] > file.opml"
		dbFlag()
	case "check":
		synopsis = "[-db path] [-concurrency n]"
		dbFlag()
		fs.IntVar(&s.Concurrency, "concurrency", s.Concurrency, "feeds fetched at once")
	case "discover":
		synopsis, nargs = "URL", 1
	}
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: stalker %s %s\n", cmd, synopsis)
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return s, err
		}
		// the flag package has printed the problem and usage
		return s, errUsage
	}
	s.Args = fs.Args()
	var problem string
	switch {
	case len(s.Args) < nargs:
		problem = "missing argument"
	case len(s.Args) > nargs:
		problem = fmt.Sprintf("unexpected argument %q (flags go before arguments)", s.Args[nargs])
	case s.Concurrency < 1:
		problem = "-concurrency must be at least 1"
	}
	if problem != "" {
		fmt.Fprintf(fs.Output(), "stalker %s: %s\n", cmd, problem)
		fs.Usage()
		return s, errUsage
	}

	for _, b := range []struct {
		name string
		dst  *bool
	}{{"STALKER_NO_AUTH", &s.NoAuth}, {"STALKER_ALLOW_PRIVATE", &s.AllowPrivate}} {
		if v := a.getenv(b.name); v != "" {
			on, err := strconv.ParseBool(v)
			if err != nil {
				return s, fmt.Errorf("%s=%q: want 1 or 0", b.name, v)
			}
			*b.dst = on
		}
	}
	for h := range strings.SplitSeq(a.getenv("STALKER_ALLOWED_HOSTS"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			s.AllowedHosts = append(s.AllowedHosts, h)
		}
	}
	if v := a.getenv("STALKER_LOG_LEVEL"); v != "" {
		if err := s.LogLevel.UnmarshalText([]byte(v)); err != nil {
			return s, fmt.Errorf("STALKER_LOG_LEVEL=%q: want debug, info, warn or error", v)
		}
	}
	return s, nil
}
