package protocol

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/JustIkra/umir-feedback/internal/adapters/journal"
)

type Request struct {
	Version   int             `json:"version"`
	Project   string          `json:"project"`
	RequestID string          `json:"requestID"`
	Operation string          `json:"operation"`
	Data      json.RawMessage `json:"data"`
}
type Fault struct {
	Code      string `json:"code"`
	Effect    string `json:"effect"`
	Retryable bool   `json:"retryable"`
}
type Response struct {
	Version   int    `json:"version"`
	Project   string `json:"project"`
	RequestID string `json:"requestID"`
	Operation string `json:"operation"`
	Data      any    `json:"data,omitempty"`
	Error     *Fault `json:"error,omitempty"`
}
type ReadyState struct {
	Target       string   `json:"target"`
	InstanceID   string   `json:"instanceID"`
	Protocol     string   `json:"protocol"`
	BinarySHA256 string   `json:"binarySHA256"`
	ConfigSHA256 string   `json:"configSHA256"`
	Ready        bool     `json:"ready"`
	Capabilities []string `json:"capabilities"`
}
type HandlerOptions struct {
	Project, Credential, BinarySHA256, ConfigSHA256 string
	Clock                                           *Clock
	Store                                           *journal.Store
	Faults                                          *Faults
	Ready                                           func(context.Context) bool
}
type cachedResponse struct {
	canonical []byte
	done      chan struct{}
	body      []byte
	status    int
}
type Handler struct {
	opts  HandlerOptions
	slots chan struct{}
	mu    sync.Mutex
	cache map[string]*cachedResponse
}

func NewHandler(opts HandlerOptions) (*Handler, error) {
	if !projectPattern.MatchString(opts.Project) || !digestPattern.MatchString(opts.Credential) || opts.Clock == nil || opts.Store == nil || opts.Faults == nil || opts.Ready == nil || !digestPattern.MatchString(opts.BinarySHA256) || !digestPattern.MatchString(opts.ConfigSHA256) {
		return nil, errors.New("invalid control dependencies")
	}
	return &Handler{opts: opts, slots: make(chan struct{}, 8), cache: map[string]*cachedResponse{}}, nil
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := UUID()
	if err != nil {
		w.WriteHeader(503)
		return
	}
	input := Request{Version: 1, Project: h.opts.Project, RequestID: id, Operation: "invalid"}
	fail := func(code string) { h.write(w, h.failure(input, code), statusFor(code)) }
	if r.Method != "POST" || r.URL.Path != ControlPath || r.URL.RawQuery != "" || r.URL.ForceQuery {
		fail("InvalidRequest")
		return
	}
	auth := r.Header.Values("Authorization")
	if len(auth) != 1 || subtle.ConstantTimeCompare([]byte(auth[0]), []byte("Bearer "+h.opts.Credential)) != 1 {
		fail("Unauthenticated")
		return
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		fail("CapacityExceeded")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	content, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || content != "application/json" {
		fail("InvalidRequest")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxRequest))
	if err != nil {
		fail("InvalidRequest")
		return
	}
	if err := validateEnvelope(body); err != nil {
		fail("InvalidRequest")
		return
	}
	if err := json.Unmarshal(body, &input); err != nil {
		fail("InvalidRequest")
		return
	}
	if input.Project != h.opts.Project {
		fail("WrongProject")
		return
	}
	name := strings.ReplaceAll(input.Operation, ".", "_") + "_request"
	if _, ok := schemas[name]; !ok {
		input = Request{Version: 1, Project: h.opts.Project, RequestID: id, Operation: "invalid"}
		fail("InvalidRequest")
		return
	}
	if err := validate(name, body); err != nil {
		fail("InvalidRequest")
		return
	}
	if !authOperation(input.Operation) {
		fail("UnsupportedOperation")
		return
	}
	var canonicalValue any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&canonicalValue); err != nil {
		fail("InvalidRequest")
		return
	}
	canonical, err := json.Marshal(canonicalValue)
	if err != nil {
		fail("InvalidRequest")
		return
	}
	h.mu.Lock()
	if prior, ok := h.cache[input.RequestID]; ok {
		h.mu.Unlock()
		if !bytes.Equal(prior.canonical, canonical) {
			fail("CorrelationConflict")
			return
		}
		select {
		case <-prior.done:
			h.writeBytes(w, prior.body, prior.status)
		case <-ctx.Done():
			code := "OutcomeUnknown"
			if readOnly(input.Operation) {
				code = "WaitExpired"
			}
			fail(code)
		}
		return
	}
	if len(h.cache) >= 256 {
		h.mu.Unlock()
		fail("CapacityExceeded")
		return
	}
	entry := &cachedResponse{canonical: canonical, done: make(chan struct{})}
	h.cache[input.RequestID] = entry
	h.mu.Unlock()
	response := h.execute(ctx, input)
	entry.status = http.StatusOK
	if response.Error != nil {
		entry.status = statusFor(response.Error.Code)
	}
	entry.body, err = json.Marshal(response)
	schemaName := operationSchema(input.Operation, "response")
	if response.Error != nil {
		schemaName = "Error"
	}
	if err != nil || len(entry.body) > MaxResponse || validate(schemaName, entry.body) != nil {
		response = h.failure(input, "CapacityExceeded")
		entry.body, err = json.Marshal(response)
		if err != nil {
			w.WriteHeader(503)
			close(entry.done)
			return
		}
		entry.status = statusFor("CapacityExceeded")
	}
	close(entry.done)
	h.writeBytes(w, entry.body, entry.status)
}
func validateEnvelope(body []byte) error {
	if len(body) > MaxRequest {
		return errors.New("body bound")
	}
	if err := validate("clock_read_request", []byte(`{"version":1,"project":"umir-backend-000000000000000000000000","requestID":"11111111-1111-4111-8111-111111111111","operation":"clock.read","data":{}}`)); err != nil {
		return err
	}
	if err := checkDocument(body); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || len(fields) != 5 {
		return errors.New("envelope fields")
	}
	for _, k := range []string{"version", "project", "requestID", "operation", "data"} {
		if _, ok := fields[k]; !ok {
			return errors.New("envelope fields")
		}
	}
	var req Request
	if err := json.Unmarshal(body, &req); err != nil || req.Version != 1 || !projectPattern.MatchString(req.Project) || !uuidPattern.MatchString(req.RequestID) || len(req.Operation) == 0 || len(req.Operation) > 128 || len(req.Data) == 0 || req.Data[0] != '{' {
		return errors.New("envelope identity")
	}
	return nil
}
func (h *Handler) failure(r Request, code string) Response {
	effect := "none"
	if code == "OutcomeUnknown" {
		effect = "unknown"
	}
	return Response{Version: 1, Project: h.opts.Project, RequestID: r.RequestID, Operation: r.Operation, Error: &Fault{Code: code, Effect: effect, Retryable: readOnly(r.Operation) && (code == "NotReady" || code == "WaitExpired")}}
}
func statusFor(code string) int {
	switch code {
	case "InvalidRequest":
		return 400
	case "Unauthenticated":
		return 401
	case "Forbidden", "WrongProject", "OwnershipMismatch":
		return 403
	case "UnsupportedOperation", "NotFound":
		return 404
	case "ClockConflict", "ClockBackward", "BootstrapConflict", "CorrelationConflict", "FaultConflict":
		return 409
	case "CapacityExceeded":
		return 413
	case "WaitExpired":
		return 408
	case "PrivacyViolation":
		return 422
	default:
		return 503
	}
}
func (h *Handler) write(w http.ResponseWriter, response Response, status int) {
	body, err := json.Marshal(response)
	if err != nil {
		w.WriteHeader(503)
		return
	}
	h.writeBytes(w, body, status)
}
func (h *Handler) writeBytes(w http.ResponseWriter, body []byte, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		return
	}
}

func readOnly(op string) bool {
	return op == "fixture.ready" || op == "clock.read" || op == "clock.checkpoint" || op == "feedback.observe" || op == "owner.fault.read"
}
func authOperation(op string) bool {
	return readOnly(op) || op == "clock.set" || op == "owner.fault.arm" || op == "owner.fault.clear"
}
