package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"rss-er/internal/config"
	"rss-er/internal/feed"
	"rss-er/internal/store"
)

// FeedPath is a site's RSS path below base_path.
func FeedPath(siteID string) string { return "feeds/" + siteID + ".xml" }

// RenderRSS renders the site's newest max_items stored items. It returns the
// number of items rendered.
func RenderRSS(ctx context.Context, st *store.Store, g *config.Global, site *config.Site, generator string) ([]byte, int, error) {
	items, err := st.Recent(ctx, site.ID, site.Schedule.MaxItems)
	if err != nil {
		return nil, 0, err
	}
	state, err := st.SiteState(ctx, site.ID)
	if err != nil {
		return nil, 0, err
	}
	ch := feed.Channel{
		Title:       site.Channel.Title,
		Link:        site.Channel.Link,
		Description: site.Channel.Description,
		Language:    site.Channel.Language,
		Image:       site.Channel.Image,
		TTL:         site.Channel.TTL,
		SelfURL:     g.PublicBaseURL + g.BasePath + FeedPath(site.ID),
		LastBuild:   state.LastChange,
		Generator:   generator,
	}
	out := make([]feed.Item, len(items))
	for i, it := range items {
		author := it.Author
		if author == "" {
			author = site.Channel.Author
		}
		out[i] = feed.Item{
			Title:       it.Title,
			Link:        it.Canonical,
			GUID:        it.GUID,
			PermaLink:   site.Item.GUID == config.GUIDPermalink,
			Description: it.Summary,
			ContentHTML: it.ContentHTML,
			Author:      author,
			Categories:  it.Categories,
			Published:   it.Published,
			Updated:     it.Updated,
			Image:       it.ImageURL,
			Enclosure:   site.Item.Enclosure,
		}
	}
	data, err := feed.RSS(ch, out)
	return data, len(items), err
}

// WriteFileAtomic writes via a temp file and rename, so readers never see a
// partial feed (§8, build).
func WriteFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replacing %s: %w", path, err)
	}
	return nil
}
