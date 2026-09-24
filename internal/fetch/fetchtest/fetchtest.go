// Package fetchtest serves saved fixtures in place of the network.
package fetchtest

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
)

// Transport is an http.RoundTripper that answers from a URL → file map.
// Unknown URLs get 404, which matches how robots.txt absence behaves.
type Transport struct {
	Files map[string]string

	mu       sync.Mutex
	Requests []*http.Request
}

func (t *Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.Requests = append(t.Requests, r)
	t.mu.Unlock()

	resp := &http.Response{Request: r, Header: http.Header{}, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1}
	path, ok := t.Files[r.URL.String()]
	if !ok {
		resp.StatusCode = http.StatusNotFound
		resp.Body = io.NopCloser(bytes.NewReader(nil))
		return resp, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("fetchtest: %w", err)
	}
	resp.StatusCode = http.StatusOK
	resp.Body = io.NopCloser(bytes.NewReader(data))
	resp.ContentLength = int64(len(data))
	return resp, nil
}

// Count returns how many requests were made for a URL.
func (t *Transport) Count(url string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, r := range t.Requests {
		if r.URL.String() == url {
			n++
		}
	}
	return n
}
