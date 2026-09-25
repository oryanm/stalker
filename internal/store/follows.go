package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/oryanm/stalker/internal/feedurl"
	"github.com/oryanm/stalker/internal/model"
)

var errNoFeedURL = errors.New("store: follow has no feed URL")

// followColumns selects a follow as scanned by scanFollow, tags as a sorted JSON array.
const followColumns = `
	f.id, f.url, f.feed_url, f.title, f.feed_title, f.description, f.photo_url, f.importance,
	f.created_at, f.edited_at, f.etag, f.last_modified, f.last_fetched_at, f.next_fetch_at,
	f.last_error, f.error_count, f.last_post_at,
	(SELECT json_group_array(t.tag ORDER BY t.tag) FROM follow_tags t WHERE t.follow_id = f.id)`

type scanner interface{ Scan(dest ...any) error }

func scanFollow(sc scanner) (model.Follow, error) {
	var f model.Follow
	err := sc.Scan(
		&f.ID, &f.URL, &f.FeedURL, &f.Title, &f.FeedTitle, &f.Description, &f.PhotoURL, &f.Importance,
		msTime{&f.CreatedAt}, msTime{&f.EditedAt}, &f.ETag, &f.LastModified, msTime{&f.LastFetchedAt},
		msTime{&f.NextFetchAt}, &f.LastError, &f.ErrorCount, msTime{&f.LastPostAt},
		jsonStrings{&f.Tags},
	)
	return f, err
}

func (s *Store) queryFollows(ctx context.Context, query string, args ...any) ([]model.Follow, error) {
	rows, err := s.r.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Follow
	for rows.Next() {
		f, err := scanFollow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ListFollows returns every follow with its tags, in no particular order.
func (s *Store) ListFollows(ctx context.Context) ([]model.Follow, error) {
	follows, err := s.queryFollows(ctx, `SELECT `+followColumns+` FROM follows f ORDER BY f.id`)
	if err != nil {
		return nil, fmt.Errorf("store: list follows: %w", err)
	}
	return follows, nil
}

// GetFollow returns ErrNotFound when the id does not exist.
func (s *Store) GetFollow(ctx context.Context, id int64) (model.Follow, error) {
	return s.getFollow(ctx, `SELECT `+followColumns+` FROM follows f WHERE f.id = ?`, id)
}

// GetFollowByFeedURL returns ErrNotFound when no follow has this feed URL.
//
// Without an exact match it returns the follow of the same feed under
// another URL (see feedurl.Canonical), the one ErrDuplicate refers to.
func (s *Store) GetFollowByFeedURL(ctx context.Context, feedURL string) (model.Follow, error) {
	f, err := s.getFollow(ctx, `SELECT `+followColumns+` FROM follows f WHERE f.feed_url = ?`, feedURL)
	if !errors.Is(err, ErrNotFound) {
		return f, err
	}
	feeds, err := followedFeeds(ctx, s.r)
	if err != nil {
		return model.Follow{}, fmt.Errorf("store: get follow: %w", err)
	}
	id, ok := feeds[feedKey(feedURL)]
	if !ok {
		return model.Follow{}, ErrNotFound
	}
	return s.GetFollow(ctx, id)
}

// feedKey identifies a feed: URLs with the same key are fetched from the same address.
func feedKey(feedURL string) string {
	return feedurl.Canonical(strings.TrimSpace(feedURL))
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// followedFeeds maps the feed key of every follow to its id.
func followedFeeds(ctx context.Context, q querier) (map[string]int64, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, feed_url FROM follows`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int64)
	for rows.Next() {
		var id int64
		var feedURL string
		if err := rows.Scan(&id, &feedURL); err != nil {
			return nil, err
		}
		out[feedKey(feedURL)] = id
	}
	return out, rows.Err()
}

func (s *Store) getFollow(ctx context.Context, query string, arg any) (model.Follow, error) {
	f, err := scanFollow(s.r.QueryRowContext(ctx, query, arg))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return model.Follow{}, ErrNotFound
	case err != nil:
		return model.Follow{}, fmt.Errorf("store: get follow: %w", err)
	}
	return f, nil
}

// CreateFollow inserts f and sets f.ID. Zero CreatedAt/EditedAt default to now,
// zero NextFetchAt defaults to now. Returns ErrDuplicate on an existing feed URL.
//
// A feed URL fetched from the same address as an existing one (a YouTube
// channel page and its videos.xml feed, see feedurl.Canonical) is a duplicate
// too. On success *f holds exactly what was stored: defaults applied, tags
// normalized and times in UTC truncated to the millisecond.
func (s *Store) CreateFollow(ctx context.Context, f *model.Follow) error {
	row, err := prepareNewFollow(*f, s.now())
	if err != nil {
		return err
	}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		feeds, err := followedFeeds(ctx, tx)
		if err != nil {
			return err
		}
		id, err := insertFollow(ctx, tx, row, feeds)
		if err != nil {
			return err
		}
		if id == 0 {
			return ErrDuplicate
		}
		row.ID = id
		return insertTags(ctx, tx, id, row.Tags)
	})
	if errors.Is(err, ErrDuplicate) {
		return ErrDuplicate
	}
	if err != nil {
		return fmt.Errorf("store: create follow: %w", err)
	}
	*f = row
	return nil
}

// ImportFollows inserts follows in one transaction, skipping feed URLs that
// already exist. Returns how many were added and skipped.
//
// Follows without a feed URL and repeats within follows are skipped too, all
// compared like CreateFollow does.
func (s *Store) ImportFollows(ctx context.Context, follows []model.Follow) (added, skipped int, err error) {
	now := s.now()
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		added, skipped = 0, 0
		feeds, err := followedFeeds(ctx, tx)
		if err != nil {
			return err
		}
		for _, f := range follows {
			row, err := prepareNewFollow(f, now)
			if errors.Is(err, errNoFeedURL) {
				skipped++
				continue
			}
			if err != nil {
				return err
			}
			id, err := insertFollow(ctx, tx, row, feeds)
			if err != nil {
				return err
			}
			if id == 0 {
				skipped++
				continue
			}
			if err := insertTags(ctx, tx, id, row.Tags); err != nil {
				return err
			}
			added++
		}
		return nil
	})
	if err != nil {
		return 0, 0, fmt.Errorf("store: import follows: %w", err)
	}
	return added, skipped, nil
}

// prepareNewFollow applies CreateFollow's defaults and normalization.
func prepareNewFollow(f model.Follow, now time.Time) (model.Follow, error) {
	if strings.TrimSpace(f.FeedURL) == "" {
		return f, errNoFeedURL
	}
	f.ID = 0
	f.Tags = normalizeTags(f.Tags)
	for _, t := range []*time.Time{&f.CreatedAt, &f.EditedAt, &f.NextFetchAt} {
		if t.IsZero() {
			*t = now
		}
	}
	for _, t := range []*time.Time{&f.CreatedAt, &f.EditedAt, &f.LastFetchedAt, &f.NextFetchAt, &f.LastPostAt} {
		*t = asStored(*t)
	}
	return f, nil
}

// insertFollow returns id 0 when feeds, the followedFeeds of tx, already has
// the feed; otherwise it adds the new follow to feeds.
func insertFollow(ctx context.Context, tx *sql.Tx, f model.Follow, feeds map[string]int64) (int64, error) {
	// checked first because a conflicting AUTOINCREMENT insert still uses up an id
	key := feedKey(f.FeedURL)
	if _, ok := feeds[key]; ok {
		return 0, nil
	}
	var id int64
	err := tx.QueryRowContext(ctx, `
		INSERT INTO follows (
			url, feed_url, title, feed_title, description, photo_url, importance, created_at, edited_at,
			etag, last_modified, last_fetched_at, next_fetch_at, last_error, error_count, last_post_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (feed_url) DO NOTHING
		RETURNING id`,
		f.URL, f.FeedURL, f.Title, f.FeedTitle, f.Description, f.PhotoURL, int(f.Importance),
		toMS(f.CreatedAt), toMS(f.EditedAt), f.ETag, f.LastModified, toMS(f.LastFetchedAt),
		toMS(f.NextFetchAt), f.LastError, f.ErrorCount, toMS(f.LastPostAt),
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err == nil {
		feeds[key] = id
	}
	return id, err
}

// insertTags expects normalized tags.
func insertTags(ctx context.Context, tx *sql.Tx, followID int64, tags []string) error {
	for _, tag := range tags {
		if _, err := tx.ExecContext(ctx, `INSERT INTO follow_tags (follow_id, tag) VALUES (?, ?)`, followID, tag); err != nil {
			return err
		}
	}
	return nil
}

// normalizeTags trims, drops empty and duplicate tags and sorts the rest.
// HomeTag is kept when given: an explicit house tag is meaningful.
func normalizeTags(tags []string) []string {
	var out []string
	for _, t := range tags {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// UpdateFollowSettings saves the user-editable fields: URL, FeedURL, Title,
// Importance, Tags and EditedAt. Changing FeedURL resets ETag/LastModified
// and schedules an immediate fetch. Returns ErrNotFound or ErrDuplicate.
//
// A zero EditedAt defaults to now. Changing FeedURL also clears the error
// state; posts are kept since a moved feed usually keeps its GUIDs. Moving the
// follow to a more important tier schedules it for now unless it is due
// sooner, so it does not wait out the old tier's longer interval.
func (s *Store) UpdateFollowSettings(ctx context.Context, f model.Follow) error {
	if strings.TrimSpace(f.FeedURL) == "" {
		return errNoFeedURL
	}
	now := s.now()
	edited := f.EditedAt
	if edited.IsZero() {
		edited = now
	}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var oldFeedURL string
		var oldImportance model.Importance
		err := tx.QueryRowContext(ctx, `SELECT feed_url, importance FROM follows WHERE id = ?`, f.ID).
			Scan(&oldFeedURL, &oldImportance)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if f.FeedURL != oldFeedURL {
			feeds, err := followedFeeds(ctx, tx)
			if err != nil {
				return err
			}
			if id, ok := feeds[feedKey(f.FeedURL)]; ok && id != f.ID {
				return ErrDuplicate
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE follows
				SET feed_url = ?, etag = '', last_modified = '', next_fetch_at = ?, last_error = '', error_count = 0
				WHERE id = ?`, f.FeedURL, toMS(now), f.ID); err != nil {
				return err
			}
		}
		promoted := model.NormalizeImportance(int(f.Importance)) < model.NormalizeImportance(int(oldImportance))
		if _, err := tx.ExecContext(ctx, `
			UPDATE follows
			SET url = ?1, title = ?2, importance = ?3, edited_at = ?4,
				next_fetch_at = iif(?5, min(next_fetch_at, ?6), next_fetch_at)
			WHERE id = ?7`,
			f.URL, f.Title, int(f.Importance), toMS(edited), promoted, toMS(now), f.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM follow_tags WHERE follow_id = ?`, f.ID); err != nil {
			return err
		}
		return insertTags(ctx, tx, f.ID, normalizeTags(f.Tags))
	})
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrDuplicate) {
		return err
	}
	if err != nil {
		return fmt.Errorf("store: update follow %d: %w", f.ID, err)
	}
	return nil
}

// DeleteFollow removes the follow with its tags and posts.
// Returns ErrNotFound when the id does not exist.
func (s *Store) DeleteFollow(ctx context.Context, id int64) error {
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{
			`DELETE FROM posts WHERE follow_id = ?`,
			`DELETE FROM follow_tags WHERE follow_id = ?`,
		} {
			if _, err := tx.ExecContext(ctx, q, id); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM follows WHERE id = ?`, id)
		return affectedOne(res, err)
	})
	if errors.Is(err, ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: delete follow %d: %w", id, err)
	}
	return nil
}

// DueFollows returns up to limit follows whose NextFetchAt <= now, oldest due first.
// A limit below 1 returns nothing, so a poller without free workers gets no work.
func (s *Store) DueFollows(ctx context.Context, now time.Time, limit int) ([]model.Follow, error) {
	if limit < 1 {
		return nil, nil
	}
	follows, err := s.queryFollows(ctx, `
		SELECT `+followColumns+` FROM follows f
		WHERE f.next_fetch_at <= ?
		ORDER BY f.next_fetch_at, f.id
		LIMIT ?`, toMS(now), limit)
	if err != nil {
		return nil, fmt.Errorf("store: due follows: %w", err)
	}
	return follows, nil
}

// affectedOne maps an UPDATE or DELETE that touched no row to ErrNotFound.
func affectedOne(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
