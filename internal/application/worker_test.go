package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

type fixedJitter struct{}

func (fixedJitter) Delay(d time.Duration) time.Duration { return d }

type outcomeSender struct {
	result domain.DeliveryResult
	sent   int
}

func (s *outcomeSender) Send(context.Context, domain.Delivery) domain.DeliveryResult {
	s.sent++
	return s.result
}
func TestWorkerStoresTransientRetryAndProviderDelay(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.FixedZone("MSK", 10800))
	d := domain.Delivery{Submission: domain.Submission{ID: "123e4567-e89b-42d3-a456-426614174000", ReceivedAt: now}, Job: domain.Job{Channel: domain.Telegram, State: domain.InFlight, Attempts: 1, StartedAt: now}}
	repo := &memoryRepository{claims: []domain.Delivery{d}}
	sender := &outcomeSender{result: domain.DeliveryResult{Code: "TelegramRateLimited", RetryAfter: time.Minute}}
	worker := NewWorker(repo, fixedClock{now}, fixedJitter{}, sender, domain.Telegram, domain.DefaultRetryPolicy())
	worked, err := worker.ProcessOne(context.Background())
	if err != nil || !worked || len(repo.finished) != 1 {
		t.Fatalf("process %v %v", worked, err)
	}
	job := repo.finished[0]
	if job.State != domain.Pending || job.Attempts != 1 || !job.NextAttemptAt.Equal(now.Add(time.Minute)) || job.ErrorCode != "TelegramRateLimited" {
		t.Fatalf("job %#v", job)
	}
}
func TestWorkerFinishesSentAndPropagatesJournalFailure(t *testing.T) {
	now := time.Now()
	repo := &memoryRepository{claims: []domain.Delivery{{Submission: domain.Submission{ID: "123e4567-e89b-42d3-a456-426614174000", ReceivedAt: now}, Job: domain.Job{Channel: domain.SMTP, State: domain.InFlight, Attempts: 1}}}, finishError: ErrStorageUnavailable}
	sender := &outcomeSender{result: domain.DeliveryResult{Success: true, ProviderID: "message-1"}}
	worker := NewWorker(repo, fixedClock{now}, fixedJitter{}, sender, domain.SMTP, domain.DefaultRetryPolicy())
	_, err := worker.ProcessOne(context.Background())
	if !errors.Is(err, ErrStorageUnavailable) {
		t.Fatalf("%v", err)
	}
	if repo.finished[0].State != domain.Sent || repo.finished[0].ProviderID != "message-1" || repo.finished[0].ConfirmedAt.IsZero() {
		t.Fatal("sent state")
	}
}
func TestWorkerExhaustionPreservesAttempts(t *testing.T) {
	now := time.Now()
	repo := &memoryRepository{claims: []domain.Delivery{{Submission: domain.Submission{ReceivedAt: now}, Job: domain.Job{Channel: domain.SMTP, State: domain.InFlight, Attempts: 10}}}}
	sender := &outcomeSender{result: domain.DeliveryResult{Code: "SMTPTransient"}}
	w := NewWorker(repo, fixedClock{now}, fixedJitter{}, sender, domain.SMTP, domain.DefaultRetryPolicy())
	_, err := w.ProcessOne(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(repo.finished) != 1 {
		t.Fatal("missing durable finish")
	}
	if repo.finished[0].State != domain.Failed || repo.finished[0].Attempts != 10 {
		t.Fatal("not exhausted")
	}
}

func TestWorkerCompletesWithoutResurrectingDeletedSubmission(t *testing.T) {
	now := time.Now()
	repo := &memoryRepository{claims: []domain.Delivery{{Submission: domain.Submission{ReceivedAt: now}, Job: domain.Job{Channel: domain.SMTP, State: domain.InFlight, Attempts: 1}}}, finishError: ErrNotFound}
	sender := &outcomeSender{result: domain.DeliveryResult{Success: true}}
	w := NewWorker(repo, fixedClock{now}, fixedJitter{}, sender, domain.SMTP, domain.DefaultRetryPolicy())
	worked, err := w.ProcessOne(context.Background())
	if !worked || err != nil {
		t.Fatalf("deleted in-flight submission stopped worker: %v", err)
	}
}

func TestWorkerKeepsRunningWhenLeaseWasTakenOver(t *testing.T) {
	now := time.Now()
	repo := &memoryRepository{claims: []domain.Delivery{{Submission: domain.Submission{ReceivedAt: now}, Job: domain.Job{Channel: domain.SMTP, State: domain.InFlight, Attempts: 1}}}, finishError: ErrLeaseLost}
	sender := &outcomeSender{result: domain.DeliveryResult{Success: true}}
	w := NewWorker(repo, fixedClock{now}, fixedJitter{}, sender, domain.SMTP, domain.DefaultRetryPolicy())
	worked, err := w.ProcessOne(context.Background())
	if !worked || err != nil {
		t.Fatalf("lost lease stopped worker: %v", err)
	}
}

type recordingObserver struct{ events []DeliveryEvent }

func (o *recordingObserver) ObserveDelivery(e DeliveryEvent) { o.events = append(o.events, e) }

func observedWorker(t *testing.T, repo *memoryRepository, result domain.DeliveryResult, attempts int, now time.Time) []DeliveryEvent {
	t.Helper()
	repo.claims = []domain.Delivery{{Submission: domain.Submission{ID: "123e4567-e89b-42d3-a456-426614174000", ReceivedAt: now}, Job: domain.Job{ID: "job-1", Channel: domain.SMTP, State: domain.InFlight, Attempts: attempts, StartedAt: now}}}
	observer := &recordingObserver{}
	w := NewWorker(repo, fixedClock{now}, fixedJitter{}, &outcomeSender{result: result}, domain.SMTP, domain.DefaultRetryPolicy()).WithObserver(observer)
	if _, err := w.ProcessOne(context.Background()); (err != nil) != errors.Is(repo.finishError, ErrStorageUnavailable) {
		t.Fatalf("process error %v", err)
	}
	return observer.events
}

func TestWorkerObservesEachTransitionOnce(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name     string
		repo     *memoryRepository
		result   domain.DeliveryResult
		attempts int
		want     DeliveryEvent
	}{
		{"sent", &memoryRepository{}, domain.DeliveryResult{Success: true, ProviderID: "provider-secret"}, 1, DeliveryEvent{Outcome: DeliverySent, Attempt: 1}},
		{"retry", &memoryRepository{}, domain.DeliveryResult{Code: "SMTPTransient"}, 2, DeliveryEvent{Outcome: DeliveryRetry, Code: "SMTPTransient", Attempt: 2}},
		{"failed permanent", &memoryRepository{}, domain.DeliveryResult{Code: "SMTPRejected", Permanent: true}, 1, DeliveryEvent{Outcome: DeliveryFailed, Code: "SMTPRejected", Attempt: 1}},
		{"failed exhausted", &memoryRepository{}, domain.DeliveryResult{Code: "SMTPTransient"}, 10, DeliveryEvent{Outcome: DeliveryFailed, Code: "SMTPTransient", Attempt: 10}},
		{"unknown on store error", &memoryRepository{finishError: ErrStorageUnavailable}, domain.DeliveryResult{Success: true}, 3, DeliveryEvent{Outcome: DeliveryUnknown, Attempt: 3}},
		{"unknown on lost lease", &memoryRepository{finishError: ErrLeaseLost}, domain.DeliveryResult{Code: "SMTPTransient"}, 1, DeliveryEvent{Outcome: DeliveryUnknown, Code: "SMTPTransient", Attempt: 1}},
		{"unknown on missing submission", &memoryRepository{finishError: ErrNotFound}, domain.DeliveryResult{Success: true}, 1, DeliveryEvent{Outcome: DeliveryUnknown, Attempt: 1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			events := observedWorker(t, c.repo, c.result, c.attempts, now)
			c.want.Channel = domain.SMTP
			c.want.JobID = "job-1"
			c.want.SubmissionID = "123e4567-e89b-42d3-a456-426614174000"
			if len(events) != 1 || events[0] != c.want {
				t.Fatalf("events %#v want %#v", events, c.want)
			}
		})
	}
}

func TestWorkerObserverIsOptional(t *testing.T) {
	now := time.Now()
	repo := &memoryRepository{claims: []domain.Delivery{{Submission: domain.Submission{ReceivedAt: now}, Job: domain.Job{Channel: domain.SMTP, State: domain.InFlight, Attempts: 1}}}}
	w := NewWorker(repo, fixedClock{now}, fixedJitter{}, &outcomeSender{result: domain.DeliveryResult{Success: true}}, domain.SMTP, domain.DefaultRetryPolicy()).WithObserver(nil)
	if worked, err := w.ProcessOne(context.Background()); !worked || err != nil {
		t.Fatalf("%v %v", worked, err)
	}
}
