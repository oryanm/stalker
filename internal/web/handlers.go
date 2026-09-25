package web

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/oryanm/stalker/internal/discover"
	"github.com/oryanm/stalker/internal/events"
	"github.com/oryanm/stalker/internal/model"
	"github.com/oryanm/stalker/internal/opml"
	"github.com/oryanm/stalker/internal/store"
)

const (
	maxTitleLen   = 300
	maxCandidates = 20
	exportTitle   = "stalker follows"
)

type homePage struct {
	layout
	Welcome  bool
	Tag      string
	Tier     model.Tier
	TierTabs []tierTab
	Rows     []row
	Sort     string
	Self     string // this page's URL, where the sort buttons return to
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	now := s.now()
	follows, err := s.st.ListFollows(ctx)
	if err != nil {
		return err
	}
	if len(follows) == 0 {
		s.page(w, r, http.StatusOK, "home", homePage{layout: layout{AddURL: "/add"}, Welcome: true})
		return nil
	}

	q := r.URL.Query()
	tag := cmp.Or(strings.TrimSpace(q.Get("tag")), defaultTag(follows))
	inTag := followsInTag(follows, tag)
	tier := defaultTier(inTag)
	if q.Has("tier") {
		var ok bool
		if tier, ok = parseTier(q.Get("tier")); !ok {
			return httpError(http.StatusBadRequest, "That is not an importance tier.")
		}
	}

	var visible []model.Follow
	var ids []int64
	for _, f := range inTag {
		if tierOf(f) == tier {
			visible = append(visible, f)
			ids = append(ids, f.ID)
		}
	}
	latest, err := s.st.LatestPosts(ctx, latestPerFollow)
	if err != nil {
		return err
	}
	activity := map[int64][]time.Time{}
	if len(ids) > 0 {
		if activity, err = s.st.Activity(ctx, now.Add(-activityWindow), ids); err != nil {
			return err
		}
	}
	rows := make([]row, len(visible))
	for i, f := range visible {
		rows[i] = row{Follow: f, Posts: latest[f.ID], Activity: activity[f.ID], Fetching: s.fetch.IsFetching(f.ID), Now: now}
	}
	sortBy := s.sortSetting(ctx)
	sortRows(rows, sortBy)

	t := model.TierFor(tier)
	s.page(w, r, http.StatusOK, "home", homePage{
		layout:   layout{Title: tag + " " + t.Name, Tabs: buildTagTabs(follows, tag, now), AddURL: addURL(tag, &tier)},
		Tag:      tag,
		Tier:     t,
		TierTabs: buildTierTabs(inTag, tier),
		Rows:     rows,
		Sort:     sortBy,
		Self:     homeURL(tag, &tier),
	})
	return nil
}

func (s *Server) sortSetting(ctx context.Context) string {
	v, err := s.st.GetSetting(ctx, settingSort, sortRecent)
	if err != nil {
		s.log.Warn("read sort setting", "err", err)
	}
	if !validSort(v) {
		return sortRecent
	}
	return v
}

// loadRow gathers one follow's summary, as listed on the home page.
func (s *Server) loadRow(ctx context.Context, id int64, now time.Time) (row, error) {
	f, err := s.st.GetFollow(ctx, id)
	if err != nil {
		return row{}, err
	}
	posts, err := s.st.RecentPosts(ctx, id, latestPerFollow)
	if err != nil {
		return row{}, err
	}
	activity, err := s.st.Activity(ctx, now.Add(-activityWindow), []int64{id})
	if err != nil {
		return row{}, err
	}
	return row{Follow: f, Posts: posts, Activity: activity[id], Fetching: s.fetch.IsFetching(id), Now: now}, nil
}

// followFromPath loads the follow named by the {id} path segment.
func (s *Server) followFromPath(r *http.Request) (model.Follow, error) {
	id, err := parseID(r)
	if err != nil {
		return model.Follow{}, err
	}
	f, err := s.st.GetFollow(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return model.Follow{}, errNoFollows
	}
	return f, err
}

func parseID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return 0, errBadID
	}
	return id, nil
}

type postsPage struct {
	layout
	Follow   model.Follow
	Posts    []model.Post
	Fragment bool
	Back     string
	Now      time.Time
}

func (s *Server) posts(w http.ResponseWriter, r *http.Request) error {
	f, err := s.followFromPath(r)
	if err != nil {
		return err
	}
	posts, err := s.st.RecentPosts(r.Context(), f.ID, recentPostLimit)
	if err != nil {
		return err
	}
	data := postsPage{Follow: f, Posts: posts, Fragment: isFragment(r), Back: followLocation(f), Now: s.now()}
	if data.Fragment {
		s.fragment(w, http.StatusOK, "posts", data)
		return nil
	}
	data.layout = s.newLayout(r.Context(), f.DisplayTitle(), f.DisplayTags()[0])
	s.page(w, r, http.StatusOK, "posts", data)
	return nil
}

// addForm holds the fields shared by the add form and the candidate chooser.
type addForm struct {
	URL   string
	Title string
	Tags  string
	Tier  model.Importance
}

type addPage struct {
	layout
	Form     addForm
	Error    string
	Existing string // location of the follow a duplicate points at
}

func (s *Server) showAdd(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	form := addForm{URL: strings.TrimSpace(q.Get("url")), Tier: model.Frequent}
	if tag := strings.TrimSpace(q.Get("tag")); tag != model.HomeTag {
		form.Tags = tag
	}
	if t, ok := parseTier(q.Get("tier")); ok {
		form.Tier = t
	}
	s.renderAdd(w, r, http.StatusOK, addPage{Form: form})
	return nil
}

func (s *Server) renderAdd(w http.ResponseWriter, r *http.Request, status int, data addPage) {
	data.layout = s.newLayout(r.Context(), "Add a follow", firstTag(data.Form.Tags))
	s.page(w, r, status, "add", data)
}

func firstTag(tags string) string {
	if t := parseTags(tags); len(t) > 0 {
		return t[0]
	}
	return model.HomeTag
}

// readAddForm parses the fields of the add form and the chooser. problem is a
// user-facing validation message; err means the form could not be read at all.
// The URL is only required when requireURL is set.
func readAddForm(w http.ResponseWriter, r *http.Request, requireURL bool) (form addForm, problem string, err error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		return addForm{}, "", errUnreadableForm
	}
	form = addForm{
		URL:   strings.TrimSpace(r.PostForm.Get("url")),
		Title: strings.TrimSpace(r.PostForm.Get("title")),
		Tags:  strings.TrimSpace(r.PostForm.Get("tags")),
		Tier:  model.Frequent,
	}
	tier, ok := parseTier(r.PostForm.Get("tier"))
	if ok {
		form.Tier = tier
	}
	switch {
	case requireURL && form.URL == "":
		problem = "Enter the address of a site, feed or channel."
	case len(form.URL) > maxURLLen:
		problem = "That URL is too long."
	case utf8.RuneCountInString(form.Title) > maxTitleLen:
		problem = fmt.Sprintf("Titles are limited to %d characters.", maxTitleLen)
	case !ok:
		problem = "Pick an importance tier."
	}
	return form, problem, nil
}

func (s *Server) add(w http.ResponseWriter, r *http.Request) error {
	form, problem, err := readAddForm(w, r, true)
	if err != nil {
		return err
	}
	if problem != "" {
		s.renderAdd(w, r, http.StatusUnprocessableEntity, addPage{Form: form, Error: problem})
		return nil
	}

	ctx, cancel := context.WithTimeout(r.Context(), discoverTimeout)
	candidates, err := s.disc.Discover(ctx, form.URL)
	cancel()
	if err != nil && r.Context().Err() != nil {
		return err
	}
	candidates = validCandidates(candidates)
	if err != nil || len(candidates) == 0 {
		s.renderAdd(w, r, http.StatusUnprocessableEntity, addPage{Form: form, Error: discoverMessage(err)})
		return nil
	}
	if len(candidates) > 1 {
		s.renderChoose(w, r, http.StatusOK, choosePage{
			Form: form, Candidates: candidates, Selected: map[string]bool{candidates[0].FeedURL: true},
		})
		return nil
	}

	f := newFollow(candidates[0], form)
	err = s.st.CreateFollow(r.Context(), &f)
	if errors.Is(err, store.ErrDuplicate) {
		data := addPage{Form: form, Error: "You already follow this feed."}
		if existing, err := s.st.GetFollowByFeedURL(r.Context(), f.FeedURL); err == nil {
			data.Existing = followLocation(existing)
		}
		s.renderAdd(w, r, http.StatusUnprocessableEntity, data)
		return nil
	}
	if err != nil {
		return err
	}
	s.ev.Publish(events.Event{Kind: events.FollowsChanged})
	s.fetchBounded(r.Context(), f.ID)
	http.Redirect(w, r, followLocation(f), http.StatusSeeOther)
	return nil
}

// discoverMessage explains a failed discovery to the user.
func discoverMessage(err error) string {
	switch {
	case err == nil:
		return sentence(discover.ErrNoFeed.Error()) + "."
	case errors.Is(err, context.DeadlineExceeded):
		return "Looking for a feed took too long. Try the feed's own URL."
	case errors.Is(err, discover.ErrInvalidURL):
		return "That does not look like a web address. Try something like https://example.com."
	default:
		return sentence(err.Error()) + "."
	}
}

// validCandidates drops candidates whose URLs are not http(s), deduplicating
// by feed URL and capping the list.
func validCandidates(cs []discover.Candidate) []discover.Candidate {
	var out []discover.Candidate
	seen := map[string]bool{}
	for _, c := range cs {
		c.FeedURL = strings.TrimSpace(c.FeedURL)
		if safeURL(c.FeedURL) == "" || seen[c.FeedURL] || len(out) == maxCandidates {
			continue
		}
		seen[c.FeedURL] = true
		if safeURL(c.SiteURL) == "" {
			c.SiteURL = ""
		}
		c.Title = strings.TrimSpace(c.Title)
		out = append(out, c)
	}
	return out
}

func newFollow(c discover.Candidate, form addForm) model.Follow {
	return model.Follow{
		URL:        c.SiteURL, // empty is filled from the feed on its first fetch
		FeedURL:    c.FeedURL,
		Title:      form.Title,
		FeedTitle:  c.Title,
		Importance: form.Tier,
		Tags:       parseTags(form.Tags),
	}
}

// fetchBounded fetches the follows, waiting at most s.fetchWait. Fetches keep
// running after the wait (or the request) ends, and report through SSE.
func (s *Server) fetchBounded(ctx context.Context, ids ...int64) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchLimit)
		defer cancel()
		var wg sync.WaitGroup
		for _, id := range ids {
			wg.Go(func() {
				// the error is recorded on the follow and shown on its row
				if err := s.fetch.FetchNow(fctx, id); err != nil {
					s.log.Debug("fetch failed", "follow", id, "err", err)
				}
			})
		}
		wg.Wait()
	}()
	wait := time.NewTimer(s.fetchWait)
	defer wait.Stop()
	select {
	case <-done:
	case <-wait.C:
	case <-ctx.Done():
	}
}

type choosePage struct {
	layout
	Form       addForm
	Candidates []discover.Candidate
	Selected   map[string]bool
	Error      string
}

func (s *Server) renderChoose(w http.ResponseWriter, r *http.Request, status int, data choosePage) {
	data.layout = s.newLayout(r.Context(), "Choose feeds", firstTag(data.Form.Tags))
	s.page(w, r, status, "choose", data)
}

func (s *Server) choose(w http.ResponseWriter, r *http.Request) error {
	form, problem, err := readAddForm(w, r, false)
	if err != nil {
		return err
	}
	// the chooser echoes every candidate back in parallel hidden fields
	feeds, sites, titles := r.PostForm["candidate"], r.PostForm["candidate_site"], r.PostForm["candidate_title"]
	if len(feeds) != len(sites) || len(feeds) != len(titles) || len(feeds) > maxCandidates {
		return httpError(http.StatusBadRequest, "The list of feeds was incomplete. Please add the URL again.")
	}
	var candidates []discover.Candidate
	for i := range feeds {
		candidates = append(candidates, discover.Candidate{FeedURL: feeds[i], SiteURL: sites[i], Title: titles[i]})
	}
	if candidates = validCandidates(candidates); len(candidates) == 0 {
		return httpError(http.StatusBadRequest, "There were no feeds to choose from. Please add the URL again.")
	}

	selected := map[string]bool{}
	var picked []discover.Candidate
	for _, feed := range r.PostForm["feed"] {
		i := slices.IndexFunc(candidates, func(c discover.Candidate) bool { return c.FeedURL == feed })
		if i >= 0 && !selected[feed] {
			selected[feed] = true
			picked = append(picked, candidates[i])
		}
	}
	if problem == "" && len(picked) == 0 {
		problem = "Pick at least one feed."
	}
	if problem != "" {
		data := choosePage{Form: form, Candidates: candidates, Selected: selected, Error: problem}
		s.renderChoose(w, r, http.StatusUnprocessableEntity, data)
		return nil
	}

	var created []model.Follow
	for _, c := range picked {
		f := newFollow(c, form)
		if len(picked) > 1 {
			// one title cannot name several feeds
			f.Title = ""
		}
		err := s.st.CreateFollow(r.Context(), &f)
		if errors.Is(err, store.ErrDuplicate) {
			continue
		}
		if err != nil {
			return err
		}
		created = append(created, f)
	}
	if len(created) == 0 {
		// everything picked is followed already: show it where it is
		existing, err := s.st.GetFollowByFeedURL(r.Context(), picked[0].FeedURL)
		if err != nil {
			return err
		}
		http.Redirect(w, r, followLocation(existing), http.StatusSeeOther)
		return nil
	}
	s.ev.Publish(events.Event{Kind: events.FollowsChanged})
	ids := make([]int64, len(created))
	for i, f := range created {
		ids[i] = f.ID
	}
	s.fetchBounded(r.Context(), ids...)
	http.Redirect(w, r, followLocation(created[0]), http.StatusSeeOther)
	return nil
}

type editForm struct {
	Title   string
	URL     string
	FeedURL string
	Tags    string
	Tier    model.Importance
}

type editPage struct {
	layout
	Follow   model.Follow
	Form     editForm
	Error    string
	Fragment bool
	Back     string
}

func (s *Server) showEdit(w http.ResponseWriter, r *http.Request) error {
	f, err := s.followFromPath(r)
	if err != nil {
		return err
	}
	data := editPage{
		Follow: f,
		Form: editForm{
			Title: f.Title, URL: f.URL, FeedURL: f.FeedURL, Tags: strings.Join(f.Tags, " "), Tier: tierOf(f),
		},
		Fragment: isFragment(r),
		Back:     followLocation(f),
	}
	if data.Fragment {
		s.fragment(w, http.StatusOK, "editform", data)
		return nil
	}
	s.renderEdit(w, r, http.StatusOK, data)
	return nil
}

func (s *Server) renderEdit(w http.ResponseWriter, r *http.Request, status int, data editPage) {
	data.layout = s.newLayout(r.Context(), "Edit "+data.Follow.DisplayTitle(), data.Follow.DisplayTags()[0])
	s.page(w, r, status, "edit", data)
}

func (s *Server) save(w http.ResponseWriter, r *http.Request) error {
	f, err := s.followFromPath(r)
	if err != nil {
		return err
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		return errUnreadableForm
	}
	form := editForm{
		Title:   strings.TrimSpace(r.PostForm.Get("title")),
		URL:     strings.TrimSpace(r.PostForm.Get("url")),
		FeedURL: strings.TrimSpace(r.PostForm.Get("feed_url")),
		Tags:    strings.TrimSpace(r.PostForm.Get("tags")),
		Tier:    tierOf(f),
	}
	tier, tierOK := parseTier(r.PostForm.Get("tier"))
	if tierOK {
		form.Tier = tier
	}
	var problem string
	if utf8.RuneCountInString(form.Title) > maxTitleLen {
		problem = fmt.Sprintf("Titles are limited to %d characters.", maxTitleLen)
	} else if _, err := parseHTTPURL(form.FeedURL); err != nil {
		problem = "Feed URL: " + sentence(err.Error()) + "."
	} else if _, err := parseHTTPURL(form.URL); form.URL != "" && err != nil {
		problem = "Site URL: " + sentence(err.Error()) + "."
	} else if !tierOK {
		problem = "Pick an importance tier."
	}
	data := editPage{Follow: f, Form: form, Back: followLocation(f)}
	if problem != "" {
		data.Error = problem
		s.renderEdit(w, r, http.StatusUnprocessableEntity, data)
		return nil
	}

	updated := f
	updated.Title = form.Title
	updated.URL = form.URL
	updated.FeedURL = form.FeedURL
	updated.Tags = parseTags(form.Tags)
	updated.Importance = tier
	updated.EditedAt = time.Time{}
	switch err := s.st.UpdateFollowSettings(r.Context(), updated); {
	case errors.Is(err, store.ErrDuplicate):
		data.Error = "Another follow already uses this feed URL."
		s.renderEdit(w, r, http.StatusUnprocessableEntity, data)
		return nil
	case errors.Is(err, store.ErrNotFound):
		return errNoFollows
	case err != nil:
		return err
	}
	if updated.FeedURL != f.FeedURL || tierOf(updated) < tierOf(f) {
		// the store scheduled a moved feed or a more important tier for now; don't wait for the next tick
		s.fetch.Kick()
	}
	s.ev.Publish(events.Event{Kind: events.FollowsChanged})
	// re-read for the tags as the store normalized them, which decide the location
	if updated, err = s.st.GetFollow(r.Context(), f.ID); err != nil {
		return err
	}
	http.Redirect(w, r, followLocation(updated), http.StatusSeeOther)
	return nil
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request) error {
	id, err := parseID(r)
	if err != nil {
		return err
	}
	f, err := s.st.GetFollow(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		// already gone, e.g. a repeated submit
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.st.DeleteFollow(r.Context(), id); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	s.ev.Publish(events.Event{Kind: events.FollowsChanged})
	http.Redirect(w, r, s.afterDelete(r.Context(), f), http.StatusSeeOther)
	return nil
}

// afterDelete is the list the deleted follow was in, widened to its tag or
// the home page when that list is now empty.
func (s *Server) afterDelete(ctx context.Context, f model.Follow) string {
	tag, tier := f.DisplayTags()[0], tierOf(f)
	follows, err := s.st.ListFollows(ctx)
	if err != nil {
		return homeURL(tag, &tier)
	}
	inTag := followsInTag(follows, tag)
	switch {
	case len(inTag) == 0:
		return "/"
	case !slices.ContainsFunc(inTag, func(g model.Follow) bool { return tierOf(g) == tier }):
		return homeURL(tag, nil)
	}
	return homeURL(tag, &tier)
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request) error {
	f, err := s.followFromPath(r)
	if err != nil {
		return err
	}
	s.fetchBounded(r.Context(), f.ID)
	if !isFragment(r) {
		http.Redirect(w, r, followLocation(f), http.StatusSeeOther)
		return nil
	}
	rw, err := s.loadRow(r.Context(), f.ID, s.now())
	if err != nil {
		return err
	}
	s.fragment(w, http.StatusOK, "summary", rw)
	return nil
}

type tierCount struct {
	Tier  model.Tier
	Count int
}

type settingsPage struct {
	layout
	Sort        string
	Total       int
	TierCounts  []tierCount
	Failing     []model.Follow
	Flash       string
	ImportError string
	Now         time.Time
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) error {
	data, err := s.settingsData(r.Context())
	if err != nil {
		return err
	}
	q := r.URL.Query()
	if q.Has("imported") {
		added, _ := strconv.Atoi(q.Get("imported"))
		skipped, _ := strconv.Atoi(q.Get("skipped"))
		data.Flash = fmt.Sprintf("Imported %d, skipped %d.", added, skipped)
	} else if q.Has("saved") {
		data.Flash = "Saved."
	}
	s.page(w, r, http.StatusOK, "settings", data)
	return nil
}

func (s *Server) settingsData(ctx context.Context) (settingsPage, error) {
	follows, err := s.st.ListFollows(ctx)
	if err != nil {
		return settingsPage{}, err
	}
	now := s.now()
	data := settingsPage{
		layout: layout{Title: "Settings", AddURL: "/add"},
		Sort:   s.sortSetting(ctx),
		Total:  len(follows),
		Now:    now,
	}
	if len(follows) > 0 {
		data.Tabs = buildTagTabs(follows, "", now)
	}
	counts := map[model.Importance]int{}
	for _, f := range follows {
		counts[tierOf(f)]++
		if f.LastError != "" {
			data.Failing = append(data.Failing, f)
		}
	}
	for _, t := range model.Tiers {
		data.TierCounts = append(data.TierCounts, tierCount{t, counts[t.Importance]})
	}
	slices.SortFunc(data.Failing, compareTitles)
	return data, nil
}

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		return errUnreadableForm
	}
	sortBy := r.PostForm.Get("sort")
	if !validSort(sortBy) {
		return httpError(http.StatusBadRequest, "That is not a sort order.")
	}
	if err := s.st.SetSetting(r.Context(), settingSort, sortBy); err != nil {
		return err
	}
	next := r.PostForm.Get("next")
	if !isLocalPath(next) {
		next = "/settings?saved=1"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
	return nil
}

// isLocalPath accepts only same-site paths, so "next" cannot redirect elsewhere.
// Browsers read a backslash as a slash and http.Redirect cleans the path
// ("/./\evil.example" becomes "/\evil.example"), so backslashes and control
// characters are refused wherever they are.
func isLocalPath(p string) bool {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") ||
		strings.ContainsFunc(p, func(r rune) bool { return r == '\\' || unicode.IsControl(r) }) {
		return false
	}
	u, err := url.Parse(p)
	return err == nil && u.Scheme == "" && u.Host == ""
}

func (s *Server) importOPML(w http.ResponseWriter, r *http.Request) error {
	// room for the multipart framing around a file of the maximum size
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+64<<10)
	data, err := readUpload(r, "file")
	if err != nil {
		return err
	}
	entries, err := opml.Parse(bytes.NewReader(data))
	if err != nil {
		page, derr := s.settingsData(r.Context())
		if derr != nil {
			return derr
		}
		page.ImportError = "That file could not be imported: " + err.Error()
		s.page(w, r, http.StatusUnprocessableEntity, "settings", page)
		return nil
	}
	now := s.now()
	follows := make([]model.Follow, len(entries))
	for i, e := range entries {
		follows[i] = e.ToFollow(now)
	}
	added, skipped, err := s.st.ImportFollows(r.Context(), follows)
	if err != nil {
		return err
	}
	s.fetch.Kick()
	s.ev.Publish(events.Event{Kind: events.FollowsChanged})
	http.Redirect(w, r, fmt.Sprintf("/settings?imported=%d&skipped=%d", added, skipped), http.StatusSeeOther)
	return nil
}

// readUpload streams the named file part into memory, capped at maxUploadBytes.
func readUpload(r *http.Request, field string) ([]byte, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, httpError(http.StatusBadRequest, "Choose an OPML file to import.")
	}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return nil, httpError(http.StatusBadRequest, "Choose an OPML file to import.")
		}
		if err != nil {
			if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
				return nil, err
			}
			return nil, httpError(http.StatusBadRequest, "The upload could not be read.")
		}
		if part.FormName() != field {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(part, maxUploadBytes+1))
		if err != nil {
			if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
				return nil, err
			}
			return nil, httpError(http.StatusBadRequest, "The upload could not be read.")
		}
		if len(data) > maxUploadBytes {
			return nil, httpError(http.StatusRequestEntityTooLarge, "OPML files are limited to 5 MiB.")
		}
		return data, nil
	}
}

func (s *Server) exportOPML(w http.ResponseWriter, r *http.Request) error {
	follows, err := s.st.ListFollows(r.Context())
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := opml.Write(&buf, exportTitle, follows); err != nil {
		return err
	}
	h := w.Header()
	h.Set("Content-Type", "text/x-opml; charset=utf-8")
	h.Set("Content-Disposition", `attachment; filename="stalker.opml"`)
	h.Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
	return nil
}
