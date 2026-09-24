// Package pipeline runs one site end to end: discover, fetch what's new or
// recent, extract, normalize and store (§3.1, §3.2).
package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"

	"rss-er/internal/config"
	"rss-er/internal/discovery"
	"rss-er/internal/extract"
	"rss-er/internal/fetch"
	"rss-er/internal/normalize"
	"rss-er/internal/store"
)

// Stats summarize one run; they are logged as one line per run (§8, Observability).
type Stats struct {
	Discovered  int           `json:"discovered"`
	Fetched     int           `json:"fetched"`
	NotModified int           `json:"not_modified"`
	New         int           `json:"new_items"`
	Updated     int           `json:"updated_items"`
	Unchanged   int           `json:"unchanged"`
	Errors      int           `json:"errors"`
	NoContent   int           `json:"no_content"`
	NoDate      int           `json:"no_date"`
	Duration    time.Duration `json:"duration"`
}

// Changed reports whether the run added or edited any item.
func (s Stats) Changed() bool { return s.New+s.Updated > 0 }

// healthThreshold is the share of fetched pages missing content or a date
// above which a run warns that extraction is probably broken (§8).
const healthThreshold = 0.5

// SummaryChars is the length of a summary made from the body (§6.8).
const SummaryChars = 300

// Runner holds what every site run needs.
type Runner struct {
	Store *store.Store
	Log   *slog.Logger
	Now   func() time.Time // replaceable in tests

	// Stop, when closed, ends a run after the page in flight (§8.1, SIGTERM).
	// The run returns ErrStopped and records nothing, so the site runs again
	// at the next start.
	Stop <-chan struct{}
}

// ErrStopped is returned by a run ended early through Runner.Stop.
var ErrStopped = errors.New("run stopped")

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

type target struct {
	cand  discovery.Candidate
	known bool
}

// Run performs one run for site. A discovery failure aborts the run and
// leaves stored items untouched, so the feed keeps its last good content.
func (r *Runner) Run(ctx context.Context, site *config.Site, f fetch.Fetcher) (Stats, error) {
	start := r.now()
	log := r.Log.With("site", site.ID)
	var st Stats

	state, err := r.Store.SiteState(ctx, site.ID)
	if err != nil {
		return st, err
	}
	state.LastRun = start
	fail := func(err error) (Stats, error) {
		state.LastError = err.Error()
		if perr := r.Store.PutSiteState(ctx, site.ID, state); perr != nil {
			err = errors.Join(err, perr)
		}
		st.Duration = r.now().Sub(start)
		return st, err
	}

	cands, err := discovery.Discover(ctx, f, site)
	if err != nil {
		return fail(err)
	}
	st.Discovered = len(cands)
	if len(cands) == 0 {
		return fail(fmt.Errorf("discovery returned no URLs"))
	}

	known, err := r.Store.KnownURLs(ctx, site.ID)
	if err != nil {
		return fail(err)
	}
	targets := plan(site, cands, known, start)
	log.Debug("planned run", "discovered", len(cands), "known", len(known), "fetching", len(targets))

	for _, t := range targets {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		select {
		case <-r.Stop:
			st.Duration = r.now().Sub(start)
			return st, ErrStopped
		default:
		}
		if err := r.process(ctx, site, f, t, start, &st, log); err != nil {
			st.Errors++
			log.Warn("item failed", "url", t.cand.URL, "err", err)
		}
	}

	discovered := make(map[string]bool, len(cands))
	for _, c := range cands {
		discovered[c.URL] = true
	}
	if err := r.Store.SetMissing(ctx, site.ID, discovered, start); err != nil {
		return fail(err)
	}

	fetched := st.Fetched - st.NotModified
	if fetched >= 2 {
		if bad := st.NoContent + st.NoDate; float64(bad) > healthThreshold*float64(fetched) {
			log.Warn("extraction health: most fetched pages are missing content or a date; a selector may be broken",
				"fetched", fetched, "no_content", st.NoContent, "no_date", st.NoDate)
		}
	}

	state.LastSuccess, state.LastError = start, ""
	if st.Errors > 0 {
		state.LastError = fmt.Sprintf("%d of %d fetches failed", st.Errors, len(targets))
	}
	if st.Changed() {
		state.LastChange = start
	}
	if err := r.Store.PutSiteState(ctx, site.ID, state); err != nil {
		return st, err
	}
	st.Duration = r.now().Sub(start)
	return st, nil
}

// plan picks what to fetch (§3.2): every new URL, known items published
// within the refresh window, and (with trust_lastmod) items whose sitemap
// lastmod moved past their last fetch. When discovery dates the new URLs, a
// backfill fetches only the newest max_items of them.
func plan(site *config.Site, cands []discovery.Candidate, known map[string]store.Known, now time.Time) []target {
	var fresh []discovery.Candidate
	var refresh []target
	trustLastmod := false
	for _, d := range site.Discovery {
		trustLastmod = trustLastmod || d.TrustLastmod
	}
	loc := site.Schedule.Location

	for _, c := range cands {
		k, ok := known[c.URL]
		if !ok {
			fresh = append(fresh, c)
			continue
		}
		recent := now.Sub(k.Published) <= site.Schedule.RefreshWindow.D
		moved := false
		if trustLastmod && c.Hints["lastmod"] != "" {
			if d, err := normalize.ParseDate(c.Hints["lastmod"], nil, loc); err == nil && d.Time.After(k.LastFetched) {
				moved = true
			}
		}
		if recent || moved {
			refresh = append(refresh, target{cand: c, known: true})
		}
	}

	fresh = newestOnly(fresh, known, site.Schedule.MaxItems, now, loc)

	out := make([]target, 0, len(fresh)+len(refresh))
	for _, c := range fresh {
		out = append(out, target{cand: c})
	}
	return append(out, refresh...)
}

// newestOnly keeps the new candidates that would rank in the feed's top
// maxItems alongside the stored items. It needs a published hint on every new
// candidate; without one (a sitemap, say) every new candidate is fetched, since
// fetching is the only way to learn its date. Date-only hints are placed as
// publishedAt would place them, so a post dated today ranks at now, not midnight.
func newestOnly(fresh []discovery.Candidate, known map[string]store.Known, maxItems int, now time.Time, loc *time.Location) []discovery.Candidate {
	type ranked struct {
		at   time.Time
		cand *discovery.Candidate // nil for a stored item
	}
	all := make([]ranked, 0, len(fresh)+len(known))
	for i := range fresh {
		d, err := normalize.ParseDate(fresh[i].Hints["published"], nil, loc)
		if err != nil {
			return fresh
		}
		at := d.Time
		if d.DateOnly {
			at, _ = dateOnlyAt(d.Time, now, fresh[i].Order, loc)
		}
		all = append(all, ranked{at: at, cand: &fresh[i]})
	}
	for _, k := range known {
		all = append(all, ranked{at: k.Published})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].at.After(all[j].at) })

	var out []discovery.Candidate
	for _, r := range all[:min(maxItems, len(all))] {
		if r.cand != nil {
			out = append(out, *r.cand)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	return out
}

// process fetches, extracts, normalizes and stores one candidate.
func (r *Runner) process(ctx context.Context, site *config.Site, f fetch.Fetcher, t target, now time.Time, st *Stats, log *slog.Logger) error {
	hh := hintsHash(site, t.cand.Hints)
	req := fetch.Request{URL: t.cand.URL}
	if t.known {
		// Conditional GET only for stored items: a 304 for an unstored URL
		// would leave nothing to extract. And only when discovery's hints are
		// the ones the item was built from: a feed that corrects a date or
		// title changes the item even when the page itself is unchanged.
		v, err := r.Store.Validators(ctx, site.ID, t.cand.URL)
		if err != nil {
			return err
		}
		if v.HintsHash == hh {
			req.ETag, req.LastModified = v.ETag, v.LastModified
		}
	}
	resp, err := f.Fetch(ctx, req)
	if err != nil {
		return err
	}
	st.Fetched++
	if err := r.apply(ctx, site, t, resp, now, st, log); err != nil {
		return err
	}
	// Saved only now: had extraction or storage failed, the next run must
	// fetch in full rather than get a 304 for content that was never stored.
	v := store.Validators{ETag: resp.ETag, LastModified: resp.LastModified, HintsHash: hh}
	return r.Store.PutValidators(ctx, site.ID, t.cand.URL, v, resp.Status, now)
}

// hintsHash identifies the hints extraction can read: those named by the
// site's listing: sources. Others (a sitemap lastmod that changes on every
// build, say) can't change the item, so they mustn't defeat conditional GET.
func hintsHash(site *config.Site, h map[string]string) string {
	it := &site.Item
	used := map[string]bool{}
	for _, srcs := range [][]config.Source{it.Canonical, it.Title, it.Summary, it.Author, it.Image,
		it.Categories, it.Published.Sources, it.Updated.Sources} {
		for _, s := range srcs {
			if s.Kind == config.SourceListing {
				used[s.Expr] = true
			}
		}
	}
	sum := sha256.New()
	for _, k := range slices.Sorted(maps.Keys(used)) {
		fmt.Fprintf(sum, "%s\x00%s\x00", k, h[k])
	}
	return hex.EncodeToString(sum.Sum(nil)[:16])
}

// apply turns one fetched response into a new, updated or unchanged item.
func (r *Runner) apply(ctx context.Context, site *config.Site, t target, resp *fetch.Response, now time.Time, st *Stats, log *slog.Logger) error {
	existing, err := r.Store.ItemByURL(ctx, site.ID, t.cand.URL)
	if err != nil {
		return err
	}
	if resp.NotModified {
		st.NotModified++
		if existing != nil {
			return r.Store.TouchFetched(ctx, site.ID, existing.GUID, now)
		}
		return nil
	}

	res, err := extract.Extract(site, resp.URL, resp.Body, t.cand.Hints)
	if err != nil {
		return err
	}
	for _, w := range res.Warnings {
		log.Debug("extraction warning", "url", t.cand.URL, "warning", w)
	}
	if res.Published.IsZero() {
		st.NoDate++
	}
	if res.Content == "" {
		st.NoContent++
		return fmt.Errorf("no content extracted; not stored")
	}

	it, err := build(site, res)
	if err != nil {
		return err
	}
	it.URL, it.LastFetched = t.cand.URL, now

	if existing == nil && site.Item.GUID == config.GUIDPermalink {
		// The same article under a new discovered URL (say, a trailing-slash change).
		if existing, err = r.Store.ItemByGUID(ctx, site.ID, it.Canonical); err != nil {
			return err
		}
	}

	if existing == nil {
		it.FirstSeen = now
		it.GUID = guidFor(site, it.Canonical, t.cand.URL)
		it.Published, it.PublishedSource = publishedAt(res, now, t.cand.Order, site.Schedule.Location)
		it.Updated = laterThan(res.Updated, it.Published)
		st.New++
		return r.Store.PutItem(ctx, it)
	}

	it.GUID, it.FirstSeen = existing.GUID, existing.FirstSeen
	it.Published, it.PublishedSource = existing.Published, existing.PublishedSource
	it.Updated, it.MissingSince = existing.Updated, time.Time{}
	if it.SourcePublished == "" {
		// A date missing from one fetch isn't a change: keep the stored one so
		// a move is still detected when the date comes back.
		it.SourcePublished = existing.SourcePublished
	}
	if existing.SourcePublished != "" && it.SourcePublished != existing.SourcePublished {
		// The site itself moved the publish date to another day (§5.3): the one
		// case where a frozen timestamp is recomputed.
		it.Published, it.PublishedSource = publishedAt(res, existing.FirstSeen, t.cand.Order, site.Schedule.Location)
		log.Info("source publish date changed", "url", t.cand.URL, "from", existing.SourcePublished, "to", it.SourcePublished)
	}

	edited := it.ContentHash != existing.ContentHash
	if u := laterThan(res.Updated, it.Published); u.After(it.Updated) {
		it.Updated = u
	} else if edited {
		it.Updated = now // edited without a newer dateModified
	}
	if !edited && it.Published.Equal(existing.Published) && it.Updated.Equal(existing.Updated) && sameMeta(it, existing) {
		st.Unchanged++
		return r.Store.TouchFetched(ctx, site.ID, existing.GUID, now)
	}
	// Metadata-only changes (a new canonical, author, image or categories, or
	// the article moving to a new URL) are stored too, without bumping Updated.
	st.Updated++
	return r.Store.PutItem(ctx, it)
}

// sameMeta compares the stored fields outside the content hash.
func sameMeta(a, b *store.Item) bool {
	return a.URL == b.URL && a.Canonical == b.Canonical && a.Author == b.Author && a.SourcePublished == b.SourcePublished &&
		a.ImageURL == b.ImageURL && slices.Equal(a.Categories, b.Categories)
}

// build turns an extraction into an item, normalizing the body and summary.
func build(site *config.Site, res *extract.Result) (*store.Item, error) {
	base, err := url.Parse(res.Base)
	if err != nil {
		return nil, err
	}
	opts := normalize.BodyOptions{Base: base, KeepVideo: site.Item.Content.KeepVideo}
	if site.KeepFirstImage() {
		opts.LeadImage = res.Image
	}
	body, err := normalize.Body(res.Content, opts)
	if err != nil {
		return nil, fmt.Errorf("normalizing body: %w", err)
	}
	summary := res.Summary
	if summary == "" {
		summary = normalize.Excerpt(normalize.PlainText(body), SummaryChars)
	}
	it := &store.Item{
		SiteID:      site.ID,
		Canonical:   res.Canonical,
		Title:       res.Title,
		Summary:     summary,
		ContentHTML: body,
		Author:      res.Author,
		ImageURL:    res.Image,
		Categories:  res.Categories,
	}
	if !res.Published.IsZero() {
		it.SourcePublished = res.Published.In(site.Schedule.Location).Format(time.DateOnly)
	}
	sum := sha256.Sum256([]byte(res.Title + "\x00" + summary + "\x00" + body))
	it.ContentHash = hex.EncodeToString(sum[:])
	return it, nil
}

// publishedAt applies the §5.2 policy. Exact timestamps are used as-is. A
// date-only value gets the first-seen time when the item was first seen that
// same local day (a new post), else local midnight plus a sub-second offset
// by discovery order so same-day items sort deterministically.
func publishedAt(res *extract.Result, firstSeen time.Time, order int, loc *time.Location) (time.Time, string) {
	src := res.Sources["published"]
	switch {
	case res.Published.IsZero():
		return firstSeen, "first_seen"
	case !res.PublishedDateOnly:
		return res.Published, src
	}
	at, usedFirstSeen := dateOnlyAt(res.Published, firstSeen, order, loc)
	if usedFirstSeen {
		return at, src + " (date) + first_seen time"
	}
	return at, src
}

// dateOnlyAt places a date-only value (§5.2): the first-seen time when first
// seen that same local day, else local midnight plus a sub-second offset by
// discovery order.
func dateOnlyAt(date, firstSeen time.Time, order int, loc *time.Location) (time.Time, bool) {
	day := date.In(loc)
	if fs := firstSeen.In(loc); fs.Year() == day.Year() && fs.YearDay() == day.YearDay() {
		return firstSeen, true
	}
	midnight := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
	return midnight.Add(time.Duration(999-min(order, 999)) * time.Millisecond), false
}

// guidFor returns the canonical URL, or for guid: stable-hash a UUIDv5 of the
// first-seen URL (§7.1).
func guidFor(site *config.Site, canonical, discoveredURL string) string {
	if site.Item.GUID == config.GUIDStableHash {
		return uuid.NewSHA1(uuid.NameSpaceURL, []byte(discoveredURL)).String()
	}
	return canonical
}

// laterThan returns t if it is after ref, else the zero time.
func laterThan(t, ref time.Time) time.Time {
	if t.After(ref) {
		return t
	}
	return time.Time{}
}
