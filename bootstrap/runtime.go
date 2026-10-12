package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/JustIkra/umir-feedback/internal/adapters/delivery"
	"github.com/JustIkra/umir-feedback/internal/adapters/httpapi"
	"github.com/JustIkra/umir-feedback/internal/adapters/journal"
	mechanisms "github.com/JustIkra/umir-feedback/internal/adapters/runtime"
	"github.com/JustIkra/umir-feedback/internal/application"
	"github.com/JustIkra/umir-feedback/internal/domain"
)

func LoadConfig() (mechanisms.Config, error) { return mechanisms.LoadConfig(os.Getenv) }

func Run(ctx context.Context, listener net.Listener, logger *slog.Logger, config mechanisms.Config) (returnErr error) {
	defer func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			returnErr = errors.Join(returnErr, err)
		}
	}()
	clock, err := mechanisms.NewClock()
	if err != nil {
		return errors.New("clock initialization failed")
	}
	store, err := journal.Open(journal.Config{Path: config.StorePath, MaxBytes: config.MaxStoreBytes, UID: os.Getuid(), GID: os.Getgid(), Policy: config.Policy, Now: clock.Now})
	if err != nil {
		return fmt.Errorf("open feedback journal: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, store.Close()) }()
	if err := store.Purge(ctx, clock.Now().AddDate(0, 0, -config.RetentionDays)); err != nil {
		return fmt.Errorf("startup retention: %w", err)
	}
	smtp, err := delivery.NewSMTP(config.SMTP)
	if err != nil {
		return err
	}
	telegram, err := delivery.NewTelegram(config.Telegram)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, telegram.Close()) }()
	intake := application.NewIntake(store, clock, mechanisms.Normalizer{}, config.ConsentVersion)
	var challenges *application.Challenges
	var codeSender *delivery.RegistrationSMTP
	ready := store.Ready
	if config.OTP.Enabled {
		if config.OTP.Secrets == nil {
			return errors.New("OTP secrets are required")
		}
		challenges, err = application.NewChallenges(ctx, store, config.OTP.Secrets, clock, config.OTP.MaxChallenges)
		if err != nil {
			return fmt.Errorf("initialize email challenges: %w", err)
		}
		codeSender, err = delivery.NewRegistrationSMTP(smtp, config.OTP.Mail)
		if err != nil {
			return err
		}
		ready = func() bool { return store.Ready() && challenges.Ready() }
	}
	handler := httpapi.NewHandler(logger, intake, ready)
	if challenges != nil {
		internal, err := httpapi.NewChallengeHandler(logger, challenges, config.OTP.ServiceToken)
		if err != nil {
			return err
		}
		mux := http.NewServeMux()
		mux.Handle("/internal/v1/email-challenges", internal)
		mux.Handle("/internal/v1/email-challenges/", internal)
		mux.Handle("/", handler)
		handler = mux
	}
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	finished := make(chan error, 5)
	var owned sync.WaitGroup
	start := func(run func() error) { owned.Add(1); go func() { defer owned.Done(); finished <- run() }() }
	for _, channelSender := range []struct {
		channel domain.Channel
		sender  application.Sender
	}{{domain.SMTP, smtp}, {domain.Telegram, telegram}} {
		worker := application.NewWorker(store, clock, mechanisms.Jitter{}, channelSender.sender, channelSender.channel, config.Policy)
		start(func() error { return runWorker(workerCtx, worker, mechanisms.Waiter{}, config.PollInterval) })
	}
	if challenges != nil {
		start(func() error {
			return runChallengeWorker(workerCtx, challenges, codeSender, mechanisms.Waiter{}, config.PollInterval)
		})
	} else {
		start(func() error {
			return runChallengeRetention(workerCtx, store, clock, mechanisms.Waiter{}, config.PollInterval)
		})
	}
	start(func() error {
		waiter := mechanisms.Waiter{}
		for {
			if err := waiter.Wait(workerCtx, 24*time.Hour); err != nil {
				return cancellationResult(workerCtx, err)
			}
			if err := store.Purge(workerCtx, clock.Now().AddDate(0, 0, -config.RetentionDays)); err != nil {
				return cancellationResult(workerCtx, fmt.Errorf("scheduled retention: %w", err))
			}
		}
	})
	start(func() error { return Serve(workerCtx, listener, handler) })
	first := <-finished
	cancel()
	owned.Wait()
	close(finished)
	for err := range finished {
		first = errors.Join(first, err)
	}
	return first
}

func runWorker(ctx context.Context, worker *application.Worker, waiter application.Waiter, poll time.Duration) error {
	for ctx.Err() == nil {
		worked, err := worker.ProcessOne(ctx)
		if err != nil {
			return cancellationResult(ctx, err)
		}
		if !worked {
			if err := waiter.Wait(ctx, poll); err != nil {
				return cancellationResult(ctx, err)
			}
		}
	}
	return nil
}

func runChallengeWorker(ctx context.Context, challenges *application.Challenges, sender application.CodeSender, waiter application.Waiter, poll time.Duration) error {
	for ctx.Err() == nil {
		if err := challenges.Sweep(ctx); err != nil {
			return cancellationResult(ctx, err)
		}
		worked, err := challenges.ProcessOne(ctx, sender)
		if err != nil {
			return cancellationResult(ctx, err)
		}
		if !worked {
			if err := waiter.Wait(ctx, poll); err != nil {
				return cancellationResult(ctx, err)
			}
		}
	}
	return nil
}

type challengePruner interface {
	PruneChallenges(context.Context, time.Time) error
}

func runChallengeRetention(ctx context.Context, store challengePruner, clock application.Clock, waiter application.Waiter, poll time.Duration) error {
	for ctx.Err() == nil {
		if err := store.PruneChallenges(ctx, clock.Now()); err != nil {
			return cancellationResult(ctx, err)
		}
		if err := waiter.Wait(ctx, poll); err != nil {
			return cancellationResult(ctx, err)
		}
	}
	return nil
}

func cancellationResult(ctx context.Context, err error) error {
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return nil
	}
	return err
}
