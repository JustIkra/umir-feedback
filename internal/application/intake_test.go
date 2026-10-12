package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

type memoryRepository struct {
	records     map[domain.SubmissionID]domain.Submission
	claims      []domain.Delivery
	finished    []domain.Job
	finishError error
}

func (s *memoryRepository) Accept(_ context.Context, in domain.Submission) (domain.Receipt, bool, error) {
	old, ok := s.records[in.ID]
	if ok {
		if old.Fingerprint != in.Fingerprint {
			return domain.Receipt{}, false, ErrIdentityConflict
		}
		return domain.Receipt{SubmissionID: old.ID, ReceivedAt: old.ReceivedAt}, false, nil
	}
	s.records[in.ID] = in
	return domain.Receipt{SubmissionID: in.ID, ReceivedAt: in.ReceivedAt}, true, nil
}
func (s *memoryRepository) Lookup(_ context.Context, id domain.SubmissionID) (domain.Submission, bool, error) {
	v, ok := s.records[id]
	return v, ok, nil
}
func (s *memoryRepository) Claim(_ context.Context, c domain.Channel, _ time.Time, _ domain.RetryPolicy) (domain.Delivery, bool, error) {
	for i, d := range s.claims {
		if d.Job.Channel == c {
			s.claims = append(s.claims[:i], s.claims[i+1:]...)
			return d, true, nil
		}
	}
	return domain.Delivery{}, false, nil
}
func (s *memoryRepository) Finish(_ context.Context, _ domain.SubmissionID, _ domain.Channel, j domain.Job) error {
	s.finished = append(s.finished, j)
	return s.finishError
}

type fixedClock struct{ at time.Time }

func (c fixedClock) Now() time.Time { return c.at }

type identityNormalizer struct{}

func (identityNormalizer) NFC(s string) string { return s }

func TestIntakeIdempotencyAndConsentRotation(t *testing.T) {
	ctx := context.Background()
	repo := &memoryRepository{records: make(map[domain.SubmissionID]domain.Submission)}
	clock := fixedClock{time.Date(2026, 10, 12, 12, 0, 0, 0, time.FixedZone("MSK", 10800))}
	in := domain.Input{ID: "123e4567-e89b-42d3-a456-426614174000", Name: " Operator ", Email: "a@EXAMPLE.ORG", Message: " hello ", Accepted: true, ConsentVersion: "v1"}
	service := NewIntake(repo, clock, identityNormalizer{}, "v1")
	first, created, err := service.Submit(ctx, in)
	if err != nil || !created {
		t.Fatalf("first: %v %v", created, err)
	}
	service = NewIntake(repo, clock, identityNormalizer{}, "v2")
	second, created, err := service.Submit(ctx, in)
	if err != nil || created || first != second {
		t.Fatalf("replay: %v %v", created, err)
	}
	in.Message = "changed"
	_, _, err = service.Submit(ctx, in)
	if !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("conflict: %v", err)
	}
	in.ID = "223e4567-e89b-42d3-a456-426614174000"
	_, _, err = service.Submit(ctx, in)
	var invalid *ValidationError
	if !errors.As(err, &invalid) || invalid.Code != "ConsentRequired" || invalid.Fields["consent.documentVersion"] != "ConsentOutdated" {
		t.Fatalf("consent: %v", err)
	}
}

func TestHoneypotNeverStores(t *testing.T) {
	repo := &memoryRepository{records: make(map[domain.SubmissionID]domain.Submission)}
	s := NewIntake(repo, fixedClock{time.Now()}, identityNormalizer{}, "v1")
	_, created, err := s.Submit(context.Background(), domain.Input{ID: "123e4567-e89b-42d3-a456-426614174000", Website: "trap"})
	if err != nil || !created || len(repo.records) != 0 {
		t.Fatal("honeypot was persisted or rejected")
	}
}
