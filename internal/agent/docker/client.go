// Package docker implements a minimal Docker Engine API client for the
// agent, talking to the local daemon over its Unix socket using only the
// standard library (ARCHITECTURE.md §9 — "avoid unnecessary dependencies").
//
// This is deliberately not the official Docker SDK: that module pulls in
// golang.org/x/net transitively, and more importantly the agent only ever
// needs a handful of read/write calls against a local socket, so a thin
// stdlib client keeps the binary small and dependency-free per the
// lightweight-agent requirement (ARCHITECTURE.md §9, master prompt §31).
package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Client is a minimal Docker Engine API client bound to a single Unix
// socket path.
type Client struct {
	httpClient *http.Client
	sockPath   string
}

// NewClient builds a client that dials sockPath for every request. The
// host portion of request URLs is never actually resolved over the network
// — DialContext ignores it and always dials the Unix socket — so a fixed
// placeholder host ("docker") is used throughout this package.
func NewClient(sockPath string) *Client {
	return &Client{
		sockPath: sockPath,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sockPath)
				},
			},
		},
	}
}

// APIError represents a non-2xx response from the Docker daemon itself —
// as opposed to a transport-level failure (socket missing, connection
// refused, timeout). Callers such as the monitoring FSM rely on this
// distinction: an APIError means "the daemon is up and told us no"; any
// other error means "we couldn't reach the daemon at all" and should be
// treated as a host/daemon-unreachable critical condition
// (ARCHITECTURE.md §35, §3).
type APIError struct {
	StatusCode int
	Path       string
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("docker API error: %s: %d %s", e.Path, e.StatusCode, e.Message)
}

// do issues an HTTP request against the Docker Engine API and decodes a
// JSON response into out (skipped if out is nil). A non-2xx status becomes
// an *APIError; any other failure (dial refused, socket absent, context
// deadline) is returned unwrapped so callers can tell it apart from an
// APIError.
func (c *Client) do(ctx context.Context, method, path string, body io.Reader, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		apiErr := &APIError{StatusCode: resp.StatusCode, Path: path}
		var decoded struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &decoded) == nil && decoded.Message != "" {
			apiErr.Message = decoded.Message
		} else {
			apiErr.Message = string(raw)
		}
		return apiErr
	}

	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Ping checks daemon reachability (GET /_ping) — used as the agent's
// cheapest possible daemon-unavailable detector (ARCHITECTURE.md §3).
func (c *Client) Ping(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/_ping", nil, nil)
}

// VersionInfo is the subset of GET /version fields the agent reports and
// stores on the Host record (ARCHITECTURE.md §7 — "Docker version",
// "OS information").
type VersionInfo struct {
	Version    string `json:"Version"`
	APIVersion string `json:"ApiVersion"`
	Os         string `json:"Os"`
	Arch       string `json:"Arch"`
}

// Version fetches the daemon's version info.
func (c *Client) Version(ctx context.Context) (VersionInfo, error) {
	var out VersionInfo
	err := c.do(ctx, http.MethodGet, "/version", nil, &out)
	return out, err
}
