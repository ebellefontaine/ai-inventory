// Package mcpapi is the MCP surface of the inventory: one tool per
// service-layer operation, each a decode/call/encode wrapper with no logic
// of its own — the same discipline internal/httpapi holds itself to, for the
// same reason. The two surfaces delegate to one service layer so they cannot
// tell an agent two different things about the same operation.
//
// Every tool authenticates with the Authorization header carried on the
// underlying HTTP request (mcp.RequestExtra.Header), exactly as the HTTP API
// does — the same header, the same token formats, the same service-layer
// sentinels. See docs/plans/2026-08-16-milestone-4-mcp-surface.md for why.
package mcpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ebellefontaine/ai-inventory/internal/auth"
	"github.com/ebellefontaine/ai-inventory/internal/service"
)

const (
	serverName    = "ai-inventory"
	serverVersion = "v0.1.0"
)

// handlers holds the four services every tool delegates to. It exists as a
// type of its own, distinct from NewServer, so the tests in this package can
// call a handler directly without going through mcp.Server at all — the only
// way to exercise req.Extra with a hand-built value rather than a real
// transport.
type handlers struct {
	registration *service.RegistrationService
	heartbeat    *service.HeartbeatService
	update       *service.UpdateService
	query        *service.QueryService
}

func newHandlers(
	registration *service.RegistrationService,
	heartbeat *service.HeartbeatService,
	update *service.UpdateService,
	query *service.QueryService,
) *handlers {
	return &handlers{registration: registration, heartbeat: heartbeat, update: update, query: query}
}

// NewServer builds the MCP server and registers all five tools. Nothing here
// reads a team or an environment from a tool's arguments — the credential is
// the only source of scope, for the same reason internal/httpapi/agents.go
// never reads one from the request body.
func NewServer(
	registration *service.RegistrationService,
	heartbeat *service.HeartbeatService,
	update *service.UpdateService,
	query *service.QueryService,
) *mcp.Server {
	h := newHandlers(registration, heartbeat, update, query)
	server := mcp.NewServer(&mcp.Implementation{Name: serverName, Version: serverVersion}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name: "register_agent",
		Description: "Register this agent with AI Inventory, or update its record if an agent " +
			"with the same identity has already registered. Requires an enrollment token in the " +
			"Authorization header. The owning team and environment are inherited from that token " +
			"and cannot be set by the descriptor.",
	}, h.registerAgent)

	mcp.AddTool(server, &mcp.Tool{
		Name: "heartbeat",
		Description: "Signal that this agent is still running. Requires the per-agent credential " +
			"returned by register_agent, in the Authorization header. Carries no descriptor changes.",
	}, h.heartbeatTool)

	mcp.AddTool(server, &mcp.Tool{
		Name: "update_agent",
		Description: "Report a partial change to this agent's descriptor — only the fields " +
			"included are changed. Requires the per-agent credential in the Authorization header.",
	}, h.updateAgent)

	mcp.AddTool(server, &mcp.Tool{
		Name: "get_agent",
		Description: "Look up one agent's current record. Requires either an enrollment token " +
			"scoped to that agent's team and environment, or the agent's own per-agent credential, " +
			"in the Authorization header.",
	}, h.getAgent)

	mcp.AddTool(server, &mcp.Tool{
		Name: "search_agents",
		Description: "List agents within an enrollment token's own team and environment, most " +
			"recently active first. Requires an enrollment token in the Authorization header.",
	}, h.searchAgents)

	return server
}

// bearerToken extracts the credential from req, wrapped so every handler
// below reports a failure the same way this plan's Task 1 already made HTTP
// and MCP agree on: toolError classifies auth.ErrNoBearerToken as an
// authentication failure explicitly, alongside every sentinel in
// service.AuthSentinels — see toolError in errors.go.
//
// req.Extra is nil over the in-memory transport (no real HTTP request sits
// behind it), which is exactly the case Task 3's tests construct by hand; a
// nil Extra is treated as an empty header set rather than a panic, and an
// empty header set is precisely what auth.BearerToken already reports as
// ErrNoBearerToken.
func bearerToken(req *mcp.CallToolRequest) (string, error) {
	header := http.Header{}
	if req.Extra != nil {
		header = req.Extra.Header
	}
	return auth.BearerToken(header)
}

// --- register_agent -------------------------------------------------------

type registerAgentIn struct {
	// Descriptor is the agent's self-reported descriptor: name, purpose,
	// platform, hosting, source, telemetry_links, and any other fields —
	// identical in shape to the HTTP registration body. It is map[string]any
	// rather than json.RawMessage: this plan's research found that the
	// latter infers a byte-array schema, not an object schema, from the
	// SDK's automatic schema generation.
	Descriptor map[string]any `json:"descriptor" jsonschema:"the agent's self-reported descriptor, as a JSON object: name, purpose, platform, hosting, source, telemetry_links, and any other fields"`
}

type registerAgentOut struct {
	AgentID                  string   `json:"agent_id"`
	AgentToken               string   `json:"agent_token"`
	HeartbeatIntervalSeconds int      `json:"heartbeat_interval_seconds"`
	Flags                    []string `json:"flags"`
	Created                  bool     `json:"created"`
}

func (h *handlers) registerAgent(ctx context.Context, req *mcp.CallToolRequest, in registerAgentIn) (*mcp.CallToolResult, registerAgentOut, error) {
	token, err := bearerToken(req)
	if err != nil {
		return toolError(err), registerAgentOut{}, nil
	}

	raw, err := jsonMarshal(in.Descriptor)
	if err != nil {
		return toolError(err), registerAgentOut{}, nil
	}

	result, err := h.registration.Register(ctx, service.RegisterInput{
		EnrollmentToken: token,
		RawDescriptor:   raw,
	})
	if err != nil {
		return toolError(err), registerAgentOut{}, nil
	}

	return nil, registerAgentOut{
		AgentID:                  result.AgentID.String(),
		AgentToken:               result.AgentToken,
		HeartbeatIntervalSeconds: result.HeartbeatIntervalSeconds,
		Flags:                    jsonList(result.Flags),
		Created:                  result.Created,
	}, nil
}

// --- heartbeat --------------------------------------------------------------

type heartbeatIn struct {
	// AgentID is a string, not uuid.UUID: this plan's research found that
	// uuid.UUID's underlying [16]byte array infers an integer-array schema
	// from the SDK's reflection-based generator, not a string one, and a real
	// UUID string then fails schema validation. auth.AgentID converts it.
	AgentID string `json:"agent_id" jsonschema:"the agent id returned by register_agent"`
}

type heartbeatOut struct {
	HeartbeatIntervalSeconds int    `json:"heartbeat_interval_seconds"`
	LifecycleState           string `json:"lifecycle_state"`
	ReplicaCount             int    `json:"replica_count"`
}

func (h *handlers) heartbeatTool(ctx context.Context, req *mcp.CallToolRequest, in heartbeatIn) (*mcp.CallToolResult, heartbeatOut, error) {
	token, err := bearerToken(req)
	if err != nil {
		return toolError(err), heartbeatOut{}, nil
	}

	agentID, err := auth.AgentID(in.AgentID)
	if err != nil {
		return toolError(err), heartbeatOut{}, nil
	}

	result, err := h.heartbeat.Heartbeat(ctx, service.HeartbeatInput{
		AgentID:    agentID,
		AgentToken: token,
	})
	if err != nil {
		return toolError(err), heartbeatOut{}, nil
	}

	return nil, heartbeatOut{
		HeartbeatIntervalSeconds: result.HeartbeatIntervalSeconds,
		LifecycleState:           string(result.LifecycleState),
		ReplicaCount:             result.ReplicaCount,
	}, nil
}

// --- update_agent -----------------------------------------------------------

type updateAgentIn struct {
	// AgentID is a string for the same reason heartbeatIn's is; see there.
	AgentID    string         `json:"agent_id" jsonschema:"the agent id returned by register_agent"`
	Descriptor map[string]any `json:"descriptor" jsonschema:"the fields to change; fields not included are left alone"`
}

type updateAgentOut struct {
	Flags   []string `json:"flags"`
	Changed bool     `json:"changed"`
}

func (h *handlers) updateAgent(ctx context.Context, req *mcp.CallToolRequest, in updateAgentIn) (*mcp.CallToolResult, updateAgentOut, error) {
	token, err := bearerToken(req)
	if err != nil {
		return toolError(err), updateAgentOut{}, nil
	}

	agentID, err := auth.AgentID(in.AgentID)
	if err != nil {
		return toolError(err), updateAgentOut{}, nil
	}

	raw, err := jsonMarshal(in.Descriptor)
	if err != nil {
		return toolError(err), updateAgentOut{}, nil
	}

	result, err := h.update.Update(ctx, service.UpdateInput{
		AgentID:       agentID,
		AgentToken:    token,
		RawDescriptor: raw,
	})
	if err != nil {
		return toolError(err), updateAgentOut{}, nil
	}

	return nil, updateAgentOut{Flags: jsonList(result.Flags), Changed: result.Changed}, nil
}

// --- get_agent ----------------------------------------------------------

type getAgentIn struct {
	// AgentID is a string for the same reason heartbeatIn's is; see there.
	AgentID string `json:"agent_id" jsonschema:"the agent id to look up"`
}

type agentOut struct {
	ID                 string   `json:"id"`
	TeamSlug           string   `json:"team_slug"`
	Environment        string   `json:"environment"`
	DisplayName        string   `json:"display_name"`
	Purpose            string   `json:"purpose"`
	LifecycleState     string   `json:"lifecycle_state"`
	RegistrationSource string   `json:"registration_source"`
	ReplicaCount       int      `json:"replica_count"`
	Flags              []string `json:"flags"`
	FirstSeenAt        string   `json:"first_seen_at"`
	LastHeartbeatAt    string   `json:"last_heartbeat_at"`
}

func (h *handlers) getAgent(ctx context.Context, req *mcp.CallToolRequest, in getAgentIn) (*mcp.CallToolResult, agentOut, error) {
	token, err := bearerToken(req)
	if err != nil {
		return toolError(err), agentOut{}, nil
	}

	agentID, err := auth.AgentID(in.AgentID)
	if err != nil {
		return toolError(err), agentOut{}, nil
	}

	agent, err := h.query.GetAgent(ctx, service.GetAgentInput{AgentID: agentID, Credential: token})
	if err != nil {
		return toolError(err), agentOut{}, nil
	}

	return nil, toAgentOut(agent), nil
}

// --- search_agents ----------------------------------------------------------

type searchAgentsIn struct {
	LifecycleState string `json:"lifecycle_state,omitempty" jsonschema:"filter to one lifecycle state: active, idle, stale, retired, or quarantined; omit for all"`
	Limit          int    `json:"limit,omitempty" jsonschema:"maximum number of agents to return; defaults to 50, capped at 200"`
}

type searchAgentsOut struct {
	Agents []agentOut `json:"agents"`
}

func (h *handlers) searchAgents(ctx context.Context, req *mcp.CallToolRequest, in searchAgentsIn) (*mcp.CallToolResult, searchAgentsOut, error) {
	token, err := bearerToken(req)
	if err != nil {
		return toolError(err), searchAgentsOut{}, nil
	}

	results, err := h.query.SearchAgents(ctx, service.SearchAgentsInput{
		EnrollmentToken: token,
		LifecycleState:  in.LifecycleState,
		Limit:           in.Limit,
	})
	if err != nil {
		return toolError(err), searchAgentsOut{}, nil
	}

	out := searchAgentsOut{Agents: make([]agentOut, len(results))}
	for i, agent := range results {
		out.Agents[i] = toAgentOut(agent)
	}
	return nil, out, nil
}

func toAgentOut(a service.Agent) agentOut {
	return agentOut{
		ID:                 a.ID.String(),
		TeamSlug:           a.TeamSlug,
		Environment:        a.Environment,
		DisplayName:        a.DisplayName,
		Purpose:            a.Purpose,
		LifecycleState:     a.LifecycleState,
		RegistrationSource: a.RegistrationSource,
		ReplicaCount:       a.ReplicaCount,
		Flags:              jsonList(a.Flags),
		FirstSeenAt:        a.FirstSeenAt.Format(timeFormat),
		LastHeartbeatAt:    a.LastHeartbeatAt.Format(timeFormat),
	}
}

const timeFormat = "2006-01-02T15:04:05Z07:00" // time.RFC3339

// jsonList turns a nil slice into an empty one, matching
// internal/httpapi/agents.go's jsonList: a field that is sometimes [] and
// sometimes null is a field every client has to special-case.
func jsonList(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// jsonMarshal turns a decoded descriptor argument back into the bytes
// domain.DecodeDescriptor and service.RegisterInput/UpdateInput already
// accept — the map[string]any round trip this plan's research required in
// place of json.RawMessage.
func jsonMarshal(v map[string]any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode descriptor: %w", err)
	}
	return raw, nil
}
