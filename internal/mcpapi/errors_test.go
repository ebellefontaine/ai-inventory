package mcpapi

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

// TestToolFailuresAreIndistinguishable is the MCP counterpart to
// internal/httpapi/agents_test.go's TestMalformedAgentIDIsTheSame401AsABadCredential
// and TestAnotherAgentsCredentialIsTheSame401: it proves the Critical finding
// from the whole-branch review is fixed, not just that each handler no
// longer returns a raw error.
//
// Four different handlers, four different underlying sentinels --
// auth.ErrNoBearerToken (no header at all), service.ErrTokenNotFound (a
// syntactically valid but never-minted enrollment token),
// service.ErrCredentialNotFound (a syntactically valid but never-issued
// agent credential), and auth.ErrNoBearerToken again but reached through
// auth.AgentID's malformed-uuid wrapping -- must all produce IsError==true
// and byte-identical Content. Before this fix, each of these reached the
// client as a distinct, human-readable error string (the raw
// service/auth error, sometimes wrapped in SQL context), which is exactly
// the enumeration oracle internal/httpapi/errors.go's classify exists to
// close on the HTTP surface. This test fails against the old
// return-the-raw-error behavior and passes against toolError.
func TestToolFailuresAreIndistinguishable(t *testing.T) {
	f := newMCPFixture(t)
	s := newHandlers(f.registration, f.heartbeat, f.update, f.query)
	ctx := context.Background()

	var results []*mcp.CallToolResult

	// 1. No Authorization header at all -> auth.ErrNoBearerToken.
	res, _, err := s.registerAgent(ctx, bearer(""), registerAgentIn{
		Descriptor: map[string]any{"name": "x", "purpose": "y"},
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.True(t, res.IsError)
	results = append(results, res)

	// 2. A syntactically valid but never-minted enrollment token ->
	// service.ErrTokenNotFound.
	res, _, err = s.registerAgent(ctx, bearer("aiinv_et_thistokendoesnotexist"), registerAgentIn{
		Descriptor: map[string]any{"name": "x", "purpose": "y"},
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.True(t, res.IsError)
	results = append(results, res)

	// 3. A syntactically valid but never-issued agent credential on
	// heartbeatTool -> service.ErrCredentialNotFound.
	res, _, err = s.heartbeatTool(ctx, bearer("aiinv_ac_thiscredentialdoesnotexist"), heartbeatIn{
		AgentID: uuid.New().String(),
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.True(t, res.IsError)
	results = append(results, res)

	// 4. A malformed agent id string on heartbeatTool -> the wrapped
	// auth.ErrNoBearerToken produced by auth.AgentID, reached only after
	// bearerToken itself succeeds.
	res, _, err = s.heartbeatTool(ctx, bearer("aiinv_ac_wellformedbutirrelevant"), heartbeatIn{
		AgentID: "not-a-uuid-at-all",
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.True(t, res.IsError)
	results = append(results, res)

	// All four must be byte-identical: same Content, not just the same
	// IsError flag.
	want := contentText(t, results[0])
	require.Equal(t, "invalid credentials", want)
	for i, r := range results[1:] {
		got := contentText(t, r)
		require.Equal(t, want, got, "failure mode %d produced different Content than failure mode 0", i+1)
		require.Len(t, r.Content, len(results[0].Content), "failure mode %d has a different Content shape", i+1)
	}
}

func contentText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	require.Len(t, res.Content, 1)
	text, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok, "expected a *mcp.TextContent, got %T", res.Content[0])
	return text.Text
}
