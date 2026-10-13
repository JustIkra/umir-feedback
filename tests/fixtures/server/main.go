package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/JustIkra/umir-feedback/bootstrap"
	"github.com/JustIkra/umir-feedback/internal/adapters/authkeys"
	"github.com/JustIkra/umir-feedback/internal/adapters/delivery"
	"github.com/JustIkra/umir-feedback/internal/adapters/httpapi"
	"github.com/JustIkra/umir-feedback/internal/adapters/journal"
	mechanisms "github.com/JustIkra/umir-feedback/internal/adapters/runtime"
	"github.com/JustIkra/umir-feedback/internal/application"
	"github.com/JustIkra/umir-feedback/internal/domain"
	"github.com/JustIkra/umir-feedback/tests/fixtures/protocol"
)

func main() {
	var e error
	if len(os.Args) != 2 {
		e = errors.New("command required")
	} else {
		switch os.Args[1] {
		case "serve":
			e = serve()
		case "healthcheck":
			e = protocol.Healthcheck()
		default:
			e = errors.New("unsupported command")
		}
	}
	if e != nil {
		if _, err := fmt.Fprintln(os.Stderr, "Feedback fixture NotReady"); err != nil {
			os.Exit(1)
		}
		os.Exit(1)
	}
}
func serve() (returnErr error) {
	cfg, digest, e := protocol.LoadConfig(protocol.ConfigPath)
	if e != nil {
		return e
	}
	credential, e := protocol.ReadCredential(cfg.ControlCredentialFile)
	if e != nil {
		return e
	}
	keys := map[string]string{}
	seen := map[string]bool{credential: true}
	for name, ref := range cfg.CredentialFiles {
		key, e := protocol.ReadCredential(ref)
		if e != nil || seen[key] {
			return errors.New("missing distinct discovery credentials")
		}
		seen[key] = true
		keys[name] = key
	}
	instance, e := protocol.UUID()
	if e != nil {
		return e
	}
	clock, e := protocol.NewClock(cfg.FixtureID, instance, cfg.InitialInstant, cfg.ClockGeneration)
	if e != nil {
		return e
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	store, e := journal.Open(journal.Config{Path: cfg.StateFile, MaxBytes: 16 << 20, UID: os.Getuid(), GID: os.Getgid(), Policy: policy(), Now: clock.Now})
	if e != nil {
		return e
	}
	defer func() { returnErr = errors.Join(returnErr, store.Close()) }()
	faults := &protocol.Faults{}
	repo := &protocol.FaultRepository{Store: store, Faults: faults}
	intake := application.NewIntake(repo, clock, mechanisms.Normalizer{}, "e2e-fixture-v1")
	transport := &http.Transport{DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, ResponseHeaderTimeout: 5 * time.Second, MaxResponseHeaderBytes: 16384}
	defer transport.CloseIdleConnections()
	keyClient, e := authkeys.NewClient(authkeys.Options{URL: cfg.ServiceURLs["auth"] + "/internal/auth/v1/jwks", ServiceKey: keys["feedback-auth"], Now: clock.Now, HTTP: &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, AllowPlainHTTP: true})
	if e != nil {
		return e
	}
	verifier, e := httpapi.NewJWTVerifier(httpapi.JWTOptions{Now: clock.Now, Keys: keyClient})
	if e != nil {
		return e
	}
	ready := func(check context.Context) bool {
		if !store.Ready() {
			return false
		}
		_, e := keyClient.Key(check, "fixture-readiness")
		return e == nil || errors.Is(e, application.ErrInvalidToken)
	}
	handler, e := httpapi.NewScopedHandler(httpapi.ScopedOptions{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Intake: intake, Reader: store, Verifier: verifier, ServiceKey: keys["gateway-feedback"], Now: clock.Now, Ready: func() bool {
		check, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return ready(check)
	}})
	if e != nil {
		return e
	}
	binary, e := protocol.BinarySHA()
	if e != nil {
		return e
	}
	control, e := protocol.NewHandler(protocol.HandlerOptions{Project: cfg.Project, Credential: credential, BinarySHA256: binary, ConfigSHA256: digest, Clock: clock, Store: store, Faults: faults, Ready: ready})
	if e != nil {
		return e
	}
	smtp, telegram, e := syntheticSenders(cfg)
	if e != nil {
		return e
	}
	defer func() { returnErr = errors.Join(returnErr, telegram.Close()) }()
	listener, e := net.Listen("tcp", fmt.Sprintf(":%d", cfg.PrivatePort))
	if e != nil {
		return e
	}
	defer func() {
		if e := listener.Close(); e != nil && !errors.Is(e, net.ErrClosed) {
			returnErr = errors.Join(returnErr, e)
		}
	}()
	socket, e := controlListener(cfg.ControlSocket)
	if e != nil {
		return e
	}
	server := &http.Server{Handler: control, ReadHeaderTimeout: time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 10 * time.Second, MaxHeaderBytes: 16384}
	done := make(chan error, 1)
	go func() { done <- server.Serve(socket) }()
	business := make(chan error, 1)
	go func() {
		business <- bootstrap.RunOwner(ctx, listener, faults.WrapHTTP(handler), store, clock, map[domain.Channel]application.Sender{domain.SMTP: smtp, domain.Telegram: telegram}, policy(), 100*time.Millisecond)
	}()
	var ce, be error
	controlJoined, businessJoined := false, false
	select {
	case ce = <-done:
		controlJoined = true
	case be = <-business:
		businessJoined = true
	case <-ctx.Done():
	}
	stop()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	se := server.Shutdown(shutdown)
	cancel()
	if se != nil {
		se = errors.Join(se, server.Close())
	}
	if !controlJoined {
		ce = <-done
	}
	if !businessJoined {
		be = <-business
	}
	if errors.Is(ce, http.ErrServerClosed) {
		ce = nil
	}
	return errors.Join(be, ce, se)
}
func policy() domain.RetryPolicy {
	return domain.RetryPolicy{MaxAttempts: 3, MaxAge: 24 * time.Hour, MinDelay: time.Second, MaxDelay: time.Minute}
}
func syntheticSenders(cfg protocol.Config) (*delivery.SMTP, *delivery.Telegram, error) {
	var binding struct {
		SMTPHost          string `json:"smtpHost"`
		SMTPPort          int    `json:"smtpPort"`
		SMTPTLS           string `json:"smtpTLS"`
		NotificationProxy string `json:"notificationProxy"`
	}
	if json.Unmarshal(cfg.Delivery, &binding) != nil || binding.SMTPHost != "smtp" || binding.SMTPPort != 2525 || binding.SMTPTLS != "starttls" || binding.NotificationProxy != "http://notification:3128" {
		return nil, nil, errors.New("invalid synthetic delivery")
	}
	secrets := map[string][]byte{}
	defer func() {
		for _, v := range secrets {
			clear(v)
		}
	}()
	for _, name := range []string{"smtp-username", "smtp-password", "proxy-username", "proxy-password", "telegram-bot", "synthetic-ca.pem"} {
		v, e := protocol.ReadSecret(name, 16384)
		if e != nil {
			return nil, nil, e
		}
		secrets[name] = v
	}
	if string(secrets["smtp-username"]) != "umir-e2e" || string(secrets["proxy-username"]) != "umir-e2e" || len(secrets["smtp-password"]) != 32 || len(secrets["proxy-password"]) != 32 || len(secrets["telegram-bot"]) != 34 {
		return nil, nil, errors.New("invalid synthetic credential")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(secrets["synthetic-ca.pem"]) {
		return nil, nil, errors.New("invalid synthetic trust")
	}
	smtp, e := delivery.NewSMTPWithOptions(delivery.SMTPConfig{Host: binding.SMTPHost, Port: binding.SMTPPort, TLSMode: binding.SMTPTLS, Username: string(secrets["smtp-username"]), Password: string(secrets["smtp-password"]), From: "sender@umir.invalid", To: "feedback@umir.invalid"}, delivery.SMTPOptions{RootCAs: roots, Observer: protocol.Observer{}})
	if e != nil {
		return nil, nil, e
	}
	telegram, e := delivery.NewTelegramWithOptions(delivery.TelegramConfig{Token: string(secrets["telegram-bot"]), ChatID: "1", ProxyURL: binding.NotificationProxy, ProxyUsername: string(secrets["proxy-username"]), ProxyPassword: string(secrets["proxy-password"])}, delivery.TelegramOptions{RootCAs: roots, Observer: protocol.Observer{}})
	return smtp, telegram, e
}
func controlListener(path string) (net.Listener, error) {
	parent := filepath.Dir(path)
	if e := os.MkdirAll(parent, 0700); e != nil {
		return nil, e
	}
	info, e := os.Lstat(parent)
	if e != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, errors.New("unsafe control directory")
	}
	if _, e = os.Lstat(path); !errors.Is(e, os.ErrNotExist) {
		return nil, errors.New("existing control socket")
	}
	listener, e := net.Listen("unix", path)
	if e != nil {
		return nil, e
	}
	if e = os.Chmod(path, 0600); e != nil {
		return nil, errors.Join(e, listener.Close())
	}
	if e = protocol.CheckControlSocket(path); e != nil {
		return nil, errors.Join(e, listener.Close())
	}
	return listener, nil
}
