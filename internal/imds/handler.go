package imds

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/giantswarm/vm-manager/internal/apierr"
)

const (
	textPlain       = "text/plain; charset=utf-8"
	applicationJSON = "application/json"
	octetStream     = "application/octet-stream"

	// maxJSONBody bounds /attest/quote (event logs) and /report uploads.
	maxJSONBody = 8 << 20

	// userDataGatedBody is the one-line 403 reason while /user-data is locked.
	userDataGatedBody = "user-data is released after the initrd-stage attestation verifies"
)

// Deps is what Handler serves from. Resolver, Attestor, Reports and Artifacts
// are required; Handler panics on a nil one, like http.NewServeMux does on a
// bad pattern, because that is a wiring bug, not a runtime condition.
type Deps struct {
	Resolver  Resolver
	Attestor  Attestor
	Reports   ReportSink
	Artifacts ArtifactSource
	// Log is the request logger; nil means slog.Default(). One debug line per
	// request, one info line per attestation outcome.
	Log *slog.Logger
	// ClientAddr extracts the caller's address. nil parses r.RemoteAddr, which
	// is the net.Conn peer on the in-stack listener; X-Forwarded-For is never
	// consulted. Inject it when the listener is fronted by something else.
	ClientAddr func(*http.Request) (netip.Addr, bool)
}

type server struct {
	Deps
}

// handlerFunc serves one key for an already resolved VM.
type handlerFunc func(s *server, w http.ResponseWriter, r *http.Request, inst Instance)

// routes maps every Keys entry to its handler; TestKeysRouted keeps them equal.
var routes = map[string]handlerFunc{
	"/hostname":           plain(func(i Instance) string { return cmp.Or(i.Hostname, i.Name) }),
	"/region":             plain(func(i Instance) string { return i.Region }),
	"/zone":               plain(func(i Instance) string { return i.Zone }),
	"/instance-id":        plain(func(i Instance) string { return i.ID }),
	"/kubernetes-version": plain(func(i Instance) string { return i.KubernetesVersion }),
	"/public-keys/":       (*server).publicKeyIndex,
	"/public-keys/0":      (*server).publicKey,
	"/metadata/":          (*server).metadata,
	"/user-data":          (*server).userData,
	"/attest/nonce":       (*server).attestNonce,
	"/attest/quote":       (*server).attestQuote,
	"/report":             (*server).report,
	"/sysupdate/":         (*server).sysupdate,
}

// Handler serves the IMDS contract under BasePath.
func Handler(d Deps) http.Handler {
	for _, dep := range []struct {
		name string
		nil  bool
	}{
		{"Resolver", d.Resolver == nil},
		{"Attestor", d.Attestor == nil},
		{"Reports", d.Reports == nil},
		{"Artifacts", d.Artifacts == nil},
	} {
		if dep.nil {
			panic("imds: Deps." + dep.name + " is nil")
		}
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.ClientAddr == nil {
		d.ClientAddr = remoteAddr
	}
	s := &server{Deps: d}

	mux := http.NewServeMux()
	for _, k := range Keys {
		h, ok := routes[k.Path]
		if !ok {
			panic("imds: no handler for key " + k.Path)
		}
		mux.Handle(k.Method+" "+BasePath+k.pattern(), s.resolved(h))
	}
	return s.logged(mux)
}

// remoteAddr is the default Deps.ClientAddr: the net.Conn peer, IPv4-unmapped.
func remoteAddr(r *http.Request) (netip.Addr, bool) {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, false
	}
	return ap.Addr().Unmap(), true
}

type ctxKey struct{}

// logged resolves the caller once per request, rejects unknown addresses with
// 403 before any routing happens, and writes the per-request debug line.
func (s *server) logged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w}
		inst, ok := s.resolve(r)
		if ok {
			next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), ctxKey{}, inst)))
		} else {
			writeText(sw, http.StatusForbidden, "forbidden: client address is not a managed VM")
		}
		s.Log.DebugContext(r.Context(), "imds request",
			"method", r.Method, "path", r.URL.Path, "status", sw.status,
			"client", r.RemoteAddr, "vm", inst.ID)
	})
}

func (s *server) resolve(r *http.Request) (Instance, bool) {
	ip, ok := s.ClientAddr(r)
	if !ok {
		return Instance{}, false
	}
	return s.Resolver.LookupByIP(r.Context(), ip)
}

// resolved hands the Instance stored by logged to a key handler.
func (s *server) resolved(h handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inst, _ := r.Context().Value(ctxKey{}).(Instance)
		h(s, w, r, inst)
	})
}

// plain serves a fixed-key value; an empty value is an unset key (404).
func plain(get func(Instance) string) handlerFunc {
	return func(_ *server, w http.ResponseWriter, _ *http.Request, inst Instance) {
		v := get(inst)
		if v == "" {
			notFound(w)
			return
		}
		writeText(w, http.StatusOK, v)
	}
}

func (s *server) publicKeyIndex(w http.ResponseWriter, _ *http.Request, inst Instance) {
	lines := make([]string, len(inst.SSHAuthorizedKeys))
	for i := range inst.SSHAuthorizedKeys {
		lines[i] = strconv.Itoa(i)
	}
	writeText(w, http.StatusOK, strings.Join(lines, "\n"))
}

func (s *server) publicKey(w http.ResponseWriter, r *http.Request, inst Instance) {
	n, err := strconv.Atoi(r.PathValue("index"))
	if err != nil || n < 0 || n >= len(inst.SSHAuthorizedKeys) {
		notFound(w)
		return
	}
	writeText(w, http.StatusOK, inst.SSHAuthorizedKeys[n])
}

func (s *server) metadata(w http.ResponseWriter, r *http.Request, inst Instance) {
	v, ok := inst.Metadata[r.PathValue("key")]
	if !ok {
		notFound(w)
		return
	}
	writeText(w, http.StatusOK, v)
}

func (s *server) userData(w http.ResponseWriter, _ *http.Request, inst Instance) {
	switch {
	case len(inst.UserData) == 0:
		notFound(w)
	case !inst.UserDataReleased:
		writeText(w, http.StatusForbidden, userDataGatedBody)
	default:
		writeText(w, http.StatusOK, string(inst.UserData))
	}
}

func (s *server) attestNonce(w http.ResponseWriter, r *http.Request, inst Instance) {
	nonce, err := s.Attestor.Nonce(r.Context(), inst.ID)
	if err != nil {
		s.Log.ErrorContext(r.Context(), "imds nonce", "vm", inst.ID, "err", err)
		writeText(w, http.StatusInternalServerError, "attestation unavailable")
		return
	}
	writeText(w, http.StatusOK, nonce)
}

func (s *server) attestQuote(w http.ResponseWriter, r *http.Request, inst Instance) {
	var req QuoteRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(&req); err != nil {
		writeText(w, http.StatusBadRequest, "malformed quote: "+err.Error())
		return
	}
	if err := req.validate(); err != nil {
		writeText(w, http.StatusBadRequest, err.Error())
		return
	}

	res, err := s.Attestor.SubmitQuote(r.Context(), inst.ID, req)
	if err != nil {
		s.Log.ErrorContext(r.Context(), "imds quote", "vm", inst.ID, "stage", req.Stage, "err", err)
		writeText(w, statusFor(err), "quote not processed")
		return
	}
	res.Stage = req.Stage
	res.UserDataReleased = ReleasesUserData(req.Stage, res.Verified)
	s.Log.InfoContext(r.Context(), "imds attestation",
		"vm", inst.ID, "stage", res.Stage, "verified", res.Verified,
		"user_data_released", res.UserDataReleased, "message", res.Message)

	status := http.StatusOK
	if !res.Verified {
		status = http.StatusForbidden
	}
	writeJSON(w, status, res)
}

func (q QuoteRequest) validate() error {
	switch {
	case !q.Stage.Valid():
		return fmt.Errorf("%w: stage must be %q or %q", apierr.ErrInvalid, StageInitrd, StageReady)
	case q.Nonce == "":
		return fmt.Errorf("%w: nonce is required", apierr.ErrInvalid)
	case len(q.AKPub) == 0 || len(q.Quote) == 0 || len(q.Signature) == 0:
		return fmt.Errorf("%w: ak_pub, quote and signature are required", apierr.ErrInvalid)
	case len(q.PCRs) == 0:
		return fmt.Errorf("%w: pcrs is required", apierr.ErrInvalid)
	}
	return nil
}

func (s *server) report(w http.ResponseWriter, r *http.Request, inst Instance) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONBody))
	if err != nil {
		writeText(w, http.StatusBadRequest, "report not read: "+err.Error())
		return
	}
	if !json.Valid(raw) {
		writeText(w, http.StatusBadRequest, "report is not valid JSON")
		return
	}
	if err := s.Reports.StoreReport(r.Context(), inst.ID, raw); err != nil {
		s.Log.ErrorContext(r.Context(), "imds report", "vm", inst.ID, "err", err)
		writeText(w, statusFor(err), "report not stored")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) sysupdate(w http.ResponseWriter, r *http.Request, inst Instance) {
	component, name := r.PathValue("component"), r.PathValue("file")
	f, err := s.Artifacts.Open(component, name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrInvalid) {
			notFound(w)
			return
		}
		s.Log.ErrorContext(r.Context(), "imds artifact", "vm", inst.ID, "component", component, "file", name, "err", err)
		writeText(w, http.StatusInternalServerError, "artifact unavailable")
		return
	}
	defer f.Close() //nolint:errcheck // read-only file

	st, err := f.Stat()
	if err != nil || st.IsDir() {
		notFound(w)
		return
	}
	w.Header().Set("Content-Type", octetStream)
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, f); err != nil {
		s.Log.DebugContext(r.Context(), "imds artifact copy", "vm", inst.ID, "file", name, "err", err)
	}
}

// statusFor maps a dependency error to the sentinel table in internal/apierr.
func statusFor(err error) int {
	switch {
	case errors.Is(err, apierr.ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, apierr.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, apierr.ErrConflict):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// notFound answers 404 with an empty body. The body matters: systemd-imdsd
// (261, src/imds/imdsd.c data_write_callback) aborts the transfer as soon as
// a response with status >= 300 delivers body bytes, and that curl write
// error wins over its own 404 handling, so the guest sees a generic
// io.systemd.System error instead of io.systemd.InstanceMetadata.KeyNotFound.
// Only the latter lets `systemd-imds --import` treat an absent /user-data as
// "nothing to import"; with a body systemd-imds-import.service fails and the
// guest boots degraded.
func notFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", textPlain)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusNotFound)
}

// writeText answers with a plain-text body exactly as given: no trailing
// newline is added, so systemd-imdsd gets the value verbatim.
func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", textPlain)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		writeText(w, http.StatusInternalServerError, "encoding response: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", applicationJSON)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// statusWriter records the status for the request log.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}
