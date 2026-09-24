package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"rss-er/internal/config"
	"rss-er/internal/feed"
	"rss-er/internal/fetch"
	"rss-er/internal/pipeline"
	"rss-er/internal/store"
)

// env is what the build and validate commands share.
type env struct {
	cfg   *config.Config
	store *store.Store
	log   *slog.Logger
	sites []*config.Site
}

// open loads config, opens the store and selects the sites (all, or --site).
func open(ctx context.Context, cfgPath, siteID string, stderr io.Writer) (*env, int) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, reportErrors(err, stderr)
	}
	sites := cfg.Sites
	if siteID != "" {
		s := cfg.Site(siteID)
		if s == nil {
			fmt.Fprintf(stderr, "rss-er: no site %q in %s\n", siteID, cfg.Global.SitesDir)
			return nil, 2
		}
		sites = []*config.Site{s}
	}
	st, err := store.Open(ctx, cfg.Global.StorePath)
	if err != nil {
		fmt.Fprintf(stderr, "rss-er: %v\n", err)
		return nil, 1
	}
	return &env{cfg: cfg, store: st, log: newLogger(cfg.Global.Log, stderr), sites: sites}, 0
}

func generator() string { return fetch.Product + "/" + version }

// cmdBuild runs every selected site once and writes its feed to out_dir (§8, build).
func cmdBuild(args []string, stdout, stderr io.Writer) int {
	fs, cfgPath := newFlags("build", stderr)
	siteID := fs.String("site", "", "build only this site")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	e, code := open(ctx, *cfgPath, *siteID, stderr)
	if e == nil {
		return code
	}
	defer e.store.Close()

	client := newClient(&e.cfg.Global, e.log)
	runner := &pipeline.Runner{Store: e.store, Log: e.log}
	failed := false
	for _, site := range e.sites {
		log := e.log.With("site", site.ID)
		st, err := runner.Run(ctx, site, siteFetcher(client, site))
		if err != nil {
			// The store still holds the last good items, so the feed is still written below.
			log.Error("run failed", "err", err)
			failed = true
		} else {
			log.Info("run complete", "discovered", st.Discovered, "fetched", st.Fetched, "not_modified", st.NotModified,
				"new_items", st.New, "updated_items", st.Updated, "unchanged", st.Unchanged, "errors", st.Errors, "duration", st.Duration.Round(time.Millisecond))
		}

		data, n, err := pipeline.RenderRSS(ctx, e.store, &e.cfg.Global, site, generator())
		if err != nil {
			log.Error("render failed", "err", err)
			failed = true
			continue
		}
		path := filepath.Join(e.cfg.Global.OutDir, filepath.FromSlash(pipeline.FeedPath(site.ID)))
		if n == 0 {
			// Never publish an empty feed; leave any previous file in place (§8).
			log.Warn("no items stored; feed not written", "path", path)
			failed = true
			continue
		}
		if probs := feed.Check(data); len(probs) > 0 {
			for _, p := range probs {
				log.Error("rendered feed failed a check", "problem", p)
			}
			failed = true
			continue
		}
		if err := pipeline.WriteFileAtomic(path, data); err != nil {
			log.Error("writing feed", "err", err)
			failed = true
			continue
		}
		fmt.Fprintf(stdout, "%s: %d items → %s\n", site.ID, n, path)
	}
	if failed {
		return 1
	}
	return 0
}
