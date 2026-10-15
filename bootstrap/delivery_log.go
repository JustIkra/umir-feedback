package bootstrap

import (
	"context"
	"log/slog"

	"github.com/JustIkra/umir-feedback/internal/application"
)

// DeliveryLog writes one structured record per delivery transition.
// It logs only closed-set values and identifiers, never submission fields.
type DeliveryLog struct{ logger *slog.Logger }

func NewDeliveryLog(logger *slog.Logger) *DeliveryLog { return &DeliveryLog{logger: logger} }

func (l *DeliveryLog) ObserveDelivery(event application.DeliveryEvent) {
	level := slog.LevelError
	switch event.Outcome {
	case application.DeliverySent:
		level = slog.LevelInfo
	case application.DeliveryRetry:
		level = slog.LevelWarn
	}
	l.logger.LogAttrs(context.Background(), level, "feedback.delivery",
		slog.String("outcome", string(event.Outcome)),
		slog.String("channel", string(event.Channel)),
		slog.String("code", event.Code),
		slog.Int("attempt", event.Attempt),
		slog.String("job_id", event.JobID),
		slog.String("submission_id", string(event.SubmissionID)),
	)
}
