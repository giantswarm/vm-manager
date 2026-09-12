// Package api exposes the domain services twice from one process: REST/JSON
// under /api/v1 for the portal and cluster-manager, and MCP tools for muster.
// Both map onto the same service methods so behavior cannot drift between
// them.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	spec "github.com/giantswarm/vm-manager/api"
	"github.com/giantswarm/vm-manager/internal/apierr"
)

// Prefix is the REST base path.
const Prefix = "/api/v1"

// REST serves the JSON API.
type REST struct {
	svc Services
	log *slog.Logger
}

// NewREST builds the REST handler set.
func NewREST(svc Services, log *slog.Logger) *REST {
	if log == nil {
		log = slog.Default()
	}
	return &REST{svc: svc, log: log}
}

// Register mounts the routes on mux.
func (h *REST) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET "+Prefix+"/openapi.yaml", h.getOpenAPI)
	mux.HandleFunc("GET "+Prefix+"/host", h.getHost)
	// Everything else under the prefix answers with the JSON error body.
	mux.HandleFunc(Prefix+"/", h.notFound)
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (h *REST) getOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(spec.OpenAPI)
}

func (h *REST) getHost(w http.ResponseWriter, r *http.Request) {
	info, err := h.svc.Host.Get(r.Context())
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (h *REST) notFound(w http.ResponseWriter, r *http.Request) {
	h.writeError(w, fmt.Errorf("%w: no route %s %s", apierr.ErrNotFound, r.Method, r.URL.Path))
}

func (h *REST) writeError(w http.ResponseWriter, err error) {
	status, code := statusFor(err)
	if status >= http.StatusInternalServerError {
		h.log.Error("request failed", "status", status, "error", err)
	}
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: err.Error()}})
}

// statusFor maps domain errors (internal/apierr sentinels) to an HTTP status
// and a stable code clients can switch on; MCP tool errors carry the same
// code.
func statusFor(err error) (int, string) {
	switch {
	case errors.Is(err, apierr.ErrNotFound):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, apierr.ErrInvalid):
		return http.StatusBadRequest, "invalid_request"
	case errors.Is(err, apierr.ErrConflict):
		return http.StatusConflict, "conflict"
	case errors.Is(err, apierr.ErrUnsupported):
		return http.StatusNotImplemented, "unsupported"
	default:
		return http.StatusInternalServerError, "internal_error"
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
