package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/oryanm/stalker/internal/discover"
	"github.com/oryanm/stalker/internal/model"
	"github.com/oryanm/stalker/internal/opml"
	"github.com/oryanm/stalker/internal/poller"
	"github.com/oryanm/stalker/internal/store"
)

const (
	// exportTitle mirrors Fraidycat's "Fraidycat Follows".
	exportTitle = "Stalker Follows"
	// maxTitleWidth keeps the check table readable in a terminal.
	maxTitleWidth = 48
	// discoverTimeout bounds a discovery that probes many candidate URLs.
	discoverTimeout = 2 * time.Minute
)

// importOPML adds the follows in an OPML file, skipping feed URLs already followed.
func (a *app) importOPML(ctx context.Context, s settings) error {
	name := s.Args[0]
	r := a.stdin
	if name != "-" {
		f, err := os.Open(name)
		if err != nil {
			return err
		}
		defer f.Close()
		r = f
	}
	// parsed before opening the store so a bad file does not leave an empty database behind
	entries, err := opml.Parse(r)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}

	st, err := store.Open(s.DB)
	if err != nil {
		return err
	}
	defer st.Close()
	now := time.Now()
	follows := make([]model.Follow, 0, len(entries))
	for _, e := range entries {
		follows = append(follows, e.ToFollow(now))
	}
	added, skipped, err := st.ImportFollows(ctx, follows)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "added %d, skipped %d\n", added, skipped)
	return nil
}

// export writes every follow as Fraidycat-compatible OPML.
func (a *app) export(ctx context.Context, s settings) error {
	st, err := openExisting(s.DB)
	if err != nil {
		return err
	}
	defer st.Close()
	follows, err := st.ListFollows(ctx)
	if err != nil {
		return err
	}
	return opml.Write(a.stdout, exportTitle, follows)
}

// check fetches every follow now and prints one line per follow.
func (a *app) check(ctx context.Context, s settings) error {
	st, err := openExisting(s.DB)
	if err != nil {
		return err
	}
	defer st.Close()
	pl := poller.New(st, a.feedClient(s), nil, poller.Options{})

	start := time.Now()
	results, err := pl.CheckAll(ctx, s.Concurrency)
	if results == nil && err != nil {
		return err
	}
	failed := writeCheckTable(a.stdout, results)
	fmt.Fprintf(a.stdout, "\n%d follows: %d OK, %d failed in %s\n",
		len(results), len(results)-failed, failed, time.Since(start).Round(100*time.Millisecond))
	switch {
	case err != nil:
		return fmt.Errorf("interrupted: %w", err)
	case failed > 0:
		return errReported
	}
	return nil
}

// writeCheckTable prints results as an aligned table and returns how many failed.
func writeCheckTable(w io.Writer, results []poller.CheckResult) (failed int) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "STATUS\tTITLE\tPOSTS\tTIME\tERROR")
	for _, r := range results {
		status, msg := "OK", ""
		if r.Err != nil {
			status, msg = "FAIL", r.Err.Error()
			failed++
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n",
			status, cell(r.Follow.DisplayTitle(), maxTitleWidth), r.Posts, r.Duration.Round(time.Millisecond), cell(msg, 0))
	}
	_ = tw.Flush()
	return failed
}

// cell makes s safe for a tabwriter cell and cuts it to width runes (0 means no limit).
func cell(s string, width int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); width > 0 && len(r) > width {
		s = string(r[:width-1]) + "…"
	}
	return s
}

// discover prints the verified feeds found for a URL.
func (a *app) discover(ctx context.Context, s settings) error {
	ctx, cancel := context.WithTimeout(ctx, discoverTimeout)
	defer cancel()
	candidates, err := discover.New(a.feedClient(s)).Discover(ctx, s.Args[0])
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FEED\tTITLE\tSITE")
	for _, c := range candidates {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", c.FeedURL, cell(c.Title, maxTitleWidth), c.SiteURL)
	}
	return tw.Flush()
}

// openExisting opens the database at path, refusing to create a new one.
func openExisting(path string) (*store.Store, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no database at %s (set -db or STALKER_DB, or run import or serve first)", path)
	}
	return store.Open(path)
}
