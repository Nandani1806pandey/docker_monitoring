package docker

import (
	"context"
	"net/http"
	"net/url"
)

// ContainerSummary is the shape of one entry from GET /containers/json,
// trimmed to the fields the agent's discovery pass (ARCHITECTURE.md §9 —
// "Discover containers") actually consumes.
type ContainerSummary struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	ImageID string            `json:"ImageID"`
	Command string            `json:"Command"`
	Created int64             `json:"Created"`
	State   string            `json:"State"`
	Status  string            `json:"Status"`
	Labels  map[string]string `json:"Labels"`
}

// ContainerInspect is the shape of GET /containers/{id}/json, trimmed to
// the fields the FSM's critical-condition detection and auto-restart/
// health-check logic need (ARCHITECTURE.md §F.3, §15, §16).
type ContainerInspect struct {
	ID           string `json:"Id"`
	RestartCount int    `json:"RestartCount"`
	State        struct {
		Status  string `json:"Status"`
		Running bool   `json:"Running"`
		Health  *struct {
			Status string `json:"Status"`
		} `json:"Health,omitempty"`
	} `json:"State"`
}

// ListContainers returns all containers, running or not, matching
// ARCHITECTURE.md §9 ("Discover containers"). Callers filter by State
// themselves; the agent needs to see stopped/exited containers too
// (restart-count tracking, event detection on state transitions).
func (c *Client) ListContainers(ctx context.Context) ([]ContainerSummary, error) {
	var out []ContainerSummary
	err := c.do(ctx, http.MethodGet, "/containers/json?all=true", nil, &out)
	return out, err
}

// InspectContainer fetches full state for one container: health, restart
// count, exit code — the fields the FSM's critical-condition detection and
// auto-restart/health-check logic (ARCHITECTURE.md §F.3, §15, §16) need.
func (c *Client) InspectContainer(ctx context.Context, id string) (ContainerInspect, error) {
	var out ContainerInspect
	err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil, &out)
	return out, err
}

// ContainerAction is a management operation the control plane can instruct
// an agent to execute (ARCHITECTURE.md §E.3 ExecCommand). Every action here
// must correspond to an explicit, authorized instruction from the control
// plane — the agent never decides on its own to act, except for the
// auto-restart policy loop, which is itself a control-plane-configured
// policy (§15).
type ContainerAction string

const (
	ActionStart   ContainerAction = "start"
	ActionStop    ContainerAction = "stop"
	ActionRestart ContainerAction = "restart"
)

// Exec performs a start/stop/restart against a single container.
func (c *Client) Exec(ctx context.Context, id string, action ContainerAction) error {
	switch action {
	case ActionStart, ActionStop, ActionRestart:
		return c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/"+string(action), nil, nil)
	default:
		return &APIError{StatusCode: 400, Path: "/containers/" + id, Message: "unknown action: " + string(action)}
	}
}
