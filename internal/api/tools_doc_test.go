package api_test

import (
	"sync"
	"testing"

	"github.com/giantswarm/mcp-toolkit/tooldesc"
	"github.com/giantswarm/mcp-toolkit/tooldesc/tooldesctest"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/giantswarm/vm-manager/internal/api"
	"github.com/giantswarm/vm-manager/internal/buildinfo"
)

// docsServer registers every tool; registration needs no services.
var docsServer = sync.OnceValue(func() *mcpserver.MCPServer {
	return api.NewMCPServer(api.Services{}, buildinfo.Info{Version: "docs"})
})

// TestToolsDocIsCurrent keeps docs/tools.md — every tool's description and
// input schema as a model reads them — equal to the registered tools.
// TOOLS_DOC_UPDATE=1 rewrites the file (`make tools-doc`).
func TestToolsDocIsCurrent(t *testing.T) {
	tooldesctest.AssertServerDoc(t, docsServer(),
		"# The tools\n\nGenerated from the registered tools (`make tools-doc`); through muster every tool is `x_<server>_<name>`. "+
			"The server registers every tool in every mode.",
		"../../docs/tools.md")
}

// TestToolDescriptionBudgets holds the registered descriptions to the
// budget of giantswarm/giantswarm#38592.
func TestToolDescriptionBudgets(t *testing.T) {
	if err := tooldesc.CheckServer(docsServer(), tooldesc.DefaultBudget); err != nil {
		t.Error(err)
	}
}
