package ws

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func testHub() *Hub {
	return NewHub(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// newTestServer wires a Hub's ServeTopic into a real httptest.Server and
// returns a function to dial a ws:// connection to it for a given topic.
func newTestServer(t *testing.T, hub *Hub) (dial func(topic string) *websocket.Conn) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		topic := strings.TrimPrefix(r.URL.Path, "/")
		_ = hub.ServeTopic(w, r, topic)
	}))
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	return func(topic string) *websocket.Conn {
		conn, _, err := websocket.DefaultDialer.Dial(wsURL+"/"+topic, nil)
		if err != nil {
			t.Fatalf("dial topic %q: %v", topic, err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
}

// waitForSubscribers polls until a topic has at least n subscribers, since
// ServeTopic subscribes asynchronously relative to the dial call returning.
func waitForSubscribers(t *testing.T, hub *Hub, topic string, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hub.SubscriberCount(topic) >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d subscriber(s) on topic %q, got %d", n, topic, hub.SubscriberCount(topic))
}

func TestBroadcast_SingleSubscriberReceivesMessage(t *testing.T) {
	hub := testHub()
	dial := newTestServer(t, hub)

	conn := dial("events")
	waitForSubscribers(t, hub, "events", 1)

	hub.Broadcast("events", map[string]string{"type": "host.connected", "message": "hi"})

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(msg, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["type"] != "host.connected" || got["message"] != "hi" {
		t.Fatalf("unexpected payload: %+v", got)
	}
}

func TestBroadcast_MultipleSubscribersAllReceive(t *testing.T) {
	hub := testHub()
	dial := newTestServer(t, hub)

	var conns []*websocket.Conn
	for i := 0; i < 3; i++ {
		conns = append(conns, dial("events"))
	}
	waitForSubscribers(t, hub, "events", 3)

	hub.Broadcast("events", map[string]int{"n": 42})

	for i, c := range conns {
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, msg, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("subscriber %d ReadMessage: %v", i, err)
		}
		var got map[string]int
		json.Unmarshal(msg, &got)
		if got["n"] != 42 {
			t.Fatalf("subscriber %d got unexpected payload: %+v", i, got)
		}
	}
}

// TestBroadcast_TopicIsolation is the case that would silently leak data
// across dashboards if wrong: a subscriber to one container's stats topic
// must never see another container's broadcasts, or a host's events.
func TestBroadcast_TopicIsolation(t *testing.T) {
	hub := testHub()
	dial := newTestServer(t, hub)

	connA := dial("containers/a/stats")
	connB := dial("containers/b/stats")
	waitForSubscribers(t, hub, "containers/a/stats", 1)
	waitForSubscribers(t, hub, "containers/b/stats", 1)

	hub.Broadcast("containers/a/stats", map[string]string{"container": "a"})

	connA.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, err := connA.ReadMessage()
	if err != nil {
		t.Fatalf("connA ReadMessage: %v", err)
	}
	var got map[string]string
	json.Unmarshal(msg, &got)
	if got["container"] != "a" {
		t.Fatalf("connA got wrong payload: %+v", got)
	}

	// connB must NOT receive anything — give it a short deadline and
	// expect a timeout, not a message.
	connB.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, _, err := connB.ReadMessage(); err == nil {
		t.Fatalf("connB should not have received container A's broadcast")
	}
}

func TestUnsubscribe_OnDisconnect(t *testing.T) {
	hub := testHub()
	dial := newTestServer(t, hub)

	conn := dial("events")
	waitForSubscribers(t, hub, "events", 1)

	conn.Close()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hub.SubscriberCount("events") == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("expected subscriber count to drop to 0 after disconnect, got %d", hub.SubscriberCount("events"))
}

// TestBroadcast_DropsClientWhoseBufferIsFull is the backpressure guarantee
// (§28), tested at the level this package actually controls: Broadcast
// must close a client's send channel once it's full and undrained, rather
// than blocking the broadcaster or growing the queue unboundedly.
//
// This is deliberately NOT driven over a real socket: OS-level TCP send
// buffers on loopback (tens of KB to several MB, depending on autotuning)
// happily absorb a flood of small messages well past what would fill our
// 32-slot application buffer, since a write() call only blocks once the
// SENDER's kernel buffer is full — not when the receiver stops reading.
// Making that actually block deterministically would mean fighting kernel
// socket-buffer tuning rather than testing this package's own logic, so
// this test drives the client's channel directly instead.
func TestBroadcast_DropsClientWhoseBufferIsFull(t *testing.T) {
	hub := testHub()
	c := &client{send: make(chan []byte, sendBuffer)}
	hub.subscribe("events", c)

	// Nobody ever reads from c.send. Once more messages than the buffer
	// holds are broadcast, the (sendBuffer+1)-th must find the channel
	// full and close it — without Broadcast itself ever blocking.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < sendBuffer+5; i++ {
			hub.Broadcast("events", map[string]int{"i": i})
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Broadcast blocked instead of dropping the full client")
	}

	// The channel holds up to sendBuffer buffered items; drain them all to
	// confirm it was actually closed (not just full) once drained.
	closed := false
	for i := 0; i < sendBuffer+10; i++ {
		select {
		case _, open := <-c.send:
			if !open {
				closed = true
			}
		default:
		}
		if closed {
			break
		}
	}
	if !closed {
		t.Fatalf("expected c.send to be closed after overflow, but draining it never hit a closed read")
	}
}

// TestFullPipeline_ChannelCloseDisconnectsRealConnection confirms the
// other half of the contract with an actual WebSocket connection: once a
// client's send channel is closed (whatever the reason), writePump must
// exit and ServeTopic must unsubscribe it — the same teardown path
// TestUnsubscribe_OnDisconnect exercises from the read side, exercised
// here from the write/backpressure side instead.
func TestFullPipeline_ChannelCloseDisconnectsRealConnection(t *testing.T) {
	hub := testHub()
	dial := newTestServer(t, hub)

	conn := dial("events")
	waitForSubscribers(t, hub, "events", 1)

	hub.mu.RLock()
	var c *client
	for cl := range hub.topics["events"] {
		c = cl
	}
	hub.mu.RUnlock()
	if c == nil {
		t.Fatalf("expected exactly one client registered")
	}

	// Simulate what Broadcast does on overflow, directly and deterministically.
	c.closeOnce.Do(func() { close(c.send) })

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hub.SubscriberCount("events") == 0 {
			conn.Close()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("expected connection to be torn down after its channel closed, subscriber count = %d", hub.SubscriberCount("events"))
}
