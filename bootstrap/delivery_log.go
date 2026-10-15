package bootstrap

import (
	"context"
	"log/slog"

	"github.com/JustIkra/umir-feedback/internal/application"
)

// DeliveryLog writes one structured record per delivery transition.
// It logs only closed-set values and identifiers, never submission fields.
type DeliveryLog struct{ logger *slog.Logger }

// NewDeliveryLog falls back to slog.Default when logger is nil.
func NewDeliveryLog(logger *slog.Logger) *DeliveryLog {
	if logger == nil {
		logger = slog.Default()
	}
	return &DeliveryLog{logger: logger}
}

func (l *DeliveryLog) ObserveDelivery(event application.DeliveryEvent) {
	level := slog.LevelError
	switch {
	case event.Outcome == application.DeliverySent:
		level = slog.LevelInfo
	case event.Outcome == application.DeliveryRetry:
		level = slog.LevelWarn
	case event.Cause == application.CauseLeaseLost || event.Cause == application.CauseNotFound:
		// Expected: another claim took the job over, or the submission was deleted.
		level = slog.LevelWarn
	}
	l.logger.LogAttrs(context.Background(), level, "feedback.delivery",
		slog.String("outcome", string(event.Outcome)),
		slog.String("channel", string(event.Channel)),
		slog.String("code", event.Code),
		slog.String("cause", string(event.Cause)),
		slog.Int("attempt", event.Attempt),
		slog.String("job_id", event.JobID),
		slog.String("submission_id", string(event.SubmissionID)),
	)
}
