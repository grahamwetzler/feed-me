package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"feed-me/internal/store"
)

// syncBuffer is a bytes.Buffer safe for the run goroutine's logger.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestRunServesFeedsAndShutsDown(t *testing.T) {
	transport, rateOverride = fixtures(t), 1000
	t.Setenv("FEED_ME_PUBLIC_BASE_URL", "")
	addrs := make(chan string, 1)
	listening = func(a string) { addrs <- a }
	t.Cleanup(func() { transport, rateOverride, listening = nil, 0, nil })

	dir := t.TempDir()
	sites, err := filepath.Abs(filepath.Join(root, "sites"))
	must(t, err)
	cfg := filepath.Join(dir, "feed-me.yaml")
	must(t, os.WriteFile(cfg, []byte("public_base_url: https://rss.example.com\nbase_path: /rss/\nlisten: 127.0.0.1:0\nsites_dir: "+sites+
		"\nstore_path: "+filepath.Join(dir, "feed-me.db")+"\nlog: {format: text, level: info}\n"), 0o644))

	var stdout, stderr syncBuffer
	done := make(chan int, 1)
	go func() { done <- run([]string{"run", "--config", cfg}, &stdout, &stderr) }()
	var base string
	select {
	case a := <-addrs:
		base = "http://" + a + "/rss/"
	case code := <-done:
		t.Fatalf("run exited %d before listening:\n%s", code, stderr.String())
	case <-time.After(10 * time.Second):
		t.Fatal("run never started listening")
	}

	get := func(path string) (int, string) {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	if code, _ := get("healthz"); code != http.StatusOK {
		t.Errorf("healthz: %d", code)
	}
	var out bytes.Buffer
	if code := run([]string{"healthcheck", "--url", base + "healthz"}, &out, &out); code != 0 {
		t.Errorf("healthcheck: exit %d: %s", code, out.String())
	}

	// Every site is never-run, so they run at startup; ready once all succeed.
	deadline := time.Now().Add(30 * time.Second)
	for {
		code, body := get("readyz")
		if code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never ready: %d %s\n%s", code, body, stderr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, path := range []string{"feeds/claude-blog.xml", "feeds/claude-blog.atom", "feeds/select-dev.xml", "feeds/select-dev.atom",
		"feeds/claude-dev.xml", "feeds/claude-dev.atom"} {
		// A feed is refreshed just after its run records success, so allow a moment.
		var code int
		var body string
		for range 100 {
			if code, body = get(path); code == http.StatusOK {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if code != http.StatusOK || !strings.Contains(body, "https://rss.example.com/rss/"+path) {
			t.Errorf("%s: %d", path, code)
		}
	}

	must(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit %d after SIGTERM:\n%s", code, stderr.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not exit after SIGTERM")
	}
	if _, err := http.Get(base + "healthz"); err == nil {
		t.Error("still serving after shutdown")
	}
}

func TestHealthURL(t *testing.T) {
	for listen, want := range map[string]string{
		":8080":          "http://127.0.0.1:8080/healthz",
		"0.0.0.0:9000":   "http://127.0.0.1:9000/healthz",
		"[::]:9000":      "http://127.0.0.1:9000/healthz",
		"10.0.0.5:8081":  "http://10.0.0.5:8081/healthz",
		"localhost:8082": "http://localhost:8082/healthz",
	} {
		if got, err := healthURL(listen, "/"); got != want || err != nil {
			t.Errorf("healthURL(%q) = %q, %v; want %q", listen, got, err, want)
		}
	}
	if got, _ := healthURL(":8080", "/rss/"); got != "http://127.0.0.1:8080/rss/healthz" {
		t.Errorf("with base_path: %q", got)
	}
	if got, err := healthURL("9000", "/"); err == nil {
		t.Errorf("bare port: %q, want an error rather than a guess", got)
	}
}

// A scheduler that can't start must end the process, not leave it serving
// stale feeds behind a green healthcheck.
func TestRunExitsWhenSchedulerFails(t *testing.T) {
	t.Setenv("FEED_ME_PUBLIC_BASE_URL", "")
	dir := t.TempDir()
	db := filepath.Join(dir, "feed-me.db")
	st, err := store.Open(context.Background(), db)
	must(t, err)
	must(t, st.Close())
	conn, err := sql.Open("sqlite", db)
	must(t, err)
	_, err = conn.Exec("DROP TABLE site_state")
	must(t, err)
	must(t, conn.Close())

	sites, err := filepath.Abs(filepath.Join(root, "sites"))
	must(t, err)
	cfg := filepath.Join(dir, "feed-me.yaml")
	must(t, os.WriteFile(cfg, []byte("public_base_url: https://rss.example.com\nlisten: 127.0.0.1:0\nsites_dir: "+sites+
		"\nstore_path: "+db+"\nlog: {format: text}\n"), 0o644))

	var stdout, stderr syncBuffer
	done := make(chan int, 1)
	go func() { done <- run([]string{"run", "--config", cfg}, &stdout, &stderr) }()
	select {
	case code := <-done:
		if code != 1 || !strings.Contains(stderr.String(), "scheduler stopped") {
			t.Errorf("exit %d\n%s", code, stderr.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run kept going without a scheduler")
	}
}

// Loop returns nil after a signal, which may win run's select over
// ctx.Done; only an error, or a return with ctx still live, is a failure.
func TestSchedulerFailed(t *testing.T) {
	live := context.Background()
	done, cancel := context.WithCancel(live)
	cancel()
	for _, c := range []struct {
		ctx  context.Context
		err  error
		want bool
	}{
		{done, nil, false},
		{done, errors.New("store"), true},
		{live, nil, true},
		{live, errors.New("store"), true},
	} {
		if got := schedulerFailed(c.ctx, c.err); got != c.want {
			t.Errorf("schedulerFailed(ctx err %v, %v) = %v, want %v", c.ctx.Err(), c.err, got, c.want)
		}
	}
}

// A SIGTERM right after startup, while sites are still being scheduled, is
// an ordinary shutdown.
func TestRunEarlySignalExitsCleanly(t *testing.T) {
	transport, rateOverride = fixtures(t), 1000
	t.Setenv("FEED_ME_PUBLIC_BASE_URL", "")
	listening = func(string) {
		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { transport, rateOverride, listening = nil, 0, nil })

	dir := t.TempDir()
	sites, err := filepath.Abs(filepath.Join(root, "sites"))
	must(t, err)
	cfg := filepath.Join(dir, "feed-me.yaml")
	must(t, os.WriteFile(cfg, []byte("public_base_url: https://rss.example.com\nlisten: 127.0.0.1:0\nsites_dir: "+sites+
		"\nstore_path: "+filepath.Join(dir, "feed-me.db")+"\nlog: {format: text}\n"), 0o644))
	var stdout, stderr syncBuffer
	done := make(chan int, 1)
	go func() { done <- run([]string{"run", "--config", cfg}, &stdout, &stderr) }()
	select {
	case code := <-done:
		if code != 0 || strings.Contains(stderr.String(), "scheduler") {
			t.Errorf("exit %d\n%s", code, stderr.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not exit after SIGTERM")
	}
}
