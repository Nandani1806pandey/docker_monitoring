// Package integrationtest exercises the full agent<->control-plane
// protocol (enrollment, heartbeat, telemetry) against a real TLS listener,
// the same code paths cmd/controlplane and cmd/agent use — no daemon or
// container required, since it's the transport/registry contract under
// test here, not Docker itself (that's covered separately by
// internal/agent/docker's tests against a fake Unix-socket daemon).
package integrationtest

import (
	"context"
	"crypto/x509"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/nandani/docker-monitor/internal/agent/transport"
	"github.com/nandani/docker-monitor/internal/controlplane/agentapi"
	"github.com/nandani/docker-monitor/internal/controlplane/agentregistry"
	"github.com/nandani/docker-monitor/internal/controlplane/events"
	"github.com/nandani/docker-monitor/internal/controlplane/monitoring"
	"github.com/nandani/docker-monitor/internal/controlplane/pki"
	"github.com/nandani/docker-monitor/internal/controlplane/storage/memory"
	"github.com/nandani/docker-monitor/internal/shared/models"
)

func TestEnrollHeartbeatTelemetryEndToEnd(t *testing.T) {
	store := memory.New()
	recorder := events.NewRecorder(store)

	ca, err := pki.GenerateCA("test-ca", time.Hour)
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	registry := agentregistry.New(store, ca, recorder, monitoring.NewEngine(monitoring.DefaultThresholds()))

	// Register a remote host via the same path the browser API uses.
	host := &models.Host{ID: "host-1", Name: "test-host", Kind: models.HostKindRemote, Address: "10.0.0.1"}
	if err := store.CreateHost(context.Background(), host); err != nil {
		t.Fatalf("CreateHost: %v", err)
	}

	token, err := registry.IssueEnrollmentToken(context.Background(), host.ID)
	if err != nil {
		t.Fatalf("IssueEnrollmentToken: %v", err)
	}

	// Stand up the real agent-facing TLS server on a random port.
	agentSrv := agentapi.NewServer(registry, testLogger())
	serverCertPEM, serverKeyPEM, err := ca.ServerTLSCertificate([]string{"127.0.0.1", "localhost"}, time.Hour)
	if err != nil {
		t.Fatalf("ServerTLSCertificate: %v", err)
	}
	serverCert, err := tlsCertFromPEM(serverCertPEM, serverKeyPEM)
	if err != nil {
		t.Fatalf("tlsCertFromPEM: %v", err)
	}
	caPool := x509.NewCertPool()
	caPool.AddCert(ca.Certificate())

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	httpSrv := &http.Server{
		Handler:   agentSrv.Handler(),
		TLSConfig: agentapi.TLSConfig(serverCert, caPool),
	}
	go httpSrv.ServeTLS(listener, "", "")
	t.Cleanup(func() { httpSrv.Close() })

	addr := listener.Addr().String()
	caPEM := ca.CertPEM()

	// --- Enrollment: agent has no cert yet, presents the token over TLS ---
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	enrollResult, err := transport.Enroll(ctx, transport.EnrollConfig{
		ControlPlaneAddr: addr,
		HostID:           host.ID,
		Token:            token,
		CAPEM:            caPEM,
	})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if len(enrollResult.CertPEM) == 0 || len(enrollResult.KeyPEM) == 0 {
		t.Fatalf("expected non-empty cert/key from enrollment")
	}

	// A second attempt with the same (now-burned) token must fail.
	if _, err := transport.Enroll(ctx, transport.EnrollConfig{
		ControlPlaneAddr: addr, HostID: host.ID, Token: token, CAPEM: caPEM,
	}); err == nil {
		t.Fatalf("expected second enrollment with the same token to fail")
	}

	updatedHost, err := store.GetHost(ctx, host.ID)
	if err != nil {
		t.Fatalf("GetHost: %v", err)
	}
	if !updatedHost.AgentEnrolled {
		t.Fatalf("expected host.AgentEnrolled to be true after enrollment")
	}

	// --- Authenticated client using the freshly issued cert ---
	client, err := transport.NewClient(addr, enrollResult.CertPEM, enrollResult.KeyPEM, caPEM)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	if err := client.Heartbeat(ctx, "27.3.1", "linux"); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	afterHeartbeat, err := store.GetHost(ctx, host.ID)
	if err != nil {
		t.Fatalf("GetHost after heartbeat: %v", err)
	}
	if afterHeartbeat.ConnectionStatus != models.ConnectionStatusOnline {
		t.Fatalf("expected host to be online after heartbeat, got %s", afterHeartbeat.ConnectionStatus)
	}
	if afterHeartbeat.DockerVersion != "27.3.1" {
		t.Fatalf("expected docker version to be recorded, got %q", afterHeartbeat.DockerVersion)
	}
	if afterHeartbeat.LastHeartbeatAt == nil {
		t.Fatalf("expected LastHeartbeatAt to be set")
	}

	telemetryResult, err := client.SendTelemetry(ctx,
		[]transport.TelemetryContainer{
			{DockerContainerID: "abc123", Name: "web", Image: "nginx:latest", Status: "running", HealthStatus: "healthy", RestartCount: 1},
		},
		[]models.Metric{
			{EntityType: "container", EntityID: "abc123", CPUPercent: 12.5, MemUsedBytes: 100_000_000,
				CollectedAt: time.Now().UTC(), Mode: models.ModeNormal, IntervalSeconds: 60},
		},
	)
	if err != nil {
		t.Fatalf("SendTelemetry: %v", err)
	}
	// Low CPU/no configured memory limit -> the FSM should not have
	// tripped critical, so the control plane hands back NORMAL/60s.
	if telemetryResult.Mode != models.ModeNormal || telemetryResult.IntervalSeconds != 60 {
		t.Fatalf("expected NORMAL/60s telemetry result for a quiet container, got %+v", telemetryResult)
	}

	containers, err := store.ListContainersByHost(ctx, host.ID)
	if err != nil {
		t.Fatalf("ListContainersByHost: %v", err)
	}
	if len(containers) != 1 || containers[0].Name != "web" || containers[0].RestartCount != 1 {
		t.Fatalf("unexpected containers after telemetry: %+v", containers)
	}

	latest, err := store.LatestMetric(ctx, "container", containers[0].ID)
	if err != nil {
		t.Fatalf("LatestMetric: %v", err)
	}
	if latest.CPUPercent != 12.5 {
		t.Fatalf("expected CPU 12.5, got %f", latest.CPUPercent)
	}

	// --- A cert issued for a different host must not be able to act as host-1 ---
	otherHost := &models.Host{ID: "host-2", Name: "other-host", Kind: models.HostKindRemote, Address: "10.0.0.2"}
	if err := store.CreateHost(ctx, otherHost); err != nil {
		t.Fatalf("CreateHost (other): %v", err)
	}
	otherToken, err := registry.IssueEnrollmentToken(ctx, otherHost.ID)
	if err != nil {
		t.Fatalf("IssueEnrollmentToken (other): %v", err)
	}
	otherResult, err := transport.Enroll(ctx, transport.EnrollConfig{
		ControlPlaneAddr: addr, HostID: otherHost.ID, Token: otherToken, CAPEM: caPEM,
	})
	if err != nil {
		t.Fatalf("Enroll (other): %v", err)
	}
	otherClient, err := transport.NewClient(addr, otherResult.CertPEM, otherResult.KeyPEM, caPEM)
	if err != nil {
		t.Fatalf("NewClient (other): %v", err)
	}
	if err := otherClient.Heartbeat(ctx, "1.0", "linux"); err != nil {
		t.Fatalf("Heartbeat (other): %v", err)
	}
	// host-2's heartbeat must not have touched host-1's record.
	host1Again, err := store.GetHost(ctx, host.ID)
	if err != nil {
		t.Fatalf("GetHost: %v", err)
	}
	if host1Again.DockerVersion != "27.3.1" {
		t.Fatalf("host-1's record was affected by host-2's heartbeat: %+v", host1Again)
	}
}
