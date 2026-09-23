// Package transport implements the agent side of the control-plane wire
// protocol: enrollment (no cert yet, presents a one-time token) and the
// authenticated mTLS client used for heartbeat/telemetry once enrolled.
// The agent's private key is generated locally and never leaves the host —
// only the CSR (public key + requested identity) is sent to the control
// plane (ARCHITECTURE.md §G).
package transport

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/nandani/docker-monitor/internal/shared/models"
)

// EnrollConfig holds what's needed to perform one-time enrollment against
// the control plane's agent-facing listener.
type EnrollConfig struct {
	ControlPlaneAddr string // host:port, e.g. "10.0.0.1:8443"
	HostID           string
	Token            string
	CAPEM            []byte // pinned at enrollment time, per ARCHITECTURE.md §G
}

// EnrollResult carries the freshly issued certificate and the private key
// generated locally for it — both PEM-encoded, ready to persist to disk
// (DM_AGENT_CERT_DIR) so subsequent agent runs skip enrollment.
type EnrollResult struct {
	CertPEM []byte
	KeyPEM  []byte
}

// Enroll generates a fresh ECDSA key and CSR locally, then exchanges the
// one-time token for a signed certificate over TLS (trusting only the
// pinned CA — the control plane doesn't yet have this agent's cert, so
// this leg can't be mTLS-authenticated the way heartbeat/telemetry are).
func Enroll(ctx context.Context, cfg EnrollConfig) (*EnrollResult, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate agent key: %w", err)
	}
	csrTmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: cfg.HostID}}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, csrTmpl, key)
	if err != nil {
		return nil, fmt.Errorf("create CSR: %w", err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(cfg.CAPEM) {
		return nil, fmt.Errorf("invalid CA PEM")
	}
	httpClient := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: caPool, MinVersion: tls.VersionTLS12},
		},
	}

	reqBody, err := json.Marshal(struct {
		HostID string `json:"host_id"`
		Token  string `json:"token"`
		CSRPEM []byte `json:"csr_pem"`
	}{HostID: cfg.HostID, Token: cfg.Token, CSRPEM: csrPEM})
	if err != nil {
		return nil, fmt.Errorf("marshal enroll request: %w", err)
	}

	url := "https://" + cfg.ControlPlaneAddr + "/agent/v1/enroll"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("build enroll request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("enroll request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("enroll rejected: %d %s", resp.StatusCode, string(msg))
	}

	var out struct {
		CertPEM []byte `json:"cert_pem"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode enroll response: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal agent key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	return &EnrollResult{CertPEM: out.CertPEM, KeyPEM: keyPEM}, nil
}

// Client is the authenticated mTLS client an enrolled agent uses for
// heartbeat and telemetry.
type Client struct {
	httpClient *http.Client
	addr       string
}

// NewClient builds a client presenting certPEM/keyPEM as its mTLS identity
// and trusting only caPEM (pinned at enrollment) for the server's identity.
func NewClient(addr string, certPEM, keyPEM, caPEM []byte) (*Client, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("load agent cert/key: %w", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("invalid CA PEM")
	}
	return &Client{
		addr: addr,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					Certificates: []tls.Certificate{cert},
					RootCAs:      caPool,
					MinVersion:   tls.VersionTLS12,
				},
			},
		},
	}, nil
}

func (c *Client) do(ctx context.Context, path string, body interface{}) error {
	return c.doJSON(ctx, path, body, nil)
}

// doJSON is like do but additionally decodes a JSON response body into out
// (skipped if out is nil or the response has no body, i.e. 204). Used by
// SendTelemetry to receive the FSM's next-interval directive back from the
// control plane.
func (c *Client) doJSON(ctx context.Context, path string, body interface{}, out interface{}) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+c.addr+path, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s rejected: %d %s", path, resp.StatusCode, string(msg))
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Heartbeat reports liveness and basic daemon info.
func (c *Client) Heartbeat(ctx context.Context, dockerVersion, osInfo string) error {
	return c.do(ctx, "/agent/v1/heartbeat", struct {
		DockerVersion string `json:"docker_version"`
		OSInfo        string `json:"os_info"`
	}{DockerVersion: dockerVersion, OSInfo: osInfo})
}

// TelemetryContainer is one container's discovery snapshot, sent alongside
// metrics in a single telemetry batch.
type TelemetryContainer struct {
	DockerContainerID string                 `json:"docker_container_id"`
	Name              string                 `json:"name"`
	Image             string                 `json:"image"`
	Status            models.ContainerStatus `json:"status"`
	HealthStatus      models.HealthStatus    `json:"health_status"`
	RestartCount      int                    `json:"restart_count"`
}

// TelemetryResult is the control plane's FSM decision (ARCHITECTURE.md §F)
// for this host's next collection cycle, relayed back in the telemetry
// response so the agent's collector can adjust its polling cadence without
// waiting for a separate control channel.
type TelemetryResult struct {
	Mode            models.MonitoringMode `json:"mode"`
	IntervalSeconds int                   `json:"interval_seconds"`
}

// SendTelemetry reports discovered containers and their metrics in a
// single batch (ARCHITECTURE.md §28 — batching, not per-metric requests),
// and returns the control plane's resulting polling directive.
func (c *Client) SendTelemetry(ctx context.Context, containers []TelemetryContainer, metrics []models.Metric) (*TelemetryResult, error) {
	var out TelemetryResult
	err := c.doJSON(ctx, "/agent/v1/telemetry", struct {
		Containers []TelemetryContainer `json:"containers"`
		Metrics    []models.Metric      `json:"metrics"`
	}{Containers: containers, Metrics: metrics}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}
