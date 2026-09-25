package api

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	mcpotel "github.com/mark3labs/mcp-go/otel"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// TracerName names the tracer of vm-manager's own spans.
const TracerName = "github.com/giantswarm/vm-manager"

// AttrGenAIToolName is the OpenTelemetry GenAI key naming the tool a
// tools/call runs, the key muster puts on its side of the same call.
const AttrGenAIToolName = "gen_ai.tool.name"

// tracingOptions installs mcp-go's server tracing (an mcp.<method> server
// span per JSON-RPC request, a tool.<name> span around each handler) on the
// global tracer provider. The propagator extracts nothing: the HTTP server
// span in the request context already joined the caller's traceparent, and
// the MCP span nests under it. toolNameSpan is registered first so it wraps
// the tracing middleware and sees the mcp.tools/call server span.
func tracingOptions() []mcpserver.ServerOption {
	return []mcpserver.ServerOption{
		mcpserver.WithToolHandlerMiddleware(toolNameSpan),
		mcpotel.WithServerTracingPropagator(otel.Tracer(TracerName), propagation.NewCompositeTextMapPropagator()),
	}
}

func toolNameSpan(next mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		trace.SpanFromContext(ctx).SetAttributes(attribute.String(AttrGenAIToolName, req.Params.Name))
		return next(ctx, req)
	}
}
