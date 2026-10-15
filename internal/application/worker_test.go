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
