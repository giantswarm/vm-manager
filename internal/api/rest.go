// Package api exposes the domain services twice from one process: REST/JSON
// under /api/v1 for the portal and cluster-manager, and MCP tools for muster.
// Both map onto the same service methods so behavior cannot drift between
// them.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	spec "github.com/giantswarm/vm-manager/api"
	"github.com/giantswarm/vm-manager/internal/apierr"
	"github.com/giantswarm/vm-manager/internal/vm"
)

// Prefix is the REST base path.
const Prefix = "/api/v1"

// maxBodyBytes bounds request bodies; user_data (Ignition JSON with embedded
// certificates) is the largest field.
const maxBodyBytes = 8 << 20

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

// Register mounts the routes on mux. Each route mirrors one MCP tool.
func (h *REST) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET "+Prefix+"/openapi.yaml", h.getOpenAPI)
	mux.HandleFunc("GET "+Prefix+"/host", h.getHost)

	mux.HandleFunc("GET "+Prefix+"/images", h.listImages)
	mux.HandleFunc("GET "+Prefix+"/images/{ref}", h.getImage)

	mux.HandleFunc("GET "+Prefix+"/networks", h.listNetworks)
	mux.HandleFunc("POST "+Prefix+"/networks", h.createNetwork)
	mux.HandleFunc("GET "+Prefix+"/networks/{name}", h.getNetwork)
	mux.HandleFunc("DELETE "+Prefix+"/networks/{name}", h.deleteNetwork)

	mux.HandleFunc("GET "+Prefix+"/vms", h.listVMs)
	mux.HandleFunc("POST "+Prefix+"/vms", h.createVM)
	mux.HandleFunc("GET "+Prefix+"/vms/{id}", h.getVM)
	mux.HandleFunc("DELETE "+Prefix+"/vms/{id}", h.deleteVM)
	mux.HandleFunc("POST "+Prefix+"/vms/{id}/start", h.vmOp(h.svc.VM.Start))
	mux.HandleFunc("POST "+Prefix+"/vms/{id}/stop", h.vmOp(h.svc.VM.Stop))
	mux.HandleFunc("POST "+Prefix+"/vms/{id}/reboot", h.vmOp(h.svc.VM.Reboot))
	mux.HandleFunc("POST "+Prefix+"/vms/{id}/exec", h.execVM)
	mux.HandleFunc("POST "+Prefix+"/vms/{id}/forward", h.forwardPort)
	mux.HandleFunc("GET "+Prefix+"/vms/{id}/console", h.getVMConsole)
	mux.HandleFunc("GET "+Prefix+"/vms/{id}/attestation", h.getVMAttestation)
	mux.HandleFunc("GET "+Prefix+"/vms/{id}/metrics", h.getVMMetrics)
	// The raw report has no MCP twin: a systemd-report is hundreds of KB.
	mux.HandleFunc("GET "+Prefix+"/vms/{id}/report", h.getVMReport)

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
	h.respond(w, http.StatusOK, info, err)
}

func (h *REST) listImages(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.svc.Images.List())
}

func (h *REST) getImage(w http.ResponseWriter, r *http.Request) {
	img, err := h.svc.Images.Get(r.PathValue("ref"))
	h.respond(w, http.StatusOK, img, err)
}

func (h *REST) listNetworks(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.svc.VM.ListNetworks())
}

func (h *REST) createNetwork(w http.ResponseWriter, r *http.Request) {
	var body CreateNetworkRequest
	if !h.decode(w, r, &body) {
		return
	}
	n, err := h.svc.VM.CreateNetwork(r.Context(), body.spec())
	h.respond(w, http.StatusCreated, n, err)
}

func (h *REST) getNetwork(w http.ResponseWriter, r *http.Request) {
	n, err := h.svc.VM.GetNetwork(r.PathValue("name"))
	h.respond(w, http.StatusOK, n, err)
}

func (h *REST) deleteNetwork(w http.ResponseWriter, r *http.Request) {
	_, err := h.svc.deleteNetwork(r.Context(), r.PathValue("name"))
	h.respond(w, http.StatusNoContent, nil, err)
}

func (h *REST) listVMs(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.svc.VM.List())
}

func (h *REST) createVM(w http.ResponseWriter, r *http.Request) {
	var body CreateVMRequest
	if !h.decode(w, r, &body) {
		return
	}
	v, err := h.svc.createVM(r.Context(), body)
	h.respond(w, http.StatusCreated, v, err)
}

func (h *REST) getVM(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.VM.Get(r.PathValue("id"))
	h.respond(w, http.StatusOK, v, err)
}

func (h *REST) deleteVM(w http.ResponseWriter, r *http.Request) {
	_, err := h.svc.deleteVM(r.Context(), r.PathValue("id"))
	h.respond(w, http.StatusNoContent, nil, err)
}

// vmOp adapts a lifecycle method taking the VM id.
func (h *REST) vmOp(op func(ctx context.Context, id string) (*vm.VM, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := op(r.Context(), r.PathValue("id"))
		h.respond(w, http.StatusOK, v, err)
	}
}

func (h *REST) execVM(w http.ResponseWriter, r *http.Request) {
	var body ExecRequest
	if !h.decode(w, r, &body) {
		return
	}
	res, err := h.svc.VM.Exec(r.Context(), r.PathValue("id"), body.Command)
	h.respond(w, http.StatusOK, res, err)
}

func (h *REST) forwardPort(w http.ResponseWriter, r *http.Request) {
	var body ForwardRequest
	if !h.decode(w, r, &body) {
		return
	}
	res, err := h.svc.forward(r.Context(), r.PathValue("id"), body.Port)
	h.respond(w, http.StatusOK, res, err)
}

func (h *REST) getVMConsole(w http.ResponseWriter, r *http.Request) {
	lines := DefaultConsoleLines
	if q := r.URL.Query().Get("lines"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil {
			h.writeError(w, fmt.Errorf("%w: lines: %v", apierr.ErrInvalid, err))
			return
		}
		lines = n
	}
	res, err := h.svc.console(r.PathValue("id"), lines)
	h.respond(w, http.StatusOK, res, err)
}

func (h *REST) getVMAttestation(w http.ResponseWriter, r *http.Request) {
	att, err := h.svc.VM.Attestation(r.PathValue("id"))
	h.respond(w, http.StatusOK, att, err)
}

func (h *REST) getVMMetrics(w http.ResponseWriter, r *http.Request) {
	res, err := h.svc.metrics(r.PathValue("id"))
	h.respond(w, http.StatusOK, res, err)
}

// getVMReport serves the guest's last upload, re-encoded (indented, HTML
// escaped) rather than echoed byte for byte since the guest wrote it. The
// IMDS only stores valid JSON; anything else is served as a JSON string.
func (h *REST) getVMReport(w http.ResponseWriter, r *http.Request) {
	report, err := h.svc.report(r.PathValue("id"))
	if err != nil {
		h.writeError(w, err)
		return
	}
	if json.Valid(report) {
		writeJSON(w, http.StatusOK, json.RawMessage(report))
		return
	}
	writeJSON(w, http.StatusOK, string(report))
}

func (h *REST) notFound(w http.ResponseWriter, r *http.Request) {
	h.writeError(w, fmt.Errorf("%w: no route %s %s", apierr.ErrNotFound, r.Method, r.URL.Path))
}

// decode reads a JSON body into v; unknown fields and oversized bodies are
// invalid requests. It answers the error itself and returns false.
func (h *REST) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		h.writeError(w, fmt.Errorf("%w: body: %v", apierr.ErrInvalid, err))
		return false
	}
	return true
}

// respond writes v with status, or the error body; a 204 carries no body.
func (h *REST) respond(w http.ResponseWriter, status int, v any, err error) {
	if err != nil {
		h.writeError(w, err)
		return
	}
	if status == http.StatusNoContent {
		w.WriteHeader(status)
		return
	}
	writeJSON(w, status, v)
}

func (h *REST) writeError(w http.ResponseWriter, err error) {
	status, code := statusFor(err)
	if status >= http.StatusInternalServerError {
		h.log.Error("request failed", "status", status, "error", err)
	}
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: err.Error()}})
}

// statusFor maps domain errors (internal/apierr sentinels plus the VM
// service's milestone errors) to an HTTP status and a stable code clients can
// switch on; MCP tool errors carry the same code.
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
	case errors.Is(err, vm.ErrTimeout):
		return http.StatusGatewayTimeout, "timeout"
	case errors.Is(err, vm.ErrFailed):
		return http.StatusInternalServerError, "vm_failed"
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
