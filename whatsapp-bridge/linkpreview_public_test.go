package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A link in a message must not make the bridge fetch from its own network.
func TestLinkPreviewRefusesNonPublicAddresses(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.Write([]byte(`<title>internal</title>`))
	}))
	defer srv.Close()

	if _, _, err := previewHTTPGet(context.Background(), srv.URL, 1024); err == nil || hit {
		t.Errorf("loopback fetch: err=%v hit=%v, want refused before any request", err, hit)
	}

	for ip, want := range map[string]bool{
		"127.0.0.1":       false,
		"10.0.1.56":       false,
		"172.17.0.1":      false,
		"192.168.1.10":    false,
		"169.254.169.254": false, // cloud metadata
		"100.100.1.1":     false, // CGNAT / Tailscale
		"0.0.0.0":         false,
		"::1":             false,
		"fd00::1":         false,
		"fe80::1":         false,
		"::ffff:10.0.0.1": false,
		"8.8.8.8":         true,
		"31.97.70.27":     true,
		"2606:4700::1111": true,
	} {
		if got := isPublicIP(net.ParseIP(ip)); got != want {
			t.Errorf("isPublicIP(%s) = %v, want %v", ip, got, want)
		}
	}
}
