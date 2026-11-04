package helpers

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Without a token, a request reaching the server through DNS rebinding
// carries the attacker's domain as Host and is refused; local and Docker
// clients keep working.
func TestLoopbackOnlyRefusesRebindingHosts(t *testing.T) {
	handler := loopbackOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for host, want := range map[string]int{
		"localhost:5777":        http.StatusOK,
		"127.0.0.1:5777":        http.StatusOK,
		"[::1]:5777":            http.StatusOK,
		"wa-mcp:5777":           http.StatusOK,
		"172.18.0.5:5777":       http.StatusOK,
		"app.localhost:5777":    http.StatusOK,
		"mypc.lan":              http.StatusOK,
		"box.tail1234.ts.net":   http.StatusOK,
		"nas.local:5777":        http.StatusOK,
		"evil.example.com:5777": http.StatusForbidden,
		"mypc.example.org":      http.StatusForbidden,
	} {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.RemoteAddr = "127.0.0.1:40000"
		req.Host = host
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Host %s: status %d, want %d", host, rec.Code, want)
		}
	}
	t.Setenv("MCP_ALLOWED_HOSTS", "mypc.example.org, other.example")
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "[::1]:40000"
	req.Host = "MyPC.Example.org"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("allowed host refused: %d", rec.Code)
	}
}

func TestBearerSchemeIsCaseInsensitive(t *testing.T) {
	const token = "test-mcp-http-token-0123456789abcdef"
	h := requireBearerToken(token, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for auth, want := range map[string]int{
		"bearer " + token:       http.StatusOK,
		"BEARER " + token:       http.StatusOK,
		"Bearer " + token + "x": http.StatusUnauthorized,
		"Bearer":                http.StatusUnauthorized,
		"Basic " + token:        http.StatusUnauthorized,
	} {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("Authorization", auth)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("%q: status %d, want %d", auth, rec.Code, want)
		}
	}
}

// IPv6 forms that wrap a private IPv4 address are not public.
func TestIsPublicAddrUnwrapsEmbeddedIPv4(t *testing.T) {
	for ip, want := range map[string]bool{
		"64:ff9b::a9fe:a9fe": false, // NAT64 of 169.254.169.254
		"64:ff9b::808:808":   true,  // NAT64 of 8.8.8.8
		"2002:a9fe:a9fe::1":  false, // 6to4
		"2001:0:4136:e378::": false, // Teredo
		"::7f00:1":           false, // IPv4-compatible 127.0.0.1
		"0.1.2.3":            false,
	} {
		if got := isPublicAddr(netip.MustParseAddr(ip)); got != want {
			t.Errorf("isPublicAddr(%s) = %v, want %v", ip, got, want)
		}
	}
}

func TestSendableMediaPathReviewCases(t *testing.T) {
	dir := t.TempDir()
	write := func(rel string, data []byte) string {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := map[string]struct {
		data []byte
		ok   bool
	}{
		"Documents/Pitch.key":                 {[]byte("PK\x03\x04 keynote zip"), true},
		"Documents/notes.txt":                 {[]byte("-----BEGIN OPENSSH PRIVATE KEY-----\nabc"), false},
		"Documents/server.key":                {[]byte("-----BEGIN RSA PRIVATE KEY-----\nabc"), false},
		"Library/Keychains/login.keychain-db": {[]byte("x"), false},
		"firefox/profile/logins.json":         {[]byte("{}"), false},
	}
	for rel, c := range cases {
		_, err := sendableMediaPath(write(rel, c.data))
		if (err == nil) != c.ok {
			t.Errorf("%s: sendable=%v, want %v (err=%v)", rel, err == nil, c.ok, err)
		}
	}
	if runtime.GOOS == "linux" {
		for _, p := range []string{"/proc/self/environ", "/proc/1/environ", "/etc/hostname"} {
			if _, err := sendableMediaPath(p); err == nil {
				t.Errorf("%s was sendable", p)
			}
		}
	}
	if runtime.GOOS == "windows" {
		if _, err := sendableMediaPath(`\\attacker.example@SSL\s\a.pdf`); err == nil {
			t.Error("UNC path was sendable")
		}
	}
}
