package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/giantswarm/vm-manager/internal/api"
	"github.com/giantswarm/vm-manager/internal/buildinfo"
)

// recordSpans installs an in-memory tracer provider and the W3C propagator
// as the globals tracing.Init installs, restoring the previous ones after
// the test.
func recordSpans(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	prevProvider, prevPropagator := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(prevProvider)
		otel.SetTextMapPropagator(prevPropagator)
	})
	return exporter
}

func postMCP(t *testing.T, url, sessionID, traceparent, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url+"/mcp", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		req.Header.Set(mcpserver.HeaderKeySessionID, sessionID)
	}
	if traceparent != "" {
		req.Header.Set("traceparent", traceparent)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return resp
}

func TestToolCallIsTracedAndJoinsTheCallersTrace(t *testing.T) {
	exporter := recordSpans(t)

	svc := bareHost(t)
	mcpSrv := api.NewMCPServer(svc, buildinfo.Info{Version: "test"})
	mcpSrv.AddTool(mcp.NewTool("echo"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})
	srv, err := New(Config{Addr: "127.0.0.1:0"}, svc, mcpSrv, nil)
	require.NoError(t, err)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		resp, err := http.Get(ts.URL + path)
		require.NoError(t, err)
		_ = resp.Body.Close()
	}
	require.Empty(t, exporter.GetSpans(), "the probes and the exposition are not traced")

	init := postMCP(t, ts.URL, "", "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`)
	sessionID := init.Header.Get(mcpserver.HeaderKeySessionID)
	exporter.Reset()

	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	postMCP(t, ts.URL, sessionID, "00-"+traceID+"-00f067aa0ba902b7-01",
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{}}}`)

	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range exporter.GetSpans().Snapshots() {
		require.Equal(t, traceID, s.SpanContext().TraceID().String(), "span %s joins the caller's trace", s.Name())
		byName[s.Name()] = s
	}
	require.Contains(t, byName, "POST /mcp")
	require.Contains(t, byName, "mcp.tools/call")
	require.Contains(t, byName, "tool.echo")

	httpSpan := byName["POST /mcp"]
	require.Equal(t, "00f067aa0ba902b7", httpSpan.Parent().SpanID().String())
	call := byName["mcp.tools/call"]
	require.Equal(t, trace.SpanKindServer, call.SpanKind())
	require.Equal(t, httpSpan.SpanContext().SpanID(), call.Parent().SpanID())
	attrs := map[string]string{}
	for _, kv := range call.Attributes() {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	require.Equal(t, "echo", attrs[api.AttrGenAIToolName])
	require.Equal(t, "echo", attrs["mcp.tool.name"])
	require.Equal(t, "tools/call", attrs["mcp.method"])
	require.Equal(t, call.SpanContext().SpanID(), byName["tool.echo"].Parent().SpanID())
}
