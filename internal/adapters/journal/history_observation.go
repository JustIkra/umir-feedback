package journal

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"time"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

type ArchiveChallengeID string

type HistoryJob struct {
	JobID           string
	Channel         domain.Channel
	State           domain.JobState
	Attempts        int
	NextAttemptAt   time.Time
	RetryStartedAt  time.Time
	StartedAt       time.Time
	ConfirmedAt     time.Time
	ErrorCode       string
	ProviderIDProof string
}

type HistoryRecord struct {
	SubmissionID domain.SubmissionID
	Namespace    domain.Namespace
	Fingerprint  string
	AcceptedAt   time.Time
	ReceivedAt   time.Time
	Jobs         []HistoryJob
}

type HistoryIssue struct {
	IssueID           string
	AcceptedAt        time.Time
	ResendAvailableAt time.Time
	OriginalJob       HistoryJob
}

type HistoryArchive struct {
	ChallengeID       ArchiveChallengeID
	CreatedAt         time.Time
	ExpiresAt         time.Time
	ResendAvailableAt time.Time
	RetainUntil       time.Time
	Issues            []HistoryIssue
}

type HistorySnapshot struct {
	JournalBytes int64
	Records      []HistoryRecord
	Archives     []HistoryArchive
}

func (s *Store) ObserveHistory(ctx context.Context, selectors []Selector, challengeIDs []ArchiveChallengeID, proofKey [32]byte) (HistorySnapshot, error) {
	if err := contextError(ctx); err != nil {
		return HistorySnapshot{}, err
	}
	if len(selectors) > 16 || len(challengeIDs) > 16 || len(selectors)+len(challengeIDs) == 0 || proofKey == ([32]byte{}) {
		return HistorySnapshot{}, errors.New("invalid history observation bounds or proof key")
	}
	seen := map[domain.SubmissionID]bool{}
	for _, selector := range selectors {
		key := selector.Namespace.Key(selector.ID)
		if selector.Namespace != domain.Anonymous() || !domain.ValidID(selector.ID) || seen[key] {
			return HistorySnapshot{}, errors.New("invalid history selector")
		}
		seen[key] = true
	}
	archives := map[ArchiveChallengeID]bool{}
	for _, id := range challengeIDs {
		if !domain.ValidID(domain.SubmissionID(id)) || archives[id] {
			return HistorySnapshot{}, errors.New("invalid archive selector")
		}
		archives[id] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return HistorySnapshot{}, err
	}
	if err := s.usable(); err != nil {
		return HistorySnapshot{}, err
	}
	result := HistorySnapshot{JournalBytes: s.bytes, Records: make([]HistoryRecord, 0, len(selectors)), Archives: make([]HistoryArchive, 0, len(challengeIDs))}
	for _, selector := range selectors {
		entry, ok := s.entries[selector.Namespace.Key(selector.ID)]
		if !ok || !entry.submission.Legacy || entry.submission.Namespace != selector.Namespace || entry.submission.ID != selector.ID {
			return HistorySnapshot{}, errors.New("history selection not found")
		}
		sub := entry.submission
		row := HistoryRecord{SubmissionID: sub.ID, Namespace: sub.Namespace, Fingerprint: sub.Fingerprint, AcceptedAt: sub.AcceptedAt, ReceivedAt: sub.ReceivedAt, Jobs: make([]HistoryJob, 0, 2)}
		for _, channel := range []domain.Channel{domain.SMTP, domain.Telegram} {
			job, ok := entry.jobs[channel]
			if !ok {
				return HistorySnapshot{}, errors.New("history channel edge missing")
			}
			projection, err := observeHistoryJob(job, proofKey)
			if err != nil {
				return HistorySnapshot{}, err
			}
			row.Jobs = append(row.Jobs, projection)
		}
		result.Records = append(result.Records, row)
	}
	for _, id := range challengeIDs {
		challenge, ok := s.challenges[legacyChallengeID(id)]
		if !ok {
			return HistorySnapshot{}, errors.New("archive selection not found")
		}
		if len(challenge.Issues) > maxChallengeIssues {
			return HistorySnapshot{}, errors.New("archive issue bound exceeded")
		}
		row := HistoryArchive{ChallengeID: id, CreatedAt: challenge.CreatedAt, ExpiresAt: challenge.ExpiresAt, ResendAvailableAt: challenge.ResendAvailableAt, RetainUntil: challenge.RetainUntil, Issues: make([]HistoryIssue, 0, len(challenge.Issues))}
		for _, issue := range challenge.Issues {
			projection, err := observeHistoryJob(issue.Job, proofKey)
			if err != nil {
				return HistorySnapshot{}, err
			}
			row.Issues = append(row.Issues, HistoryIssue{IssueID: string(issue.ID), AcceptedAt: issue.AcceptedAt, ResendAvailableAt: issue.ResendAvailableAt, OriginalJob: projection})
		}
		result.Archives = append(result.Archives, row)
	}
	sort.Slice(result.Records, func(i, j int) bool {
		return result.Records[i].Namespace.Key(result.Records[i].SubmissionID) < result.Records[j].Namespace.Key(result.Records[j].SubmissionID)
	})
	sort.Slice(result.Archives, func(i, j int) bool { return result.Archives[i].ChallengeID < result.Archives[j].ChallengeID })
	if err := contextError(ctx); err != nil {
		return HistorySnapshot{}, err
	}
	return result, nil
}

func observeHistoryJob(job domain.Job, key [32]byte) (HistoryJob, error) {
	proof := ""
	if job.ProviderID != "" {
		mac := hmac.New(sha256.New, key[:])
		if _, err := mac.Write([]byte(job.ProviderID)); err != nil {
			return HistoryJob{}, err
		}
		proof = hex.EncodeToString(mac.Sum(nil))
	}
	return HistoryJob{JobID: job.ID, Channel: job.Channel, State: job.State, Attempts: job.Attempts, NextAttemptAt: job.NextAttemptAt, RetryStartedAt: job.RetryStartedAt, StartedAt: job.StartedAt, ConfirmedAt: job.ConfirmedAt, ErrorCode: job.ErrorCode, ProviderIDProof: proof}, nil
}
