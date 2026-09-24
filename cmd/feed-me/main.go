// Command feed-me turns websites into full-text RSS feeds from declarative configs.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	_ "time/tzdata" // resolve site timezones inside distroless images

	"feed-me/internal/config"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `Feed Me! turns websites into full-text RSS feeds.

Usage:
  feed-me run      [--config feed-me.yaml]   long-running: scheduler + HTTP server
  feed-me build    [--site id]              one-shot: fetch and write static feeds to out_dir
  feed-me check    --site id [--url URL]    dry-run extraction for one site
  feed-me validate [--site id] [--w3c]      render feeds from the store and check them
  feed-me config lint [site.yaml ...]       validate configs; exits non-zero on error
  feed-me healthcheck                       probe /healthz (for Docker HEALTHCHECK)
  feed-me version

Every command accepts --config (default: feed-me.yaml, or $FEED_ME_CONFIG).
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "config":
		if len(args) == 0 || args[0] != "lint" {
			fmt.Fprintln(stderr, "usage: feed-me config lint [site.yaml ...]")
			return 2
		}
		return cmdConfigLint(args[1:], stdout, stderr)
	case "check":
		return cmdCheck(args, stdout, stderr)
	case "build":
		return cmdBuild(args, stdout, stderr)
	case "validate":
		return cmdValidate(args, stdout, stderr)
	case "run":
		return cmdRun(args, stdout, stderr)
	case "healthcheck":
		return cmdHealthcheck(args, stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, "feed-me", version)
		return 0
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "feed-me: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
}

// newFlags returns a flag set with the shared --config flag.
func newFlags(name string, stderr io.Writer) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	def := os.Getenv("FEED_ME_CONFIG")
	if def == "" {
		def = "feed-me.yaml"
	}
	return fs, fs.String("config", def, "path to the global config file")
}

// cmdConfigLint validates the global config and every site. With file
// arguments, it validates just those site files against the global config.
func cmdConfigLint(args []string, stdout, stderr io.Writer) int {
	fs, cfgPath := newFlags("config lint", stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if fs.NArg() == 0 {
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			return reportErrors(err, stderr)
		}
		ids := make([]string, len(cfg.Sites))
		for i, s := range cfg.Sites {
			ids[i] = s.ID
		}
		fmt.Fprintf(stdout, "ok: %s and %d site(s): %s\n", *cfgPath, len(cfg.Sites), strings.Join(ids, ", "))
		return 0
	}

	g, err := config.LoadGlobal(*cfgPath)
	if err != nil {
		return reportErrors(err, stderr)
	}
	var all config.Errors
	for _, f := range fs.Args() {
		if _, errs := config.LoadSite(f, g); len(errs) > 0 {
			all = append(all, errs...)
		} else {
			fmt.Fprintf(stdout, "ok: %s\n", filepath.Clean(f))
		}
	}
	if len(all) > 0 {
		return reportErrors(all, stderr)
	}
	return 0
}

func reportErrors(err error, stderr io.Writer) int {
	var errs config.Errors
	if errors.As(err, &errs) {
		for _, e := range errs {
			fmt.Fprintln(stderr, e.Error())
		}
		fmt.Fprintf(stderr, "%d error(s)\n", len(errs))
	} else {
		fmt.Fprintln(stderr, err)
	}
	return 1
}
