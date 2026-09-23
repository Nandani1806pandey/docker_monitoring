// Package events provides a small helper around storage.EventStore so
// callers don't hand-build models.Event everywhere. The Monitoring Engine
// and Alert Engine (later milestones) will depend on this same Recorder.
package events

import (
	"context"

	"github.com/google/uuid"
	"github.com/nandani/docker-monitor/internal/controlplane/storage"
	"github.com/nandani/docker-monitor/internal/shared/models"
)

// Broadcaster is the narrow slice of ws.Hub that Recorder needs. Declared
// here (rather than importing package ws directly) so events doesn't have
// to depend on the WebSocket layer just to type its Recorder struct — a
// real *ws.Hub satisfies this trivially, and events has no idea WebSockets
// are even involved.
type Broadcaster interface {
	Broadcast(topic string, v interface{})
}

const eventsTopic = "events"

type Recorder struct {
	store       storage.EventStore
	broadcaster Broadcaster // nil until SetBroadcaster is called; every broadcast call checks
}

func NewRecorder(store storage.EventStore) *Recorder {
	return &Recorder{store: store}
}

// SetBroadcaster enables live event push over /ws/events (ARCHITECTURE.md
// §E.2, §14 "real-time updates"). Optional and nil-safe by design: every
// existing caller and test that built a Recorder via NewRecorder alone
// keeps working unchanged — this is additive wiring, not a constructor
// signature change that would ripple through every call site.
func (r *Recorder) SetBroadcaster(b Broadcaster) {
	r.broadcaster = b
}

type RecordInput struct {
	Type        string
	Severity    models.EventSeverity
	HostID      *string
	ContainerID *string
	UserID      *string
	Message     string
	Metadata    map[string]interface{}
}

func (r *Recorder) Record(ctx context.Context, in RecordInput) error {
	e := &models.Event{
		ID:          uuid.NewString(),
		Type:        in.Type,
		Severity:    in.Severity,
		HostID:      in.HostID,
		ContainerID: in.ContainerID,
		UserID:      in.UserID,
		Message:     in.Message,
		Metadata:    in.Metadata,
	}
	if err := r.store.AppendEvent(ctx, e); err != nil {
		return err
	}
	if r.broadcaster != nil {
		r.broadcaster.Broadcast(eventsTopic, e)
	}
	return nil
}
