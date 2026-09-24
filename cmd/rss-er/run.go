package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"rss-er/internal/config"
	"rss-er/internal/pipeline"
	"rss-er/internal/scheduler"
	"rss-er/internal/server"
)

// listening, when set, is told the address run serves on (for tests that listen on port 0).
var listening func(addr string)

// Shutdown takes at most stopTimeout + drainTimeout; compose.yaml's
// stop_grace_period must leave room for both.
const (
	drainTimeout = 10 * time.Second // for open HTTP connections at shutdown
	stopTimeout  = 30 * time.Second // for the page in flight at shutdown
)

// cmdRun is the long-running mode (§8.1): the scheduler plus the HTTP server.
func cmdRun(args []string, stdout, stderr io.Writer) int {
	fs, cfgPath := newFlags("run", stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	e, code := open(ctx, *cfgPath, "", stderr)
	if e == nil {
		return code
	}
	defer e.store.Close()
	g := &e.cfg.Global

	srv := server.New(g, e.sites, e.store, generator(), e.log)
	for _, site := range e.sites {
		if err := srv.Refresh(ctx, site); err != nil {
			e.log.Info("no stored feed to serve yet", "site", site.ID, "err", err)
		}
	}
	ln, err := net.Listen("tcp", g.Listen)
	if err != nil {
		fmt.Fprintf(stderr, "rss-er: %v\n", err)
		return 1
	}
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- hs.Serve(ln) }()
	e.log.Info("serving", "addr", ln.Addr().String(), "base_path", g.BasePath, "sites", len(e.sites))
	if listening != nil {
		listening(ln.Addr().String())
	}

	// Runs don't use ctx: on a signal, Stop lets the page in flight finish.
	// runCtx is only cancelled if that takes longer than stopTimeout.
	runCtx, cancelRuns := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRuns()
	stopRuns := make(chan struct{})
	client := newClient(g, e.log)
	runner := &pipeline.Runner{Store: e.store, Log: e.log, Stop: stopRuns}
	sched := &scheduler.Scheduler{
		Sites: e.sites,
		LastRun: func(ctx context.Context, id string) (time.Time, error) {
			st, err := e.store.SiteState(ctx, id)
			return st.LastRun, err
		},
		Run: func(_ context.Context, site *config.Site) {
			log := e.log.With("site", site.ID)
			st, err := runner.Run(runCtx, site, siteFetcher(client, site))
			switch {
			case errors.Is(err, pipeline.ErrStopped):
				log.Info("run stopped for shutdown", "fetched", st.Fetched, "new_items", st.New)
			case err != nil:
				log.Error("run failed; still serving the last good feed", "err", err)
			default:
				logRun(log, st)
			}
			if err := srv.Refresh(runCtx, site); err != nil {
				log.Warn("feed not refreshed", "err", err)
			}
		},
	}
	scheduled := make(chan error, 1)
	go func() { scheduled <- sched.Loop(ctx) }()

	failed, schedDone := false, false
	select {
	case <-ctx.Done():
		e.log.Info("shutting down")
	case err := <-served:
		e.log.Error("http server stopped", "err", err)
		failed = true
		stop()
	case err := <-scheduled:
		// Without the scheduler, feeds would silently stop refreshing while
		// /healthz stays green; exit so the container is restarted.
		e.log.Error("scheduler stopped; exiting", "err", err)
		failed, schedDone = true, true
		stop()
	}
	close(stopRuns)
	if !schedDone {
		timer := time.AfterFunc(stopTimeout, cancelRuns)
		if err := <-scheduled; err != nil {
			e.log.Error("scheduler", "err", err)
			failed = true
		}
		timer.Stop()
	}

	dctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	if err := hs.Shutdown(dctx); err != nil {
		e.log.Warn("http drain", "err", err)
	}
	if failed {
		return 1
	}
	return 0
}

// cmdHealthcheck probes /healthz on the local server, for Docker's
// HEALTHCHECK in an image with no shell or curl (§8.1).
func cmdHealthcheck(args []string, stdout, stderr io.Writer) int {
	fs, cfgPath := newFlags("healthcheck", stderr)
	url := fs.String("url", "", "URL to probe (default: /healthz on the configured listen address)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *url == "" {
		g, err := config.LoadGlobal(*cfgPath)
		if err != nil {
			return reportErrors(err, stderr)
		}
		if *url, err = healthURL(g.Listen, g.BasePath); err != nil {
			fmt.Fprintf(stderr, "rss-er: %v\n", err)
			return 2
		}
	}
	hc := &http.Client{Timeout: 5 * time.Second}
	resp, err := hc.Get(*url)
	if err != nil {
		fmt.Fprintf(stderr, "unhealthy: %v\n", err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "unhealthy: %s answered %s\n", *url, resp.Status)
		return 1
	}
	fmt.Fprintln(stdout, "ok")
	return 0
}

// healthURL is the loopback /healthz URL for a listen address such as ":8080".
func healthURL(listen, basePath string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("listen address %q: %w", listen, err)
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + basePath + "healthz", nil
}
