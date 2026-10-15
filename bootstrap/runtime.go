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

	"github.com/JustIkra/umir-feedback/internal/adapters/authkeys"

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
	handler := httpapi.NewHandler(logger, intake, store.Ready)
	if config.AuthURL != "" {
		keys, err := authkeys.NewClient(authkeys.Options{URL: config.AuthURL + "/internal/auth/v1/jwks", ServiceKey: config.DiscoveryKey, Now: clock.Now, MaxStale: config.AuthKeysMaxStale, HTTP: &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}})
		if err != nil {
			return err
		}
		verifier, err := httpapi.NewJWTVerifier(httpapi.JWTOptions{Now: clock.Now, Keys: keys})
		if err != nil {
			return err
		}
		handler, err = httpapi.NewScopedHandler(httpapi.ScopedOptions{Logger: logger, Intake: intake, Reader: store, Verifier: verifier, ServiceKey: config.GatewayKey, Now: clock.Now, Ready: store.Ready})
		if err != nil {
			return err
		}
	}
	return RunOwner(ctx, listener, handler, store, clock, map[domain.Channel]application.Sender{domain.SMTP: smtp, domain.Telegram: telegram}, config.Policy, config.PollInterval)
}

func RunOwner(ctx context.Context, listener net.Listener, handler http.Handler, store *journal.Store, clock application.Clock, senders map[domain.Channel]application.Sender, policy domain.RetryPolicy, poll time.Duration) error {
	if listener == nil || handler == nil || store == nil || clock == nil || senders[domain.SMTP] == nil || senders[domain.Telegram] == nil || !policy.Valid() || poll <= 0 || poll > time.Minute {
		return application.ErrInvalidArgument
	}
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	finished := make(chan error, 3)
	var owned sync.WaitGroup
	start := func(run func() error) { owned.Add(1); go func() { defer owned.Done(); finished <- run() }() }
	for _, channelSender := range []struct {
		channel domain.Channel
		sender  application.Sender
	}{{domain.SMTP, senders[domain.SMTP]}, {domain.Telegram, senders[domain.Telegram]}} {
		worker := application.NewWorker(store, clock, mechanisms.Jitter{}, channelSender.sender, channelSender.channel, policy)
		start(func() error { return runWorker(workerCtx, worker, mechanisms.Waiter{}, poll) })
	}
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

func cancellationResult(ctx context.Context, err error) error {
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return nil
	}
	return err
}
