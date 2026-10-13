package protocol

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/JustIkra/umir-feedback/internal/adapters/httpapi"
	"github.com/JustIkra/umir-feedback/internal/adapters/journal"
	mechanisms "github.com/JustIkra/umir-feedback/internal/adapters/runtime"
	"github.com/JustIkra/umir-feedback/internal/application"
	"github.com/JustIkra/umir-feedback/internal/domain"
)

const project = "umir-backend-111111111111111111111111"
const fixtureID = "11111111-1111-4111-8111-111111111111"
const userID = "22222222-2222-4222-8222-222222222222"
const orgID = "33333333-3333-4333-8333-333333333333"
const tenant1 = "44444444-4444-4444-8444-444444444444"
const tenant2 = "55555555-5555-4555-8555-555555555555"
const submissionID = "66666666-6666-4666-8666-666666666666"

var serviceKey = strings.Repeat("a", 64)
var controlKey = strings.Repeat("b", 64)

type localKeys struct{ public ed25519.PublicKey }

func (k localKeys) Key(context.Context, string) (ed25519.PublicKey, error) { return k.public, nil }

type live struct {
	store   *journal.Store
	cfg     journal.Config
	clock   *Clock
	faults  *Faults
	control *Handler
	http    http.Handler
	private ed25519.PrivateKey
}

func newLive(t *testing.T) *live {
	t.Helper()
	id, e := UUID()
	if e != nil {
		t.Fatal(e)
	}
	clock, e := NewClock(fixtureID, id, "2026-10-13T09:00:00+03:00", "1")
	if e != nil {
		t.Fatal(e)
	}
	dir, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	cfg := journal.Config{Path: filepath.Join(dir, "journal.jsonl"), MaxBytes: 16 << 20, UID: os.Getuid(), GID: os.Getgid(), Policy: domain.RetryPolicy{MaxAttempts: 3, MaxAge: 24 * time.Hour, MinDelay: time.Second, MaxDelay: time.Minute}, Now: clock.Now}
	s, e := journal.Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	l := &live{store: s, cfg: cfg, clock: clock, faults: &Faults{}}
	l.bind(t)
	t.Cleanup(func() {
		if l.store != nil {
			if e := l.store.Close(); e != nil {
				t.Error(e)
			}
		}
	})
	return l
}
func (l *live) bind(t *testing.T) {
	t.Helper()
	public, private, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	l.private = private
	verifier, e := httpapi.NewJWTVerifier(httpapi.JWTOptions{Now: l.clock.Now, Keys: localKeys{public}})
	if e != nil {
		t.Fatal(e)
	}
	repo := &FaultRepository{Store: l.store, Faults: l.faults}
	intake := application.NewIntake(repo, l.clock, mechanisms.Normalizer{}, "e2e-fixture-v1")
	h, e := httpapi.NewScopedHandler(httpapi.ScopedOptions{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Intake: intake, Reader: l.store, Verifier: verifier, ServiceKey: serviceKey, Now: l.clock.Now, Ready: l.store.Ready})
	if e != nil {
		t.Fatal(e)
	}
	l.http = l.faults.WrapHTTP(h)
	l.control, e = NewHandler(HandlerOptions{Project: project, Credential: controlKey, BinarySHA256: strings.Repeat("c", 64), ConfigSHA256: strings.Repeat("d", 64), Clock: l.clock, Store: l.store, Faults: l.faults, Ready: func(context.Context) bool { return l.store.Ready() }})
	if e != nil {
		t.Fatal(e)
	}
}
func (l *live) token(t *testing.T, ns domain.Namespace, op string) string {
	t.Helper()
	capability := map[string]string{"appFeedback": "feedback.submit", "publicFeedbackRead": "feedback.read", "publicFeedbackExport": "feedback.export", "feedback.anonymous.submit": "feedback.anonymous.submit"}[op]
	claims := map[string]any{"iss": "urn:umir:auth", "aud": []string{"umir-feedback"}, "iat": l.clock.Now().Unix(), "nbf": l.clock.Now().Unix(), "exp": l.clock.Now().Unix() + 60, "jti": fixtureID, "azp": "umir-gateway", "operation": op, "permissions": []string{capability}}
	if ns.Kind == "app" {
		claims["kind"] = "user"
		claims["sub"] = ns.UserID
		claims["sid"] = fixtureID
		for _, k := range []string{"session_version", "credential_version", "access_version", "settings_version", "lifecycle_version"} {
			claims[k] = "1"
		}
		claims["org_id"] = ns.OrgID
		claims["tenant_id"] = ns.TenantID
		claims["time_zone"] = "Europe/Moscow"
	} else {
		claims["kind"] = "anonymous"
		claims["sub"] = "service:gateway"
	}
	return l.sign(t, claims)
}
func (l *live) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	body, e := json.Marshal(claims)
	if e != nil {
		t.Fatal(e)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"EdDSA","typ":"JWT","kid":"local-unit-key"}`))
	input := header + "." + base64.RawURLEncoding.EncodeToString(body)
	return input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(l.private, []byte(input)))
}
func app(tenant string) domain.Namespace {
	return domain.Namespace{Kind: "app", UserID: userID, OrgID: orgID, TenantID: tenant}
}
func payload(ns domain.Namespace, message string) []byte {
	in := map[string]any{"submissionID": submissionID, "name": "Synthetic", "email": "synthetic@umir.invalid", "message": message, "consent": map[string]any{"accepted": true, "documentVersion": "e2e-fixture-v1"}, "website": ""}
	var data any = in
	if ns.Kind == "app" {
		data = map[string]any{"submission": in, "context": ns}
	}
	b, e := json.Marshal(data)
	if e != nil {
		panic(e)
	}
	return b
}
func (l *live) submit(t *testing.T, ns domain.Namespace, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	path := "/internal/feedback/v2/submissions/app"
	op := "appFeedback"
	if ns.Kind == "anonymous" {
		path = "/internal/feedback/v2/submissions/anonymous"
		op = "feedback.anonymous.submit"
	}
	r := httptest.NewRequest("POST", path, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Umir-Service-Key", serviceKey)
	r.Header.Set("Authorization", "Bearer "+l.token(t, ns, op))
	w := httptest.NewRecorder()
	l.http.ServeHTTP(w, r)
	return w
}
func (l *live) observe(t *testing.T, id string, pairs []domain.Namespace) []byte {
	t.Helper()
	selectors := []any{}
	for _, ns := range pairs {
		selectors = append(selectors, map[string]any{"namespace": ns, "submissionID": submissionID})
	}
	body, e := json.Marshal(map[string]any{"version": 1, "project": project, "requestID": id, "operation": "feedback.observe", "data": map[string]any{"submissions": selectors, "waitMs": 0}})
	if e != nil {
		t.Fatal(e)
	}
	r := httptest.NewRequest("POST", ControlPath, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+controlKey)
	w := httptest.NewRecorder()
	l.control.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("projection status %d %s", w.Code, w.Body.String())
	}
	return w.Body.Bytes()
}
func decodeObservation(t *testing.T, b []byte) Observation {
	t.Helper()
	var res struct {
		Data Observation `json:"data"`
	}
	if e := json.Unmarshal(b, &res); e != nil {
		t.Fatal(e)
	}
	return res.Data
}
func TestA8ThreeNamespacesRetryAndReopen(t *testing.T) {
	l := newLive(t)
	pairs := []domain.Namespace{app(tenant1), app(tenant2), domain.Anonymous()}
	id, e := UUID()
	if e != nil {
		t.Fatal(e)
	}
	emptyBytes := l.observe(t, id, pairs)
	if decodeObservation(t, emptyBytes).Complete {
		t.Fatal("invented records")
	}
	for _, ns := range pairs {
		w := l.submit(t, ns, payload(ns, "Synthetic"))
		if w.Code != 201 {
			t.Fatalf("intake %d %s", w.Code, w.Body.String())
		}
		retry := l.submit(t, ns, payload(ns, "Synthetic"))
		if retry.Code != 201 || !strings.Contains(retry.Body.String(), `"state":"duplicate"`) {
			t.Fatal("exact replay failed")
		}
		conflict := l.submit(t, ns, payload(ns, "Changed"))
		if conflict.Code != 409 {
			t.Fatalf("changed original namespace payload accepted: %d", conflict.Code)
		}
	}
	if !bytes.Equal(emptyBytes, l.observe(t, id, pairs)) {
		t.Fatal("cached incomplete control replay changed")
	}
	fresh, e := UUID()
	if e != nil {
		t.Fatal(e)
	}
	beforeBytes := l.observe(t, fresh, pairs)
	before := decodeObservation(t, beforeBytes)
	if !before.Complete || len(before.Records) != 3 {
		t.Fatal("three durable pairs missing")
	}
	seen := map[string]bool{}
	for _, r := range before.Records {
		if r.CommandID != submissionID || len(r.Jobs) != 2 {
			t.Fatal("missing original command/job edges")
		}
		for _, j := range r.Jobs {
			if seen[j.JobID] || !j.AcceptedAt.Equal(r.ReceivedAt) {
				t.Fatal("aliased job or fabricated acceptance time")
			}
			seen[j.JobID] = true
		}
	}
	if e = l.store.Close(); e != nil {
		t.Fatal(e)
	}
	old := l.clock.Read()
	instance, e := UUID()
	if e != nil {
		t.Fatal(e)
	}
	l.clock, e = NewClock(fixtureID, instance, old.Instant, old.Generation)
	if e != nil {
		t.Fatal(e)
	}
	l.cfg.Now = l.clock.Now
	l.store, e = journal.Open(l.cfg)
	if e != nil {
		t.Fatal(e)
	}
	l.bind(t)
	fresh, e = UUID()
	if e != nil {
		t.Fatal(e)
	}
	afterBytes := l.observe(t, fresh, pairs)
	after := decodeObservation(t, afterBytes)
	if before.InstanceID == after.InstanceID || !reflect.DeepEqual(before.Records, after.Records) || before.SnapshotSHA256 != after.SnapshotSHA256 {
		t.Fatal("restart replaced original namespace/receipt/job facts")
	}
	for _, ns := range pairs {
		w := l.submit(t, ns, payload(ns, "Synthetic"))
		if w.Code != 201 || !strings.Contains(w.Body.String(), `"state":"duplicate"`) {
			t.Fatal("reopen replay changed identity")
		}
	}
	if out := os.Getenv("A8_EVIDENCE_DIR"); out != "" {
		for name, data := range map[string][]byte{"observation-before.json": beforeBytes, "observation-reopen.json": afterBytes} {
			if e := os.WriteFile(filepath.Join(out, name), data, 0600); e != nil {
				t.Fatal(e)
			}
		}
	}
}
func TestA8ScopeSpoofAndReadIsolation(t *testing.T) {
	l := newLive(t)
	for _, ns := range []domain.Namespace{app(tenant1), app(tenant2), domain.Anonymous()} {
		if w := l.submit(t, ns, payload(ns, "Synthetic")); w.Code != 201 {
			t.Fatal("intake failed")
		}
	}
	if w := l.submit(t, app(tenant1), payload(app(tenant2), "Synthetic")); w.Code != 403 {
		t.Fatalf("trusted/body scope mismatch accepted: %d", w.Code)
	}
	for _, op := range []string{"publicFeedbackRead", "publicFeedbackExport"} {
		r := httptest.NewRequest("GET", "/internal/feedback/v2/r/"+tenant1+"/submissions?limit=1", nil)
		if op == "publicFeedbackExport" {
			r.URL.Path = "/internal/feedback/v2/r/" + tenant1 + "/export"
		}
		r.Header.Set("X-Umir-Service-Key", serviceKey)
		r.Header.Set("Authorization", "Bearer "+l.token(t, app(tenant1), op))
		w := httptest.NewRecorder()
		l.http.ServeHTTP(w, r)
		if w.Code != 200 || strings.Contains(w.Body.String(), tenant2) || strings.Contains(w.Body.String(), "synthetic@") {
			t.Fatalf("scope/privacy failure: %d", w.Code)
		}
		r.Header.Set("Authorization", "Bearer "+l.token(t, app(tenant2), op))
		w = httptest.NewRecorder()
		l.http.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal("cross-tenant read permitted")
		}
	}
	r := httptest.NewRequest("POST", "/internal/feedback/v2/submissions/app", bytes.NewReader(payload(app(tenant1), "Synthetic")))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Umir-Service-Key", serviceKey)
	r.Header.Set("X-Umir-Tenant-ID", tenant2)
	r.Header.Set("Authorization", "Bearer "+l.token(t, app(tenant1), "appFeedback"))
	w := httptest.NewRecorder()
	l.http.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatal("spoofed internal scope header permitted")
	}
}
func TestA8RealCommitFaultsAndDroppedReply(t *testing.T) {
	for _, point := range []string{"before-commit", "after-real-commit-return-unknown", "after-durable-before-reply"} {
		t.Run(point, func(t *testing.T) {
			l := newLive(t)
			faultID, e := UUID()
			if e != nil {
				t.Fatal(e)
			}
			data, e := json.Marshal(map[string]any{"faultID": faultID, "point": point, "operationID": "appFeedback", "commandID": submissionID, "expireAfterMs": 30000})
			if e != nil {
				t.Fatal(e)
			}
			if _, code := l.faults.Control("owner.fault.arm", data); code != "" {
				t.Fatal(code)
			}
			server := httptest.NewServer(l.http)
			defer server.Close()
			r, e := http.NewRequest("POST", server.URL+"/internal/feedback/v2/submissions/app", bytes.NewReader(payload(app(tenant1), "Synthetic")))
			if e != nil {
				t.Fatal(e)
			}
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-Umir-Service-Key", serviceKey)
			r.Header.Set("Authorization", "Bearer "+l.token(t, app(tenant1), "appFeedback"))
			response, e := (&http.Client{Timeout: time.Second}).Do(r)
			if point == "after-durable-before-reply" {
				if e == nil {
					if ce := response.Body.Close(); ce != nil {
						t.Fatal(ce)
					}
					t.Fatal("durable reply not dropped")
				}
			} else {
				if e != nil {
					t.Fatal(e)
				}
				if ce := response.Body.Close(); ce != nil {
					t.Fatal(ce)
				}
				if response.StatusCode != 503 {
					t.Fatal("fault did not return unavailable")
				}
			}
			rows, complete, e := l.store.Observe(context.Background(), []journal.Selector{{Namespace: app(tenant1), ID: submissionID}})
			if e != nil {
				t.Fatal(e)
			}
			if (point == "before-commit") == complete {
				t.Fatal("fault scripted outcome disagrees with live store")
			}
			w := l.submit(t, app(tenant1), payload(app(tenant1), "Synthetic"))
			if w.Code != 201 {
				t.Fatal("exact retry failed")
			}
			after, ok, e := l.store.Observe(context.Background(), []journal.Selector{{Namespace: app(tenant1), ID: submissionID}})
			if e != nil || !ok || len(after) != 1 {
				t.Fatal("retry incomplete")
			}
			if complete && !reflect.DeepEqual(rows, after) {
				t.Fatal("retry duplicated jobs")
			}
			l.faults.mu.Lock()
			a := *l.faults.armed
			l.faults.mu.Unlock()
			if a.Hits != 1 || a.State != "hit" {
				t.Fatal("not a one-shot fault")
			}
		})
	}
}

func TestA8WorkersUseExactNamespaceAndRecoveryRetry(t *testing.T) {
	l := newLive(t)
	for _, ns := range []domain.Namespace{app(tenant1), app(tenant2), domain.Anonymous()} {
		if w := l.submit(t, ns, payload(ns, "Synthetic")); w.Code != 201 {
			t.Fatal("intake failed")
		}
	}
	accepted := l.clock.Now()
	d, found, e := l.store.Claim(context.Background(), domain.SMTP, accepted, l.cfg.Policy)
	if e != nil || !found {
		t.Fatal("claim failed")
	}
	originalID := d.Job.ID
	if e = l.store.Close(); e != nil {
		t.Fatal(e)
	}
	l.store, e = journal.Open(l.cfg)
	if e != nil {
		t.Fatal(e)
	}
	deliveries, e := l.store.Status(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	var retry domain.Delivery
	for _, item := range deliveries {
		if item.Job.ID == originalID {
			retry = item
		}
	}
	if retry.Job.State != domain.Pending || retry.Job.Attempts != 1 || !retry.Job.NextAttemptAt.Equal(accepted.Add(time.Second)) || retry.Submission.Namespace != d.Submission.Namespace {
		t.Fatal("recovery lost owner-clock retry or namespace")
	}
	later := accepted.Add(time.Second)
	for i := 0; i < 3; i++ {
		delivery, found, e := l.store.Claim(context.Background(), domain.SMTP, later, l.cfg.Policy)
		if e != nil || !found {
			t.Fatal("claim retry failed")
		}
		delivery.Job.State = domain.Sent
		delivery.Job.ConfirmedAt = later
		delivery.Job.ProviderID = "synthetic"
		if e = l.store.FinishScoped(context.Background(), delivery.Submission.Namespace, delivery.Submission.ID, domain.SMTP, delivery.Job); e != nil {
			t.Fatal(e)
		}
	}
	rows, e := l.store.Status(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	for _, row := range rows {
		if row.Job.Channel == domain.SMTP && row.Job.State != domain.Sent {
			t.Fatal("worker confirmation retargeted namespace")
		}
		if row.Job.Channel == domain.Telegram && (row.Job.State != domain.Pending || row.Job.Attempts != 0) {
			t.Fatal("independent Telegram job changed")
		}
	}
}
func TestA8JWTRejectsForgeryExpiryAndExtraAnonymousScope(t *testing.T) {
	l := newLive(t)
	token := l.token(t, app(tenant1), "appFeedback")
	verifier, e := httpapi.NewJWTVerifier(httpapi.JWTOptions{Now: l.clock.Now, Keys: localKeys{ed25519.PublicKey(l.private[32:])}})
	if e != nil {
		t.Fatal(e)
	}
	parts := strings.Split(token, ".")
	data, e := base64.RawURLEncoding.DecodeString(parts[1])
	if e != nil {
		t.Fatal(e)
	}
	var claims map[string]any
	if json.Unmarshal(data, &claims) != nil {
		t.Fatal("claims")
	}
	for name, edit := range map[string]func(){"wrong audience": func() { claims["aud"] = []string{"umir-warehouse"} }, "wrong operation": func() { claims["operation"] = "publicFeedbackRead" }, "null version": func() { claims["session_version"] = nil }, "unknown field": func() { claims["role"] = "Admin" }} {
		t.Run(name, func(t *testing.T) {
			var c map[string]any
			if e := json.Unmarshal(data, &c); e != nil {
				t.Fatal(e)
			}
			claims = c
			edit()
			if _, e := verifier.Validate(context.Background(), l.sign(t, claims), "appFeedback"); e == nil {
				t.Fatal("invalid token accepted")
			}
		})
	}
	signature, e := base64.RawURLEncoding.DecodeString(parts[2])
	if e != nil {
		t.Fatal(e)
	}
	signature[0] ^= 1
	if _, e := verifier.Validate(context.Background(), parts[0]+"."+parts[1]+"."+base64.RawURLEncoding.EncodeToString(signature), "appFeedback"); e == nil {
		t.Fatal("forged signature accepted")
	}
	state := l.clock.Read()
	if code := l.clock.Set(state.InstanceID, state.Generation, l.clock.Now().Add(60*time.Second).Format(time.RFC3339)); code != "" {
		t.Fatal(code)
	}
	if _, e := verifier.Validate(context.Background(), token, "appFeedback"); e == nil {
		t.Fatal("exact expiry boundary accepted")
	}
	anonymous := l.token(t, domain.Anonymous(), "feedback.anonymous.submit")
	parts = strings.Split(anonymous, ".")
	data, e = base64.RawURLEncoding.DecodeString(parts[1])
	if e != nil {
		t.Fatal(e)
	}
	if e := json.Unmarshal(data, &claims); e != nil {
		t.Fatal(e)
	}
	claims["tenant_id"] = tenant1
	if _, e := verifier.Validate(context.Background(), l.sign(t, claims), "feedback.anonymous.submit"); e == nil {
		t.Fatal("anonymous token acquired scope")
	}
}
