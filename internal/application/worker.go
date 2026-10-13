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
}

func NewWorker(repository Repository, clock Clock, jitter Jitter, sender Sender, channel domain.Channel, policy domain.RetryPolicy) *Worker {
	return &Worker{repository: repository, clock: clock, jitter: jitter, sender: sender, channel: channel, policy: policy}
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
		if errors.Is(err, ErrNotFound) {
			return true, nil
		}
		return true, fmt.Errorf("confirm delivery state: %w", err)
	}
	return true, nil
}
