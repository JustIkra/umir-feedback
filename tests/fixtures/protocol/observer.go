package protocol

import (
	"context"
	"errors"
	"net/http"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

type Observer struct{}

func (Observer) Message(_ context.Context, d domain.Delivery, message []byte) ([]byte, error) {
	if !uuidPattern.MatchString(d.Job.ID) || !uuidPattern.MatchString(string(d.Submission.ID)) {
		return nil, errors.New("missing durable correlation")
	}
	headers := []byte("X-Umir-Fixture-Job-ID: " + d.Job.ID + "\r\nX-Umir-Fixture-Submission-ID: " + string(d.Submission.ID) + "\r\n")
	return append(headers, message...), nil
}
func (Observer) Request(_ context.Context, d domain.Delivery, r *http.Request) error {
	if !uuidPattern.MatchString(d.Job.ID) || !uuidPattern.MatchString(string(d.Submission.ID)) {
		return errors.New("missing durable correlation")
	}
	r.Header.Set("X-Umir-Fixture-Job-ID", d.Job.ID)
	r.Header.Set("X-Umir-Fixture-Submission-ID", string(d.Submission.ID))
	return nil
}
