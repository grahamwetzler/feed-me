package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"time"

	"feed-me/internal/feed"
	"feed-me/internal/fetch"
	"feed-me/internal/pipeline"
)

// cmdValidate renders each selected site's feed from the store and checks it
// locally; with --w3c it also asks the W3C Feed Validation Service (§7.3).
func cmdValidate(args []string, stdout, stderr io.Writer) int {
	fs, cfgPath := newFlags("validate", stderr)
	siteID := fs.String("site", "", "validate only this site")
	w3c := fs.Bool("w3c", false, "also submit the feed to the W3C Feed Validation Service")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	e, code := open(ctx, *cfgPath, *siteID, stderr)
	if e == nil {
		return code
	}
	defer e.store.Close()

	failed, posted := false, false
	for _, site := range e.sites {
		for _, f := range pipeline.Formats {
			name := site.ID + f.Ext
			data, n, err := pipeline.Render(ctx, e.store, &e.cfg.Global, site, generator(), f)
			if err != nil {
				fmt.Fprintf(stderr, "%s: %v\n", name, err)
				failed = true
				continue
			}
			if n == 0 {
				fmt.Fprintf(stderr, "%s: no stored items; run feed-me build first\n", name)
				failed = true
				continue
			}
			probs := f.Check(data)
			for _, p := range probs {
				fmt.Fprintf(stdout, "%s: %s\n", name, p)
			}
			failed = failed || len(probs) > 0
			fmt.Fprintf(stdout, "%s: local checks: %d items, %d bytes, %d problem(s)\n", name, n, len(data), len(probs))

			if !*w3c {
				continue
			}
			if posted {
				time.Sleep(2 * time.Second) // be polite to the shared validator
			}
			posted = true
			res, err := feed.ValidateW3C(ctx, &http.Client{Timeout: 2 * time.Minute}, fetch.UserAgent(version, e.cfg.Global.ContactURL), data)
			if err != nil {
				fmt.Fprintf(stderr, "%s: %v\n", name, err)
				failed = true
				continue
			}
			for _, w := range res.Warnings {
				if why, ok := feed.W3CAllowedWarnings[w.Type]; ok {
					fmt.Fprintf(stdout, "%s: W3C (allowed: %s) %s\n", name, why, w)
				}
			}
			bad := res.Unexpected()
			for _, m := range bad {
				fmt.Fprintf(stdout, "%s: W3C %s\n", name, m)
			}
			fmt.Fprintf(stdout, "%s: W3C: valid=%v, %d error(s), %d warning(s), %d unexpected\n",
				name, res.Valid, len(res.Errors), len(res.Warnings), len(bad))
			failed = failed || len(bad) > 0
		}
	}
	if failed {
		return 1
	}
	return 0
}
