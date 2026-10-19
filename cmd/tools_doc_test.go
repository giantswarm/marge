package cmd

import (
	"sync"
	"testing"

	"github.com/giantswarm/mcp-toolkit/tooldesc"
	"github.com/giantswarm/mcp-toolkit/tooldesc/tooldesctest"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"

	gh "github.com/giantswarm/marge/internal/github"
)

// docsServer registers every tool; both transports register the same ones.
var docsServer = sync.OnceValue(func() *server.MCPServer {
	return newMCPServer(gh.NewClient)
})

// TestToolsDocIsCurrent keeps docs/tools.md — every tool's description and
// input schema as a model reads them — equal to the registered tools.
// TOOLS_DOC_UPDATE=1 rewrites the file (`make tools-doc`).
func TestToolsDocIsCurrent(t *testing.T) {
	tooldesctest.AssertServerDoc(t, docsServer(),
		"# The tools\n\nGenerated from the registered tools (`make tools-doc`); through muster every tool is `x_marge_<name>`. "+
			"Both transports, stdio and streamable-http, register the same tools.",
		"../docs/tools.md")
}

// TestToolDescriptionBudgets holds the registered descriptions to the
// budget of giantswarm/giantswarm#38592.
func TestToolDescriptionBudgets(t *testing.T) {
	require.NoError(t, tooldesc.CheckServer(docsServer(), tooldesc.DefaultBudget))
}
