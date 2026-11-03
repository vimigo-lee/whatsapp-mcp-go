package main

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A client that trickles its headers is dropped once ReadHeaderTimeout passes,
// while a request whose handler runs longer than that still completes with
// its context alive.
func TestServerDropsSlowHeadersButNotSlowHandlers(t *testing.T) {
	ctxErr := make(chan error, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		time.Sleep(600 * time.Millisecond) // longer than the header timeout below
		ctxErr <- r.Context().Err()
		w.Write([]byte("done"))
	}))
	srv.Config.ReadHeaderTimeout = 200 * time.Millisecond
	srv.Config.IdleTimeout = time.Second
	srv.Start()
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	// Slowloris: open a request and never finish the headers.
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n"))
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	start := time.Now()
	if _, err := bufio.NewReader(c).ReadByte(); err == nil || time.Since(start) > 1500*time.Millisecond {
		t.Errorf("half-sent headers: err=%v after %v, want the server to close within ~200ms", err, time.Since(start))
	}

	// A normal request with a slow handler still finishes.
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{"media_base64":"AAAA"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "done" {
		t.Errorf("slow handler response = %q, want done", body)
	}
	if err := <-ctxErr; err != nil {
		t.Errorf("slow handler's context was cancelled: %v", err)
	}
}
