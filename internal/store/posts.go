package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/oryanm/stalker/internal/model"
)

const (
	// keepPosts newest posts of a follow survive pruning regardless of age.
	keepPosts = 50
	// pruneAge is how old a post beyond the newest keepPosts must be to be pruned.
	pruneAge = 180 * 24 * time.Hour
	// maxFetchPosts caps the posts stored from one fetch, so a hostile feed cannot fill the disk.
	maxFetchPosts = 200
	// maxPosts caps a follow's stored posts, whatever their age, besides those of the latest fetch.
	maxPosts = 500
)

// ErrStale means a FetchResult was for a feed URL or tier the follow no longer has.
var ErrStale = errors.New("the follow's feed URL or tier changed during the fetch")

// FetchResult is the outcome of one fetch, written by RecordFetch.
type FetchResult struct {
	FollowID int64
	// FeedURL and Importance are the follow's when the fetch started: a result
	// for a follow edited since is refused with ErrStale. An empty FeedURL
	// skips the check.
	FeedURL     string
	Importance  model.Importance
	FetchedAt   time.Time
	NextFetchAt time.Time
	Err         string // non-empty marks a failure: sets LastError, increments ErrorCount
	NotModified bool   // success without a body: only fetch state changes

	// applied on success only
	ETag         string
	LastModified string
	FeedTitle    string // ignored when empty
	Description  string // ignored when empty
	SiteURL      string // replaces Follow.URL only when Follow.URL is empty or still the feed URL
	PhotoURL     string // ignored when empty
	Posts        []model.Post
}

// RecordFetch applies a FetchResult in one transaction. On success it upserts
// posts by (follow, GUID): new posts get FirstSeenAt = FetchedAt and, when the
// feed had no date, PublishedAt = FirstSeenAt; existing posts keep FirstSeenAt
// and keep their stored PublishedAt when the new one is zero. PublishedAt later
// than FetchedAt is clamped to FetchedAt. It then recomputes LastPostAt, clears
// LastError/ErrorCount and prunes posts beyond the newest 50 that are older than
// 180 days.
//
// Details: an existing post also keeps its stored PublishedAt when the new one
// is in the future, so a future-dated post is not bumped to "now" on every
// fetch, and when the new one is later but the post has no UpdatedAt: like
// Fraidycat, that is an edit (a CMS re-stamping pubDate), so the new date
// becomes UpdatedAt instead. Posts the feed no longer lists are deleted when
// they are newer than the oldest dated post it does list (a deleted or
// re-uploaded video), as Fraidycat does. Only the first 200 posts of a fetch
// are stored, pruning never deletes a post the fetch listed (it would come
// back as new), and beyond those a follow keeps at most 500 posts whatever
// their age. A GUID repeated within one fetch keeps its first occurrence; a
// post without GUID falls back to its URL and is dropped when it has neither.
// On NotModified, ETag and LastModified are only replaced when non-empty. A
// zero FetchedAt means now. Returns ErrNotFound when the follow does not exist
// (it may have been deleted while being fetched) and ErrStale, storing
// nothing, when FeedURL is set and the follow's feed URL or tier has changed
// since.
func (s *Store) RecordFetch(ctx context.Context, r FetchResult) error {
	fetched := r.FetchedAt
	if fetched.IsZero() {
		fetched = s.now()
	}
	fetchedMS, nextMS := toMS(fetched), toMS(r.NextFetchAt)
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if r.FeedURL != "" {
			var feedURL string
			var imp model.Importance
			err := tx.QueryRowContext(ctx, `SELECT feed_url, importance FROM follows WHERE id = ?`, r.FollowID).
				Scan(&feedURL, &imp)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				return ErrNotFound
			case err != nil:
				return err
			case feedURL != r.FeedURL, model.NormalizeImportance(int(imp)) != model.NormalizeImportance(int(r.Importance)):
				// the old feed's validators and posts must not land on a new URL, nor the old tier's schedule on a new tier
				return ErrStale
			}
		}
		switch {
		case r.Err != "":
			res, err := tx.ExecContext(ctx, `
				UPDATE follows
				SET last_error = ?, error_count = error_count + 1, last_fetched_at = ?, next_fetch_at = ?
				WHERE id = ?`, r.Err, fetchedMS, nextMS, r.FollowID)
			return affectedOne(res, err)
		case r.NotModified:
			res, err := tx.ExecContext(ctx, `
				UPDATE follows
				SET etag = iif(?1 = '', etag, ?1), last_modified = iif(?2 = '', last_modified, ?2),
					last_error = '', error_count = 0, last_fetched_at = ?3, next_fetch_at = ?4
				WHERE id = ?5`, r.ETag, r.LastModified, fetchedMS, nextMS, r.FollowID)
			return affectedOne(res, err)
		}

		// a site URL equal to the feed URL is the placeholder opml import uses when htmlUrl is missing
		res, err := tx.ExecContext(ctx, `
			UPDATE follows
			SET etag = ?1, last_modified = ?2,
				feed_title = iif(?3 = '', feed_title, ?3),
				description = iif(?4 = '', description, ?4),
				url = iif(?5 != '' AND (url = '' OR url = feed_url), ?5, url),
				photo_url = iif(?6 = '', photo_url, ?6),
				last_error = '', error_count = 0, last_fetched_at = ?7, next_fetch_at = ?8
			WHERE id = ?9`,
			r.ETag, r.LastModified, strings.TrimSpace(r.FeedTitle), strings.TrimSpace(r.Description),
			strings.TrimSpace(r.SiteURL), strings.TrimSpace(r.PhotoURL), fetchedMS, nextMS, r.FollowID)
		if err := affectedOne(res, err); err != nil {
			return err
		}
		posts := uniquePosts(r.Posts)
		if len(posts) > maxFetchPosts {
			posts = posts[:maxFetchPosts]
		}
		listed, err := jsonGUIDs(posts)
		if err != nil {
			return err
		}
		if err := upsertPosts(ctx, tx, r.FollowID, fetchedMS, posts, listed); err != nil {
			return err
		}
		if err := dropUnlisted(ctx, tx, r.FollowID, posts, listed); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM posts
			WHERE follow_id = ?1 AND guid NOT IN (SELECT value FROM json_each(?2)) AND (
				(published_at < ?3 AND id NOT IN (
					SELECT id FROM posts WHERE follow_id = ?1 ORDER BY published_at DESC, id DESC LIMIT ?4
				))
				OR id NOT IN (
					SELECT id FROM posts WHERE follow_id = ?1 ORDER BY published_at DESC, id DESC LIMIT ?5
				)
			)`, r.FollowID, listed, fetchedMS-pruneAge.Milliseconds(), keepPosts, maxPosts); err != nil {
			return fmt.Errorf("prune posts: %w", err)
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE follows
			SET last_post_at = (SELECT coalesce(max(published_at), 0) FROM posts WHERE follow_id = ?1)
			WHERE id = ?1`, r.FollowID)
		return err
	})
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrStale) {
		return err
	}
	if err != nil {
		return fmt.Errorf("store: record fetch of follow %d: %w", r.FollowID, err)
	}
	return nil
}

// jsonGUIDs encodes the GUIDs of posts as a JSON array for json_each.
func jsonGUIDs(posts []model.Post) (string, error) {
	ids := make([]string, len(posts))
	for i, p := range posts {
		ids[i] = p.GUID
	}
	b, err := json.Marshal(ids)
	return string(b), err
}

// dropUnlisted deletes the follow's posts that the fetched posts (JSON GUIDs
// listed) no longer include although they are newer than the oldest dated
// one. Like Fraidycat, a date means UpdatedAt, else PublishedAt, and a fetch
// needs two posts to span a range.
func dropUnlisted(ctx context.Context, tx *sql.Tx, followID int64, posts []model.Post, listed string) error {
	if len(posts) < 2 {
		return nil
	}
	var oldest int64
	for _, p := range posts {
		at := toMS(p.UpdatedAt)
		if at == 0 {
			at = toMS(p.PublishedAt)
		}
		if at != 0 && (oldest == 0 || at < oldest) {
			oldest = at
		}
	}
	if oldest == 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
		DELETE FROM posts
		WHERE follow_id = ? AND iif(updated_at != 0, updated_at, published_at) > ?
			AND guid NOT IN (SELECT value FROM json_each(?))`, followID, oldest, listed)
	if err != nil {
		return fmt.Errorf("drop unlisted posts: %w", err)
	}
	return nil
}

// upsertSQL stores a post with dates already resolved by upsertPosts. The
// WHERE clause skips rewriting unchanged rows, which is most of them.
const upsertSQL = `
	INSERT INTO posts (follow_id, guid, url, title, author, published_at, updated_at, first_seen_at)
	VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8)
	ON CONFLICT (follow_id, guid) DO UPDATE SET
		url = excluded.url,
		title = excluded.title,
		author = excluded.author,
		updated_at = excluded.updated_at,
		published_at = excluded.published_at
	WHERE (posts.url, posts.title, posts.author, posts.updated_at, posts.published_at)
		!= (excluded.url, excluded.title, excluded.author, excluded.updated_at, excluded.published_at)`

// storedDates is what upsertPosts needs to know about a post already stored.
type storedDates struct {
	published, firstSeen int64
}

// upsertPosts stores posts, deduplicated by uniquePosts; listed holds their GUIDs as JSON.
func upsertPosts(ctx context.Context, tx *sql.Tx, followID, fetchedMS int64, posts []model.Post, listed string) error {
	if len(posts) == 0 {
		return nil
	}
	stored, err := loadStoredDates(ctx, tx, followID, listed)
	if err != nil {
		return fmt.Errorf("upsert posts: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, upsertSQL)
	if err != nil {
		return fmt.Errorf("upsert posts: %w", err)
	}
	defer stmt.Close()
	// oldest first (feeds list newest first), so among equal dates a higher id is newer
	for _, p := range slices.Backward(posts) {
		published, updated := postDates(p, fetchedMS, stored)
		if _, err := stmt.ExecContext(ctx, followID, p.GUID, p.URL, p.Title, p.Author,
			published, updated, fetchedMS); err != nil {
			return fmt.Errorf("upsert post %q: %w", p.GUID, err)
		}
	}
	return nil
}

// postDates resolves the PublishedAt and UpdatedAt (unix ms) to store for p.
func postDates(p model.Post, fetchedMS int64, stored map[string]storedDates) (published, updated int64) {
	raw, updated := toMS(p.PublishedAt), toMS(p.UpdatedAt)
	feedDate := raw != 0 && raw <= fetchedMS
	published = raw
	if !feedDate {
		published = fetchedMS
	}
	old, ok := stored[p.GUID]
	switch {
	case !ok:
	case !feedDate:
		published = old.published
	case raw > old.published && updated == 0 && old.published != old.firstSeen:
		// a stored date equal to first seen was a stand-in (undated or clamped), anything else came from the feed
		published, updated = old.published, raw
	}
	return published, updated
}

// loadStoredDates returns the stored dates of the follow's posts among the JSON GUIDs.
func loadStoredDates(ctx context.Context, tx *sql.Tx, followID int64, guids string) (map[string]storedDates, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT guid, published_at, first_seen_at FROM posts
		WHERE follow_id = ? AND guid IN (SELECT value FROM json_each(?))`, followID, guids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]storedDates)
	for rows.Next() {
		var guid string
		var d storedDates
		if err := rows.Scan(&guid, &d.published, &d.firstSeen); err != nil {
			return nil, err
		}
		out[guid] = d
	}
	return out, rows.Err()
}

// uniquePosts keys posts by GUID (falling back to URL) and keeps the first of each.
func uniquePosts(posts []model.Post) []model.Post {
	seen := make(map[string]bool, len(posts))
	out := make([]model.Post, 0, len(posts))
	for _, p := range posts {
		if p.GUID == "" {
			p.GUID = p.URL
		}
		if p.GUID == "" || seen[p.GUID] {
			continue
		}
		seen[p.GUID] = true
		out = append(out, p)
	}
	return out
}

const postColumns = `id, follow_id, guid, url, title, author, published_at, updated_at, first_seen_at`

func scanPost(sc scanner) (model.Post, error) {
	var p model.Post
	err := sc.Scan(&p.ID, &p.FollowID, &p.GUID, &p.URL, &p.Title, &p.Author,
		msTime{&p.PublishedAt}, msTime{&p.UpdatedAt}, msTime{&p.FirstSeenAt})
	return p, err
}

// RecentPosts returns a follow's newest posts by PublishedAt.
func (s *Store) RecentPosts(ctx context.Context, followID int64, limit int) ([]model.Post, error) {
	if limit < 1 {
		return nil, nil
	}
	rows, err := s.r.QueryContext(ctx, `
		SELECT `+postColumns+` FROM posts
		WHERE follow_id = ?
		ORDER BY published_at DESC, id DESC
		LIMIT ?`, followID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: recent posts: %w", err)
	}
	defer rows.Close()
	var out []model.Post
	for rows.Next() {
		p, err := scanPost(rows)
		if err != nil {
			return nil, fmt.Errorf("store: recent posts: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: recent posts: %w", err)
	}
	return out, nil
}

// LatestPosts returns the newest perFollow posts of every follow, keyed by follow id.
// Follows without posts are absent; the map is never nil.
func (s *Store) LatestPosts(ctx context.Context, perFollow int) (map[int64][]model.Post, error) {
	out := make(map[int64][]model.Post)
	if perFollow < 1 {
		return out, nil
	}
	rows, err := s.r.QueryContext(ctx, `
		SELECT `+postColumns+` FROM (
			SELECT *, row_number() OVER (PARTITION BY follow_id ORDER BY published_at DESC, id DESC) AS rn
			FROM posts
		)
		WHERE rn <= ?
		ORDER BY follow_id, rn`, perFollow)
	if err != nil {
		return nil, fmt.Errorf("store: latest posts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		p, err := scanPost(rows)
		if err != nil {
			return nil, fmt.Errorf("store: latest posts: %w", err)
		}
		out[p.FollowID] = append(out[p.FollowID], p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: latest posts: %w", err)
	}
	return out, nil
}

// Activity returns the PublishedAt of every post since the given time, keyed by
// follow id. followIDs limits the result; nil means all follows.
// Times are newest first; follows without activity are absent; the map is never nil.
func (s *Store) Activity(ctx context.Context, since time.Time, followIDs []int64) (map[int64][]time.Time, error) {
	out := make(map[int64][]time.Time)
	query := `SELECT follow_id, published_at FROM posts WHERE published_at >= ?`
	args := []any{toMS(since)}
	if followIDs != nil {
		if len(followIDs) == 0 {
			return out, nil
		}
		ids, err := json.Marshal(followIDs)
		if err != nil {
			return nil, fmt.Errorf("store: activity: %w", err)
		}
		// json_each keeps the statement fixed however many ids there are
		query += ` AND follow_id IN (SELECT value FROM json_each(?))`
		args = append(args, string(ids))
	}
	query += ` ORDER BY follow_id, published_at DESC`

	rows, err := s.r.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: activity: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var at time.Time
		if err := rows.Scan(&id, msTime{&at}); err != nil {
			return nil, fmt.Errorf("store: activity: %w", err)
		}
		out[id] = append(out[id], at)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: activity: %w", err)
	}
	return out, nil
}
