package helpers

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

// send_media's url comes from the model, so it must not reach the server's
// own network.
func TestMediaURLClientRefusesNonPublicAddresses(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
	}))
	defer srv.Close()

	if resp, err := mediaURLClient.Get(srv.URL + "/secret.png"); err == nil || hit {
		if resp != nil {
			resp.Body.Close()
		}
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
		"31.97.70.27":     true, // the MinIO host media links point at
		"2606:4700::1111": true,
	} {
		if got := isPublicAddr(netip.MustParseAddr(ip)); got != want {
			t.Errorf("isPublicAddr(%s) = %v, want %v", ip, got, want)
		}
	}
}
