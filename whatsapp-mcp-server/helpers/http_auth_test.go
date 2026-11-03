package helpers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The HTTP transport hands every tool to whoever it serves, so a request
// without the configured bearer token must never reach the MCP handler.
func TestHTTPTransportRequiresBearerToken(t *testing.T) {
	const token = "test-mcp-http-token-0123456789abcdef"
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "v0"}, nil)
	handler := requireBearerToken(token, mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, nil))
	srv := httptest.NewServer(handler)
	defer srv.Close()

	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	cases := []struct {
		name, auth string
		want       int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"wrong token", "Bearer not-the-token", http.StatusUnauthorized},
		{"token without scheme", token, http.StatusUnauthorized},
		{"correct token", "Bearer " + token, http.StatusOK},
	}
	for _, c := range cases {
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(initialize))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if c.auth != "" {
			req.Header.Set("Authorization", c.auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s: status %d, want %d", c.name, resp.StatusCode, c.want)
		}
	}
}

// Without a token, a peer on a public address must be refused while loopback,
// private-network and tailnet peers (existing deployments) keep working.
func TestPrivateNetworkOnlyWithoutToken(t *testing.T) {
	handler := privateNetworkOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	cases := map[string]int{
		"127.0.0.1:5000":       http.StatusOK,
		"[::1]:5000":           http.StatusOK,
		"172.18.0.5:5000":      http.StatusOK,
		"192.168.1.20:5000":    http.StatusOK,
		"10.0.0.7:5000":        http.StatusOK,
		"100.101.102.103:5000": http.StatusOK,
		"[::ffff:10.0.0.7]:1":  http.StatusOK,
		"203.0.113.9:5000":     http.StatusForbidden,
		"[2001:db8::1]:5000":   http.StatusForbidden,
		"8.8.8.8:5000":         http.StatusForbidden,
	}
	for remote, want := range cases {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.RemoteAddr = remote
		req.Host = "localhost:5777" // the Host check has its own test
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("%s: status %d, want %d", remote, rec.Code, want)
		}
	}
}

func TestMCPHTTPTokenRequiresLongToken(t *testing.T) {
	t.Setenv("MCP_HTTP_TOKEN", "")
	if tok, err := mcpHTTPToken(); err != nil || tok != "" {
		t.Errorf("unset MCP_HTTP_TOKEN should be allowed and empty, got %q, %v", tok, err)
	}
	t.Setenv("MCP_HTTP_TOKEN", "too-short")
	if _, err := mcpHTTPToken(); err == nil {
		t.Error("short MCP_HTTP_TOKEN accepted")
	}
	t.Setenv("MCP_HTTP_TOKEN", strings.Repeat("x", minHTTPTokenLen))
	if _, err := mcpHTTPToken(); err != nil {
		t.Errorf("valid MCP_HTTP_TOKEN rejected: %v", err)
	}
}
