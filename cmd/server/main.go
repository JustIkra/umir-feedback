package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/JustIkra/umir-feedback/bootstrap"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if len(os.Args) > 1 {
		if len(os.Args) != 2 || os.Args[1] != "healthcheck" {
			logger.Error("unknown command")
			os.Exit(1)
		}
		if err := bootstrap.Healthcheck(os.Getenv); err != nil {
			logger.Error("process health unavailable")
			os.Exit(1)
		}
		return
	}
	config, err := bootstrap.LoadConfig()
	if err != nil {
		logger.Error("configuration rejected", "parameter", err.Error())
		os.Exit(1)
	}
	listener, err := net.Listen("tcp", config.ListenAddress)
	if err != nil {
		logger.Error("listener unavailable")
		os.Exit(1)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	err = bootstrap.Run(ctx, listener, logger, config)
	cancel()
	if err != nil {
		logger.Error("feedback stopped", "code", "RuntimeUnavailable")
		os.Exit(1)
	}
}
