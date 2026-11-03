package helpers

import (
	"encoding/json"
	"log/slog"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var apiBaseURL = readApiBaseURL()

// The bridge's API key travels with every call, so the default is the bridge
// on this machine. The old default was a LAN address from someone's own
// setup: anyone who left API_BASE_URL unset sent their key to whatever
// answered there.
const defaultAPIBaseURL = "http://localhost:8080/api"

func readApiBaseURL() string {
	v := os.Getenv("API_BASE_URL")
	if v == "" {
		slog.Warn("API_BASE_URL not set, using the bridge on this machine", "url", defaultAPIBaseURL)
		return defaultAPIBaseURL
	}
	if plainHTTPOverPublicNetwork(v) {
		slog.Warn("API_BASE_URL is plain http to a public host; the bridge API key is sent unencrypted. Use https or a private address.", "url", v)
	}
	return v
}

// plainHTTPOverPublicNetwork reports an http:// URL whose host is a public
// IP or a dotted hostname. Loopback, private IPs and single-label names
// (a Docker service such as wa-bridge) are treated as local.
func plainHTTPOverPublicNetwork(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" {
		return false
	}
	host := u.Hostname()
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		return !ip.IsLoopback() && !ip.IsPrivate() && !sharedAddressSpace.Contains(ip)
	}
	return host != "localhost" && strings.Contains(host, ".")
}

const apiTimeout = 60 * time.Second

// OkResult return proper ok result for mcp tool
func OkResult(v any) *mcp.CallToolResult {
	b, err := json.Marshal(v)
	if err != nil {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{
				&mcp.TextContent{Text: "Failed to encode JSON: " + err.Error()},
			},
		}
	}

	return &mcp.CallToolResult{
		IsError: false,
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(b)},
		},
	}
}

// ErrResult return error result for mcp tool
func ErrResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{
			&mcp.TextContent{Text: msg},
		},
	}
}

// ReadEnv read return value for an env
func ReadEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}
