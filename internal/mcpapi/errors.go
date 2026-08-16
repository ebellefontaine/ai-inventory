package mcpapi

import (
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ebellefontaine/ai-inventory/internal/domain"
	"github.com/ebellefontaine/ai-inventory/internal/service"
)

// toolError turns a service-layer error into the response an MCP client is
// allowed to see, mirroring internal/httpapi/errors.go's classify one-for-
// one: the same closed set of auth failures produces the same fixed bytes on
// this surface as on HTTP, and everything else degrades to a fixed internal-
// error message rather than leaking SQL context up through the service
// layer's wrapped errors.
//
// Every handler in server.go returns (toolError(err), zeroOut, nil) on a
// failure path, never (nil, zeroOut, err): the SDK's mcp.AddTool wrapper
// packs a returned error into CallToolResult.Content verbatim, which would
// leak the exact sentinel and its wrapped SQL context to the client. A
// handler that instead returns a populated *mcp.CallToolResult with a nil
// error is sent to the client exactly as written, with none of that
// automatic packing.
func toolError(err error) *mcp.CallToolResult {
	msg := "internal server error"
	switch {
	case service.IsAuthFailure(err):
		msg = "invalid credentials"
	case errors.Is(err, domain.ErrNameRequired):
		msg = "descriptor name is required"
	case errors.Is(err, service.ErrInvalidLifecycleState):
		msg = "invalid lifecycle state filter"
	}
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}
