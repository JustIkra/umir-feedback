package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"strings"
	"time"

	"github.com/JustIkra/umir-feedback/internal/adapters/journal"
	mechanisms "github.com/JustIkra/umir-feedback/internal/adapters/runtime"
	"github.com/JustIkra/umir-feedback/internal/application"
	"github.com/JustIkra/umir-feedback/internal/domain"
)

type statusRecord struct {
	SubmissionID   domain.SubmissionID `json:"submissionID"`
	Channel        domain.Channel      `json:"channel"`
	State          domain.JobState     `json:"state"`
	Attempts       int                 `json:"attempts"`
	NextAttemptAt  time.Time           `json:"nextAttemptAt,omitzero"`
	StartedAt      time.Time           `json:"startedAt,omitzero"`
	ConfirmedAt    time.Time           `json:"confirmedAt,omitzero"`
	RetryStartedAt time.Time           `json:"retryStartedAt,omitzero"`
	ErrorCode      string              `json:"errorCode,omitempty"`
	ProviderID     string              `json:"providerID,omitempty"`
}
type exportRecord struct {
	SubmissionID   domain.SubmissionID `json:"submissionID"`
	ReceivedAt     time.Time           `json:"receivedAt"`
	Name           string              `json:"name"`
	Email          string              `json:"email"`
	Phone          string              `json:"phone,omitempty"`
	Message        string              `json:"message"`
	ConsentVersion string              `json:"consentVersion"`
	AcceptedAt     time.Time           `json:"acceptedAt"`
	Purpose        string              `json:"purpose"`
}

func Command(ctx context.Context, args []string, getenv func(string) string, out io.Writer) (returnErr error) {
	if len(args) == 0 {
		return errors.New("usage: feedback export|delete|purge|status|retry")
	}
	command := args[0]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var since, email, id, channel string
	switch command {
	case "export":
		flags.StringVar(&since, "since", "", "Moscow date YYYY-MM-DD")
	case "delete":
		flags.StringVar(&email, "email", "", "exact normalized email")
	case "retry":
		flags.StringVar(&id, "submission-id", "", "UUID v4")
		flags.StringVar(&channel, "channel", "", "smtp or telegram")
	case "purge", "status":
	default:
		return errors.New("unknown feedback command")
	}
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
		return errors.New("invalid command arguments")
	}
	if command == "delete" && !domain.ValidEmail(domain.NormalizeEmail(email)) {
		return errors.New("invalid --email")
	}
	if command == "retry" && (!domain.ValidID(domain.SubmissionID(id)) || (channel != "smtp" && channel != "telegram")) {
		return errors.New("invalid --submission-id or --channel")
	}
	clock, err := mechanisms.NewClock()
	if err != nil {
		return errors.New("clock unavailable")
	}
	var sinceAt time.Time
	if since != "" {
		sinceAt, err = time.ParseInLocation("2006-01-02", since, clock.Now().Location())
		if err != nil {
			return errors.New("invalid --since; use YYYY-MM-DD")
		}
	}
	config, err := mechanisms.LoadStoreConfig(getenv)
	if err != nil {
		return err
	}
	store, err := journal.Open(journal.Config{Path: config.StorePath, MaxBytes: config.MaxStoreBytes, UID: os.Getuid(), GID: os.Getgid(), Policy: config.Policy, Now: clock.Now})
	if err != nil {
		if errors.Is(err, application.ErrBusy) {
			return errors.New("feedback store is locked; stop the service first")
		}
		return errors.New("feedback store unavailable")
	}
	defer func() {
		if err := store.Close(); err != nil {
			returnErr = errors.Join(returnErr, errors.New("feedback store close failed"))
		}
	}()
	encoder := json.NewEncoder(out)
	switch command {
	case "export":
		records, err := store.Export(ctx, sinceAt)
		if err != nil {
			return errors.New("feedback export failed")
		}
		for _, r := range records {
			if err := encoder.Encode(exportRecord{SubmissionID: r.ID, ReceivedAt: r.ReceivedAt, Name: r.Name, Email: r.Email, Phone: r.Phone, Message: r.Message, ConsentVersion: r.ConsentVersion, AcceptedAt: r.AcceptedAt, Purpose: r.ConsentPurpose}); err != nil {
				return errors.New("export output failed")
			}
		}
	case "status":
		jobs, err := store.Status(ctx)
		if err != nil {
			return errors.New("feedback status failed")
		}
		for _, d := range jobs {
			j := d.Job
			if err := encoder.Encode(statusRecord{SubmissionID: d.Submission.ID, Channel: j.Channel, State: j.State, Attempts: j.Attempts, NextAttemptAt: j.NextAttemptAt, StartedAt: j.StartedAt, ConfirmedAt: j.ConfirmedAt, RetryStartedAt: j.RetryStartedAt, ErrorCode: j.ErrorCode, ProviderID: j.ProviderID}); err != nil {
				return errors.New("status output failed")
			}
		}
	case "delete":
		if err := store.DeleteEmail(ctx, domain.NormalizeEmail(email)); err != nil {
			return errors.New("feedback deletion failed")
		}
	case "purge":
		if err := store.Purge(ctx, clock.Now().AddDate(0, 0, -config.RetentionDays)); err != nil {
			return errors.New("feedback purge failed")
		}
	case "retry":
		if err := store.Retry(ctx, domain.SubmissionID(strings.ToLower(id)), domain.Channel(channel), clock.Now()); err != nil {
			return errors.New("feedback retry rejected")
		}
	}
	return nil
}
