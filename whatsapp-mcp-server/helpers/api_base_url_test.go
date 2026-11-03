package helpers

import "testing"

func TestReadApiBaseURLDefaultsToThisMachine(t *testing.T) {
	t.Setenv("API_BASE_URL", "")
	if got := readApiBaseURL(); got != "http://localhost:8080/api" {
		t.Errorf("default API_BASE_URL = %q, want the bridge on localhost", got)
	}
	t.Setenv("API_BASE_URL", "http://wa-bridge:8080/api")
	if got := readApiBaseURL(); got != "http://wa-bridge:8080/api" {
		t.Errorf("set API_BASE_URL = %q, want it kept", got)
	}
}

func TestPlainHTTPOverPublicNetwork(t *testing.T) {
	for raw, want := range map[string]bool{
		"http://localhost:8080/api":      false,
		"http://127.0.0.1:41001/api":     false,
		"http://wa-bridge:8080/api":      false,
		"http://10.0.1.56:8080/api":      false,
		"http://[::1]:8080/api":          false,
		"https://bridge.example.com/api": false,
		"http://bridge.example.com/api":  true,
		"http://31.97.70.27:8080/api":    true,
	} {
		if got := plainHTTPOverPublicNetwork(raw); got != want {
			t.Errorf("plainHTTPOverPublicNetwork(%q) = %v, want %v", raw, got, want)
		}
	}
}
