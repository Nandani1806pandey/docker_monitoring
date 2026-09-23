// Package ws implements the browser-facing WebSocket layer from
// ARCHITECTURE.md §E.2: real-time push for events and live metric streams,
// so the dashboard doesn't have to poll the REST API to see updates.
//
// The hub is deliberately domain-agnostic: it knows about "topics" (opaque
// strings) and byte payloads, nothing about hosts, containers, or events.
// Callers (events.Recorder, agentregistry.Registry) own the mapping from
// "a host came online" to "broadcast JSON to topic events" — this package
// only has to get bytes to the right set of open connections, in the face
// of slow or dead clients, without leaking goroutines.
package ws

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// writeWait is how long a single write (including a ping) is allowed
	// to block before the connection is considered dead.
	writeWait = 10 * time.Second
	// pongWait is how long we'll wait for a pong before giving up on a
	// client that's stopped responding — keeps a half-open TCP connection
	// (e.g. a laptop that went to sleep) from pinning resources forever.
	pongWait = 60 * time.Second
	// pingPeriod must be less than pongWait, so a ping always has time to
	// provoke a pong before the read deadline expires.
	pingPeriod = (pongWait * 9) / 10
	// sendBuffer is how many pending messages a single slow client is
	// allowed to accumulate before we drop it rather than block the
	// broadcaster (§28: "the central server should not [let one consumer]
	// request unnecessary [buffering]" — the adaptive-monitoring
	// equivalent of backpressure for a fan-out, since a browser tab that
	// stopped reading must never slow down every other subscriber).
	sendBuffer = 32
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	// CORS is intentionally permissive at the WebSocket layer for this
	// build: the auth stub (api.withAuthStub) already gates every route
	// that reaches ServeTopic, matching ARCHITECTURE.md §G's "every
	// WebSocket upgrade is authenticated at handshake" — origin checking
	// is a separate, additive hardening step for the real auth milestone,
	// not a substitute for it.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Hub fans messages out to every client currently subscribed to a topic.
// Safe for concurrent use.
type Hub struct {
	logger *slog.Logger

	mu     sync.RWMutex
	topics map[string]map[*client]struct{}
}

func NewHub(logger *slog.Logger) *Hub {
	return &Hub{logger: logger, topics: make(map[string]map[*client]struct{})}
}

// Broadcast marshals v to JSON once and fans it out to every current
// subscriber of topic. A client whose send buffer is already full is
// dropped (its connection closed) rather than allowed to block this call
// or grow an unbounded queue — see sendBuffer's doc comment.
func (h *Hub) Broadcast(topic string, v interface{}) {
	payload, err := json.Marshal(v)
	if err != nil {
		h.logger.Error("ws: failed to marshal broadcast payload", "topic", topic, "err", err)
		return
	}

	h.mu.RLock()
	subs := h.topics[topic]
	// Copy the client list out while holding the read lock, then release
	// it before doing any I/O-adjacent work (channel sends) — holding a
	// lock across a potentially-blocking-if-not-for-default select would
	// serialize every subscribe/unsubscribe behind however long this
	// broadcast takes, on top of not being necessary for correctness.
	clients := make([]*client, 0, len(subs))
	for c := range subs {
		clients = append(clients, c)
	}
	h.mu.RUnlock()

	for _, c := range clients {
		if !c.trySend(payload) {
			h.logger.Warn("ws: dropping slow or disconnected client", "topic", topic)
			c.closeOnce.Do(func() { close(c.send) })
		}
	}
}

func (h *Hub) subscribe(topic string, c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.topics[topic] == nil {
		h.topics[topic] = make(map[*client]struct{})
	}
	h.topics[topic][c] = struct{}{}
}

func (h *Hub) unsubscribe(topic string, c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if subs, ok := h.topics[topic]; ok {
		delete(subs, c)
		if len(subs) == 0 {
			delete(h.topics, topic)
		}
	}
}

// SubscriberCount reports how many clients are currently on a topic —
// exposed for tests and for a future §37 observability endpoint
// ("WebSocket connections").
func (h *Hub) SubscriberCount(topic string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.topics[topic])
}

type client struct {
	conn      *websocket.Conn
	send      chan []byte
	closeOnce sync.Once
}

// trySend attempts a non-blocking send and reports whether it succeeded.
// It also treats "send on a closed channel" as a false result rather than
// letting it panic: the client's send channel can be closed concurrently
// from two independent places — Broadcast's slow-client drop, and the
// read pump's disconnect handler (see ServeTopic) — and closeOnce only
// prevents a double *close*, not a send racing a close that already
// happened a moment earlier. A goroutine can pass the `case c.send <-
// payload` guard's compiler-visible safety only up to the point the
// channel closes; recover() here is the actual mutual-exclusion boundary
// for that narrow window, not a substitute for closeOnce (which is still
// needed to prevent the close itself from panicking).
func (c *client) trySend(payload []byte) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	select {
	case c.send <- payload:
		return true
	default:
		return false
	}
}

// ServeTopic upgrades the request to a WebSocket and blocks, relaying
// every message broadcast to topic until the client disconnects (or the
// request context is canceled). Intended to be called directly from an
// http.HandlerFunc — it does not return until the connection is done, so
// the calling handler's lifetime is the connection's lifetime, which is
// exactly what an http.Server expects.
func (h *Hub) ServeTopic(w http.ResponseWriter, r *http.Request, topic string) error {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return err
	}

	c := &client{conn: conn, send: make(chan []byte, sendBuffer)}
	h.subscribe(topic, c)

	// The read pump's only real job is to notice disconnects/errors and
	// keep the pong handler serviced — this hub is push-only, so any
	// message an actual client sends us is discarded, not routed anywhere.
	// On exit (for any reason: client closed, network error, ping
	// timeout) it closes c.send, which is what tells writePump to stop —
	// there is no other signal path from "client is gone" to the write
	// side, so this close must not be skipped on any exit route.
	go func() {
		defer c.closeOnce.Do(func() { close(c.send) })
		conn.SetReadDeadline(time.Now().Add(pongWait))
		conn.SetPongHandler(func(string) error {
			conn.SetReadDeadline(time.Now().Add(pongWait))
			return nil
		})
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	h.writePump(c) // blocks until c.send is closed or a write fails
	h.unsubscribe(topic, c)
	conn.Close()
	return nil
}

// writePump owns conn's write side exclusively (gorilla/websocket
// connections are not safe for concurrent writes from multiple
// goroutines), relaying queued broadcasts and periodic pings until the
// send channel is closed (by Broadcast's slow-client drop) or a write
// fails (client gone).
func (h *Hub) writePump(c *client) {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()

	for {
		select {
		case msg, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// Channel closed (either normal shutdown or the
				// slow-client drop in Broadcast) — send a clean close
				// frame, best-effort.
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
