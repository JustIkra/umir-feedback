package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/JustIkra/umir-feedback/internal/application"
	"github.com/JustIkra/umir-feedback/internal/domain"
)

type ScopedIntake interface {
	SubmitScoped(context.Context, domain.Namespace, domain.Input) (domain.Receipt, bool, error)
}
type ScopedReader interface {
	ListScoped(context.Context, string, string) ([]domain.Submission, error)
}
type TokenVerifier interface {
	Validate(context.Context, string, string) (domain.Namespace, error)
}
type ScopedOptions struct {
	Logger     *slog.Logger
	Intake     ScopedIntake
	Reader     ScopedReader
	Verifier   TokenVerifier
	ServiceKey string
	Now        func() time.Time
	Ready      func() bool
}
type saved struct {
	Receipt domain.Receipt   `json:"receipt"`
	Scope   domain.Namespace `json:"scope"`
	State   string           `json:"state"`
}
type page struct {
	Namespace domain.Namespace
	Operation string
	Expires   time.Time
	Rows      []saved
	Index     int
}
type scopedHandler struct {
	opts  ScopedOptions
	mu    sync.Mutex
	pages map[string]page
}

func NewScopedHandler(o ScopedOptions) (http.Handler, error) {
	if o.Logger == nil || o.Intake == nil || o.Reader == nil || o.Verifier == nil || o.ServiceKey == "" || o.Now == nil || o.Ready == nil {
		return nil, application.ErrInvalidArgument
	}
	h := &scopedHandler{opts: o, pages: map[string]page{}}
	mux := http.NewServeMux()
	mux.Handle("/", NewHandler(o.Logger, nil, o.Ready))
	mux.Handle("/internal/feedback/v2/", h)
	return mux, nil
}
func (h *scopedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reject := func(status int, code string) {
		writeJSON(w, status, errorBody{Code: code, Message: code, RequestID: "feedback-private"}, h.opts.Logger)
	}
	values := r.Header.Values("X-Umir-Service-Key")
	if len(values) != 1 || subtle.ConstantTimeCompare([]byte(values[0]), []byte(h.opts.ServiceKey)) != 1 {
		reject(401, "Unauthenticated")
		return
	}
	auth := r.Header.Values("Authorization")
	if len(auth) != 1 || !strings.HasPrefix(auth[0], "Bearer ") || strings.Count(auth[0], " ") != 1 {
		reject(401, "InvalidToken")
		return
	}
	for k := range r.Header {
		lower := strings.ToLower(k)
		if strings.HasPrefix(lower, "x-umir-") && lower != "x-umir-service-key" {
			reject(400, "InvalidRequest")
			return
		}
	}
	operation := ""
	read := false
	tenant := ""
	switch r.URL.Path {
	case "/internal/feedback/v2/submissions/app":
		operation = "appFeedback"
	case "/internal/feedback/v2/submissions/anonymous":
		operation = "feedback.anonymous.submit"
	default:
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) == 7 && parts[4] == "r" && canonicalID(parts[5]) && (parts[6] == "submissions" || parts[6] == "export") {
			read = true
			tenant = parts[5]
			operation = "publicFeedbackRead"
			if parts[6] == "export" {
				operation = "publicFeedbackExport"
			}
		}
	}
	if operation == "" {
		reject(404, "NotFound")
		return
	}
	method := "POST"
	if read {
		method = "GET"
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		reject(405, "MethodNotAllowed")
		return
	}
	ns, err := h.opts.Verifier.Validate(r.Context(), strings.TrimPrefix(auth[0], "Bearer "), operation)
	if err != nil {
		switch {
		case errors.Is(err, application.ErrAuthUnavailable):
			reject(503, "AuthUnavailable")
		case errors.Is(err, application.ErrForbidden):
			reject(403, "Forbidden")
		default:
			reject(401, "InvalidToken")
		}
		return
	}
	if read {
		if ns.Kind != "app" || ns.TenantID != tenant {
			reject(403, "Forbidden")
			return
		}
		h.list(w, r, ns, operation)
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		reject(400, "InvalidRequest")
		return
	}
	media, params, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if e != nil || media != "application/json" || (params["charset"] != "" && !strings.EqualFold(params["charset"], "utf-8")) {
		reject(415, "UnsupportedMediaType")
		return
	}
	body, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
	if e != nil {
		var large *http.MaxBytesError
		if errors.As(e, &large) {
			reject(413, "PayloadTooLarge")
		} else {
			reject(400, "InvalidRequest")
		}
		return
	}
	var in wireInput
	if operation == "appFeedback" {
		if !utf8.Valid(body) || !validEscapes(body) {
			reject(400, "InvalidRequest")
			return
		}
		d := json.NewDecoder(strings.NewReader(string(body)))
		if uniqueKeys(d) != nil {
			reject(400, "InvalidRequest")
			return
		}
		if _, e := d.Token(); !errors.Is(e, io.EOF) {
			reject(400, "InvalidRequest")
			return
		}
		var top map[string]json.RawMessage
		if json.Unmarshal(body, &top) != nil || len(top) != 2 || top["context"] == nil || top["submission"] == nil {
			reject(400, "InvalidRequest")
			return
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(top["context"], &fields) != nil || len(fields) != 4 {
			reject(400, "InvalidRequest")
			return
		}
		for _, k := range []string{"kind", "userID", "orgID", "tenantID"} {
			if v, ok := fields[k]; !ok || string(v) == "null" {
				reject(400, "InvalidRequest")
				return
			}
		}
		var contextNS domain.Namespace
		d = json.NewDecoder(strings.NewReader(string(top["context"])))
		d.DisallowUnknownFields()
		if d.Decode(&contextNS) != nil || !contextNS.Valid() {
			reject(400, "InvalidRequest")
			return
		}
		if contextNS != ns {
			reject(403, "Forbidden")
			return
		}
		in, e = decodeInput(top["submission"])
	} else {
		in, e = decodeInput(body)
	}
	if e != nil {
		reject(400, "InvalidRequest")
		return
	}
	receipt, created, e := h.opts.Intake.SubmitScoped(r.Context(), ns, domain.Input{ID: in.SubmissionID, Name: in.Name, Email: in.Email, Phone: in.Phone, Message: in.Message, ConsentVersion: in.Consent.DocumentVersion, Accepted: in.Consent.Accepted, Website: in.Website})
	if e != nil {
		var invalid *application.ValidationError
		switch {
		case errors.As(e, &invalid):
			status := 422
			if invalid.Code == "InvalidRequest" {
				status = 400
			}
			writeJSON(w, status, errorBody{Code: invalid.Code, Message: invalid.Code, FieldErrors: invalid.Fields, RequestID: "feedback-private"}, h.opts.Logger)
		case errors.Is(e, application.ErrIdentityConflict):
			reject(409, "SubmissionIdentityConflict")
		default:
			reject(503, "FeedbackStorageUnavailable")
		}
		return
	}
	state := "duplicate"
	if created {
		state = "accepted"
	}
	writeJSON(w, 201, saved{receipt, ns, state}, h.opts.Logger)
}
func (h *scopedHandler) list(w http.ResponseWriter, r *http.Request, ns domain.Namespace, operation string) {
	fail := func(status int, code string) {
		writeJSON(w, status, errorBody{Code: code, Message: code, RequestID: "feedback-private"}, h.opts.Logger)
	}
	q, e := url.ParseQuery(r.URL.RawQuery)
	if e != nil || r.URL.ForceQuery {
		fail(400, "InvalidRequest")
		return
	}
	for k, v := range q {
		if len(v) != 1 || (k != "limit" && k != "cursor") {
			fail(400, "InvalidRequest")
			return
		}
	}
	limit := 100
	if v, ok := q["limit"]; ok {
		limit, e = strconv.Atoi(v[0])
		if e != nil || limit < 1 || limit > 500 {
			fail(400, "InvalidRequest")
			return
		}
	}
	now := h.opts.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	for key, p := range h.pages {
		if !now.Before(p.Expires) {
			delete(h.pages, key)
		}
	}
	var p page
	if v, ok := q["cursor"]; ok {
		var found bool
		p, found = h.pages[v[0]]
		if !found || p.Namespace != ns || p.Operation != operation {
			fail(403, "Forbidden")
			return
		}
	} else {
		records, e := h.opts.Reader.ListScoped(r.Context(), ns.OrgID, ns.TenantID)
		if e != nil {
			fail(503, "FeedbackStorageUnavailable")
			return
		}
		p = page{Namespace: ns, Operation: operation, Expires: now.Add(5 * time.Minute), Rows: make([]saved, 0, len(records))}
		for _, sub := range records {
			p.Rows = append(p.Rows, saved{domain.Receipt{SubmissionID: sub.ID, ReceivedAt: sub.ReceivedAt}, sub.Namespace, "accepted"})
		}
	}
	end := p.Index + limit
	if end > len(p.Rows) {
		end = len(p.Rows)
	}
	next := ""
	if end < len(p.Rows) {
		if len(h.pages) >= 1024 {
			fail(503, "FeedbackStorageUnavailable")
			return
		}
		var b [32]byte
		if _, e := rand.Read(b[:]); e != nil {
			fail(503, "FeedbackStorageUnavailable")
			return
		}
		next = hex.EncodeToString(b[:])
		cp := p
		cp.Index = end
		h.pages[next] = cp
	}
	writeJSON(w, 200, struct {
		TenantID   string  `json:"tenantID"`
		Items      []saved `json:"items"`
		NextCursor string  `json:"nextCursor,omitempty"`
	}{ns.TenantID, p.Rows[p.Index:end], next}, h.opts.Logger)
}
