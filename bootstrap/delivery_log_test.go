package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/JustIkra/umir-feedback/internal/application"
	"github.com/JustIkra/umir-feedback/internal/domain"
)

type logRepository struct {
	claims []domain.Delivery
	err    error
}

func (r *logRepository) Accept(context.Context, domain.Submission) (domain.Receipt, bool, error) {
	return domain.Receipt{}, false, nil
}
func (r *logRepository) Lookup(context.Context, domain.SubmissionID) (domain.Submission, bool, error) {
	return domain.Submission{}, false, nil
}
func (r *logRepository) Claim(context.Context, domain.Channel, time.Time, domain.RetryPolicy) (domain.Delivery, bool, error) {
	if len(r.claims) == 0 {
		return domain.Delivery{}, false, nil
	}
	d := r.claims[0]
	r.claims = r.claims[1:]
	return d, true, nil
}
func (r *logRepository) Finish(context.Context, domain.SubmissionID, domain.Channel, domain.Job) error {
	return r.err
}

type logSender struct{ result domain.DeliveryResult }

func (s logSender) Send(context.Context, domain.Delivery) domain.DeliveryResult { return s.result }

type logClock struct{ at time.Time }

func (c logClock) Now() time.Time { return c.at }

type logJitter struct{}

func (logJitter) Delay(d time.Duration) time.Duration { return d }

func TestDeliveryLogWritesOneEventPerTransitionWithoutPersonalData(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.FixedZone("MSK", 10800))
	secrets := []string{"Secret Person Name", "secret.person@example.org", "+7 900 000-11-22", "secret message body", "198.51.100.77", "Bearer secret-token-value", "provider-secret-id", "secret-provider-error-text"}
	submission := domain.Submission{
		Namespace: domain.Namespace{Kind: "app", UserID: "11111111-1111-4111-8111-111111111111", OrgID: "22222222-2222-4222-8222-222222222222", TenantID: "33333333-3333-4333-8333-333333333333"},
		CommandID: "123e4567-e89b-42d3-a456-426614174000", ID: "123e4567-e89b-42d3-a456-426614174000",
		Name: secrets[0], Email: secrets[1], Phone: secrets[2], Message: secrets[3] + " " + secrets[4] + " " + secrets[5],
		ConsentVersion: "v1", ConsentPurpose: "feedback-reply", AcceptedAt: now, ReceivedAt: now, Fingerprint: strings.Repeat("ab", 32),
	}
	cases := []struct {
		name    string
		result  domain.DeliveryResult
		err     error
		attempt int
		outcome string
		level   string
		code    string
	}{
		{"sent", domain.DeliveryResult{Success: true, ProviderID: secrets[6]}, nil, 1, "sent", "INFO", ""},
		{"retry", domain.DeliveryResult{Code: "SMTPTransient"}, nil, 2, "retry", "WARN", "SMTPTransient"},
		{"failed", domain.DeliveryResult{Code: "SMTPRejected", Permanent: true}, nil, 3, "failed", "ERROR", "SMTPRejected"},
		{"unknown", domain.DeliveryResult{Success: true, ProviderID: secrets[6]}, application.ErrStorageUnavailable, 4, "unknown", "ERROR", ""},
	}
	var out bytes.Buffer
	observer := NewDeliveryLog(slog.New(slog.NewJSONHandler(&out, nil)))
	for _, c := range cases {
		repo := &logRepository{claims: []domain.Delivery{{Submission: submission, Job: domain.Job{ID: "job-" + c.name, Channel: domain.SMTP, State: domain.InFlight, Attempts: c.attempt, StartedAt: now}}}, err: c.err}
		w := application.NewWorker(repo, logClock{now}, logJitter{}, logSender{c.result}, domain.SMTP, domain.DefaultRetryPolicy()).WithObserver(observer)
		if _, err := w.ProcessOne(context.Background()); (err != nil) != (c.err != nil) {
			t.Fatalf("%s: process error %v", c.name, err)
		}
	}
	raw := out.String()
	for _, secret := range secrets {
		if strings.Contains(raw, secret) {
			t.Fatalf("log contains %q: %s", secret, raw)
		}
	}
	for _, fragment := range []string{"secret", "example.org", "198.51", "+7 900", "Bearer", submission.Fingerprint, submission.Namespace.UserID, submission.Namespace.OrgID, submission.Namespace.TenantID} {
		if strings.Contains(raw, fragment) {
			t.Fatalf("log contains fragment %q: %s", fragment, raw)
		}
	}
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	if len(lines) != len(cases) {
		t.Fatalf("lines %d: %s", len(lines), raw)
	}
	for i, c := range cases {
		var entry map[string]any
		if err := json.Unmarshal([]byte(lines[i]), &entry); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{"msg": "feedback.delivery", "level": c.level, "outcome": c.outcome, "channel": "smtp", "code": c.code, "attempt": float64(c.attempt), "job_id": "job-" + c.name, "submission_id": "123e4567-e89b-42d3-a456-426614174000"}
		for k, v := range want {
			if entry[k] != v {
				t.Fatalf("%s: field %s = %#v want %#v in %s", c.name, k, entry[k], v, lines[i])
			}
		}
		if len(entry) != len(want)+1 {
			t.Fatalf("%s: unexpected fields in %s", c.name, lines[i])
		}
	}
}
