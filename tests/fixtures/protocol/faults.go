package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/JustIkra/umir-feedback/internal/application"
	"github.com/JustIkra/umir-feedback/internal/domain"
)

type Faults struct {
	mu    sync.Mutex
	armed *armedFault
}
type armedFault struct {
	ID, Point, Operation, Command, State, Outcome string
	Until                                         time.Time
	Hits                                          int
}

func (f *Faults) Control(operation string, data []byte) (any, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var in struct {
		FaultID       string `json:"faultID"`
		Point         string `json:"point"`
		OperationID   string `json:"operationID"`
		CommandID     string `json:"commandID"`
		ExpireAfterMs int    `json:"expireAfterMs"`
	}
	if json.Unmarshal(data, &in) != nil {
		return nil, "InvalidRequest"
	}
	if f.armed != nil && f.armed.State == "armed" && !time.Now().Before(f.armed.Until) {
		f.armed.State = "expired"
	}
	if operation == "owner.fault.arm" {
		if !supportedFault(in.Point, in.OperationID) {
			return nil, "UnsupportedOperation"
		}
		if f.armed != nil && f.armed.State == "armed" {
			return nil, "FaultConflict"
		}
		f.armed = &armedFault{ID: in.FaultID, Point: in.Point, Operation: in.OperationID, Command: in.CommandID, State: "armed", Outcome: "not-observed", Until: time.Now().Add(time.Duration(in.ExpireAfterMs) * time.Millisecond)}
		return struct {
			FaultID string `json:"faultID"`
			State   string `json:"state"`
		}{in.FaultID, "armed"}, ""
	}
	if f.armed == nil || f.armed.ID != in.FaultID {
		return nil, "NotFound"
	}
	a := f.armed
	if operation == "owner.fault.clear" {
		a.State = "cleared"
		return struct {
			FaultID string `json:"faultID"`
			Cleared bool   `json:"cleared"`
		}{a.ID, true}, ""
	}
	return struct {
		FaultID string `json:"faultID"`
		State   string `json:"state"`
		Hits    int    `json:"hits"`
		Outcome string `json:"ownerOutcome"`
	}{a.ID, a.State, a.Hits, a.Outcome}, ""
}

type FaultRepository struct {
	Store  application.Repository
	Faults *Faults
}

func (s *FaultRepository) Lookup(ctx context.Context, id domain.SubmissionID) (domain.Submission, bool, error) {
	return s.Store.Lookup(ctx, id)
}
func (s *FaultRepository) LookupScoped(ctx context.Context, ns domain.Namespace, id domain.SubmissionID) (domain.Submission, bool, error) {
	scoped, ok := s.Store.(application.ScopedRepository)
	if !ok {
		return domain.Submission{}, false, application.ErrStorageUnavailable
	}
	return scoped.LookupScoped(ctx, ns, id)
}
func (s *FaultRepository) Claim(ctx context.Context, ch domain.Channel, now time.Time, p domain.RetryPolicy) (domain.Delivery, bool, error) {
	return s.Store.Claim(ctx, ch, now, p)
}
func (s *FaultRepository) Finish(ctx context.Context, id domain.SubmissionID, ch domain.Channel, j domain.Job) error {
	return s.Store.Finish(ctx, id, ch, j)
}
func (s *FaultRepository) FinishScoped(ctx context.Context, ns domain.Namespace, id domain.SubmissionID, ch domain.Channel, j domain.Job) error {
	scoped, ok := s.Store.(application.ScopedRepository)
	if !ok {
		return application.ErrStorageUnavailable
	}
	return scoped.FinishScoped(ctx, ns, id, ch, j)
}
func (s *FaultRepository) Accept(ctx context.Context, sub domain.Submission) (domain.Receipt, bool, error) {
	fault := s.Faults.take(ctx)
	if fault == nil {
		return s.Store.Accept(ctx, sub)
	}
	if fault.Point == "before-commit" {
		s.Faults.outcome(fault, "not_committed")
		return domain.Receipt{}, false, application.ErrStorageUnavailable
	}
	receipt, created, err := s.Store.Accept(ctx, sub)
	if err != nil {
		s.Faults.outcome(fault, "unknown")
		return receipt, created, err
	}
	if !created {
		s.Faults.outcome(fault, "not_committed")
		return receipt, created, nil
	}
	s.Faults.outcome(fault, "committed")
	if fault.Point == "after-real-commit-return-unknown" {
		return domain.Receipt{}, false, application.ErrStorageUnavailable
	}
	if selected, ok := ctx.Value(selectionKey{}).(*selectedCommand); ok {
		selected.Drop.Store(true)
	}
	return receipt, created, nil
}

type selectedCommand struct {
	Operation, Command string
	Drop               atomic.Bool
}
type selectionKey struct{}

func WithCommand(ctx context.Context, operation, command string) context.Context {
	return context.WithValue(ctx, selectionKey{}, &selectedCommand{Operation: operation, Command: command})
}
func (f *Faults) take(ctx context.Context) *armedFault {
	selected, ok := ctx.Value(selectionKey{}).(*selectedCommand)
	if !ok {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.armed
	if a == nil || a.State != "armed" || a.Operation != selected.Operation || a.Command != selected.Command {
		return nil
	}
	if !time.Now().Before(a.Until) {
		a.State = "expired"
		return nil
	}
	a.State = "hit"
	a.Hits = 1
	return a
}
func (f *Faults) outcome(a *armedFault, outcome string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a.Outcome = outcome
}
func (f *Faults) WrapHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, e := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
		if e != nil || len(body) > 1<<20 {
			w.WriteHeader(400)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var in struct {
			SubmissionID string `json:"submissionID"`
			Submission   struct {
				SubmissionID string `json:"submissionID"`
			} `json:"submission"`
		}
		if json.Unmarshal(body, &in) != nil {
			next.ServeHTTP(w, r)
			return
		}
		command := in.SubmissionID
		if in.Submission.SubmissionID != "" {
			command = in.Submission.SubmissionID
		}
		if command == "" {
			next.ServeHTTP(w, r)
			return
		}
		op := facadeOperation(r.Method, r.URL.Path)
		if op == "" {
			next.ServeHTTP(w, r)
			return
		}
		ctx := WithCommand(r.Context(), op, command)
		selected, ok := ctx.Value(selectionKey{}).(*selectedCommand)
		if !ok {
			w.WriteHeader(503)
			return
		}
		capture := httptest.NewRecorder()
		next.ServeHTTP(capture, r.WithContext(ctx))
		if selected.Drop.Load() {
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, e := hj.Hijack()
				if e == nil {
					if e = conn.Close(); e != nil {
						return
					}
					return
				}
			}
			w.WriteHeader(503)
			return
		}
		for k, v := range capture.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(capture.Code)
		if _, e = w.Write(capture.Body.Bytes()); e != nil {
			return
		}
	})
}

type faultRoute struct{ Method, Path, Operation string }

var faultRoutes = []faultRoute{
	{"POST", "/internal/feedback/v2/submissions/app", "appFeedback"},
	{"POST", "/internal/feedback/v2/submissions/anonymous", "anonymousFeedback"},
}

func supportedFault(point, operation string) bool {
	switch point {
	case "before-commit", "after-real-commit-return-unknown", "after-durable-before-reply":
	default:
		return false
	}
	for _, route := range faultRoutes {
		if route.Operation == operation {
			return true
		}
	}
	return false
}

func facadeOperation(method, path string) string {
	segments := strings.Split(path, "/")
	for _, route := range faultRoutes {
		if method != route.Method {
			continue
		}
		expected := strings.Split(route.Path, "/")
		if len(segments) != len(expected) {
			continue
		}
		matches := true
		for i, part := range expected {
			if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
				if segments[i] == "" {
					matches = false
					break
				}
			} else if part != segments[i] {
				matches = false
				break
			}
		}
		if matches {
			return route.Operation
		}
	}
	return ""
}
