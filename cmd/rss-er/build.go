package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"rss-er/internal/config"
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
			logRun(log, st)
			// Stored items still make a feed, but the exit status must show that
			// some pages couldn't be refreshed.
			failed = failed || st.Errors > 0
		}

		for _, f := range pipeline.Formats {
			path := filepath.Join(e.cfg.Global.OutDir, filepath.FromSlash(pipeline.FeedPath(site.ID, f)))
			n, err := writeFeed(ctx, e, site, f, path)
			if err != nil {
				log.Error("feed not written", "path", path, "err", err)
				failed = true
				continue
			}
			fmt.Fprintf(stdout, "%s: %d items → %s\n", site.ID, n, path)
		}
	}
	if failed {
		return 1
	}
	return 0
}

// writeFeed renders a site's feed, checks it and writes it atomically. It
// never writes an empty feed, leaving any previous file in place (§8).
func writeFeed(ctx context.Context, e *env, site *config.Site, f pipeline.Format, path string) (int, error) {
	data, n, err := pipeline.Render(ctx, e.store, &e.cfg.Global, site, generator(), f)
	if err != nil {
		return 0, fmt.Errorf("render: %w", err)
	}
	if n == 0 {
		return 0, errors.New("no items stored")
	}
	if probs := f.Check(data); len(probs) > 0 {
		return 0, fmt.Errorf("rendered feed failed its checks: %s", strings.Join(probs, "; "))
	}
	return n, pipeline.WriteFileAtomic(path, data)
}

// logRun writes the one line per run that §8.1 (observability) asks for.
func logRun(log *slog.Logger, st pipeline.Stats) {
	log.Info("run complete", "discovered", st.Discovered, "fetched", st.Fetched, "not_modified", st.NotModified,
		"new_items", st.New, "updated_items", st.Updated, "unchanged", st.Unchanged, "errors", st.Errors,
		"duration", st.Duration.Round(time.Millisecond).String())
}
