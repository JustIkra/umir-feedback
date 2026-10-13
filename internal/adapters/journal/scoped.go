package journal

import (
	"context"
	"errors"
	"time"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

type Selector struct {
	Namespace domain.Namespace
	ID        domain.SubmissionID
}
type JobView struct {
	JobID      string          `json:"jobID"`
	Channel    domain.Channel  `json:"channel"`
	State      domain.JobState `json:"state"`
	Attempts   int             `json:"attempts"`
	AcceptedAt time.Time       `json:"acceptedAt"`
}
type RecordView struct {
	SubmissionID domain.SubmissionID `json:"submissionID"`
	Namespace    domain.Namespace    `json:"namespace"`
	CommandID    string              `json:"commandID"`
	Fingerprint  string              `json:"fingerprint"`
	ReceivedAt   time.Time           `json:"receivedAt"`
	Jobs         []JobView           `json:"jobs"`
}

func (s *Store) Observe(ctx context.Context, selectors []Selector) ([]RecordView, bool, error) {
	if err := contextError(ctx); err != nil {
		return nil, false, err
	}
	if len(selectors) == 0 || len(selectors) > 64 {
		return nil, false, errors.New("invalid selector bound")
	}
	seen := map[domain.SubmissionID]bool{}
	for _, p := range selectors {
		if !p.Namespace.Valid() || !domain.ValidID(p.ID) || seen[p.Namespace.Key(p.ID)] {
			return nil, false, errors.New("invalid selector")
		}
		seen[p.Namespace.Key(p.ID)] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return nil, false, err
	}
	rows := make([]RecordView, 0, len(selectors))
	for _, p := range selectors {
		item, ok := s.entries[p.Namespace.Key(p.ID)]
		if !ok {
			continue
		}
		sub := item.submission
		row := RecordView{SubmissionID: sub.ID, Namespace: sub.Namespace, CommandID: sub.CommandID, Fingerprint: sub.Fingerprint, ReceivedAt: sub.ReceivedAt, Jobs: make([]JobView, 0, 2)}
		for _, ch := range []domain.Channel{domain.SMTP, domain.Telegram} {
			j := item.jobs[ch]
			row.Jobs = append(row.Jobs, JobView{JobID: j.ID, Channel: j.Channel, State: j.State, Attempts: j.Attempts, AcceptedAt: sub.ReceivedAt})
		}
		rows = append(rows, row)
	}
	return rows, len(rows) == len(selectors), nil
}
func (s *Store) ListScoped(ctx context.Context, org, tenant string) ([]domain.Submission, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return nil, err
	}
	rows := []domain.Submission{}
	for _, e := range sortedEntries(s.entries) {
		if e.submission.Namespace.Kind == "app" && e.submission.Namespace.OrgID == org && e.submission.Namespace.TenantID == tenant {
			rows = append(rows, e.submission)
		}
	}
	return rows, nil
}
