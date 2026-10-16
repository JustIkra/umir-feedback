package application

import (
	"context"
	"errors"
	"fmt"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

type Worker struct {
	repository Repository
	clock      Clock
	jitter     Jitter
	sender     Sender
	channel    domain.Channel
	policy     domain.RetryPolicy
	observer   DeliveryObserver
}

func NewWorker(repository Repository, clock Clock, jitter Jitter, sender Sender, channel domain.Channel, policy domain.RetryPolicy) *Worker {
	return &Worker{repository: repository, clock: clock, jitter: jitter, sender: sender, channel: channel, policy: policy}
}

// WithObserver sets the delivery observer; nil keeps the worker silent.
func (w *Worker) WithObserver(observer DeliveryObserver) *Worker {
	w.observer = observer
	return w
}

func (w *Worker) observe(outcome DeliveryOutcome, cause DeliveryCause, delivery domain.Delivery, job domain.Job, code string) {
	if w.observer == nil {
		return
	}
	w.observer.ObserveDelivery(DeliveryEvent{Outcome: outcome, Channel: w.channel, Code: code, Cause: cause, Attempt: job.Attempts, JobID: job.ID, SubmissionID: delivery.Submission.ID})
}

func (w *Worker) ProcessOne(ctx context.Context) (bool, error) {
	delivery, found, err := w.repository.Claim(ctx, w.channel, w.clock.Now(), w.policy)
	if err != nil {
		return false, fmt.Errorf("claim delivery: %w", err)
	}
	if !found {
		return false, nil
	}
	result := w.sender.Send(ctx, delivery)
	now := w.clock.Now()
	job := delivery.Job
	job.ProviderID = ""
	job.ErrorCode = result.Code
	if result.Success {
		job.State = domain.Sent
		job.ConfirmedAt = now
		job.ProviderID = result.ProviderID
		job.ErrorCode = ""
	} else {
		cycle := delivery.Submission.ReceivedAt
		if !job.RetryStartedAt.IsZero() {
			cycle = job.RetryStartedAt
		}
		if result.Permanent || w.policy.Exhausted(job.Attempts, cycle, now) {
			job.State = domain.Failed
			job.ConfirmedAt = now
		} else {
			job.State = domain.Pending
			delay := w.jitter.Delay(w.policy.Delay(job.Attempts))
			if delay < w.policy.MinDelay {
				delay = w.policy.MinDelay
			}
			if delay > w.policy.MaxDelay {
				delay = w.policy.MaxDelay
			}
			if result.RetryAfter > delay {
				delay = result.RetryAfter
			}
			job.NextAttemptAt = now.Add(delay)
		}
	}
	var finishErr error
	if scoped, ok := w.repository.(ScopedRepository); ok {
		finishErr = scoped.FinishScoped(context.WithoutCancel(ctx), delivery.Submission.Namespace, delivery.Submission.ID, w.channel, job)
	} else {
		finishErr = w.repository.Finish(context.WithoutCancel(ctx), delivery.Submission.ID, w.channel, job)
	}
	if err := finishErr; err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			w.observe(DeliveryUnknown, CauseNotFound, delivery, job, result.Code)
			return true, nil
		case errors.Is(err, ErrLeaseLost):
			w.observe(DeliveryUnknown, CauseLeaseLost, delivery, job, result.Code)
			return true, nil
		}
		w.observe(DeliveryUnknown, CauseStoreError, delivery, job, result.Code)
		return true, fmt.Errorf("confirm delivery state: %w", err)
	}
	switch job.State {
	case domain.Sent:
		w.observe(DeliverySent, "", delivery, job, result.Code)
	case domain.Failed:
		w.observe(DeliveryFailed, "", delivery, job, result.Code)
	default:
		w.observe(DeliveryRetry, "", delivery, job, result.Code)
	}
	return true, nil
}
