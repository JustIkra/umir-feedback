package journal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/JustIkra/umir-feedback/internal/application"
	"github.com/JustIkra/umir-feedback/internal/domain"
)

const (
	maxChallengeRecordBytes = 8 << 10
	maxChallengeIssues      = 4
	maxChallengeVerifies    = 5
	maxChallengeAttempts    = 3
)

type wireChallenge struct {
	ID                string                 `json:"id"`
	RegistrationID    string                 `json:"registrationID"`
	Email             string                 `json:"email"`
	Purpose           string                 `json:"purpose"`
	CreatedAt         time.Time              `json:"createdAt"`
	ExpiresAt         time.Time              `json:"expiresAt"`
	ResendAvailableAt time.Time              `json:"resendAvailableAt"`
	RetainUntil       time.Time              `json:"retainUntil"`
	State             string                 `json:"state"`
	CodeDigest        []byte                 `json:"codeDigest"`
	EncryptedCode     wireSealedValue        `json:"encryptedCode"`
	BadAttempts       int                    `json:"badAttempts"`
	Verifications     []wireCodeVerification `json:"verifications"`
	Issues            []wireCodeIssuance     `json:"issues"`
	ProofDigest       []byte                 `json:"proofDigest"`
	EncryptedProof    wireSealedValue        `json:"encryptedProof"`
	VerifiedAt        time.Time              `json:"verifiedAt"`
	CompletionID      string                 `json:"completionID"`
	CompletedAt       time.Time              `json:"completedAt"`
}

type wireSealedValue struct {
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
}

type wireCodeVerification struct {
	ID       string `json:"id"`
	Digest   []byte `json:"digest"`
	Verified bool   `json:"verified"`
}

type wireCodeIssuance struct {
	ID                string    `json:"id"`
	AcceptedAt        time.Time `json:"acceptedAt"`
	ResendAvailableAt time.Time `json:"resendAvailableAt"`
	Job               wireJob   `json:"job"`
}

func (s *Store) Challenges(ctx context.Context) ([]domain.EmailChallenge, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return nil, err
	}
	items := sortedChallenges(s.challenges)
	result := make([]domain.EmailChallenge, 0, len(items))
	for _, item := range items {
		result = append(result, domain.CloneEmailChallenge(item))
	}
	return result, nil
}

func (s *Store) SaveChallenge(ctx context.Context, challenge domain.EmailChallenge) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return err
	}
	if err := validateChallengeSnapshot(challenge); err != nil {
		return err
	}
	if _, err := encodeChallengeRecord(challenge); err != nil {
		return err
	}
	current, exists := s.challenges[challenge.ID]
	if exists {
		if !sameChallengeBinding(current, challenge) {
			return application.ErrChallengeIdentityConflict
		}
		if sameChallengeSnapshot(current, challenge) {
			return nil
		}
	} else {
		if len(s.challenges) >= maxChallengeCount {
			return application.ErrChallengeCapacity
		}
		for _, existing := range s.challenges {
			if existing.RegistrationID == challenge.RegistrationID {
				return application.ErrChallengeIdentityConflict
			}
		}
	}

	projected := make(map[domain.ChallengeID]domain.EmailChallenge, len(s.challenges)+1)
	for id, existing := range s.challenges {
		projected[id] = existing
	}
	projected[challenge.ID] = domain.CloneEmailChallenge(challenge)
	line, err := encodeChallengeRecord(challenge)
	if err != nil {
		return err
	}
	if err := s.append(line, nil, reservedForState(s.entries, projected, s.config.Policy)); err != nil {
		if errors.Is(err, errCapacity) {
			return application.ErrChallengeCapacity
		}
		if s.failed {
			return errors.Join(application.ErrStorageOutcomeUnknown, err)
		}
		return err
	}
	s.challenges[challenge.ID] = domain.CloneEmailChallenge(challenge)
	if hasCiphertext(current.EncryptedCode) && !hasCiphertext(challenge.EncryptedCode) {
		s.challengeScrubNeeded = true
		if err := s.compact(s.entries, s.challenges); err != nil {
			s.failed = true
			return errors.Join(application.ErrStorageOutcomeUnknown, err)
		}
	}
	return nil
}

func (s *Store) PruneChallenges(ctx context.Context, now time.Time) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return err
	}
	return s.pruneChallengesLocked(now)
}

func (s *Store) pruneChallengesLocked(now time.Time) error {
	if now.IsZero() {
		return errors.New("challenge prune time is required")
	}
	now = now.In(moscowLocation())
	kept := make(map[domain.ChallengeID]domain.EmailChallenge, len(s.challenges))
	changed := false
	for id, stored := range s.challenges {
		challenge := domain.CloneEmailChallenge(stored)
		if !now.Before(challenge.RetainUntil) {
			changed = true
			continue
		}
		if !now.Before(challenge.ExpiresAt) || challenge.State == domain.ChallengeVerified || challenge.State == domain.ChallengeCompleted {
			if hasCiphertext(challenge.EncryptedCode) || len(challenge.EncryptedCode.Nonce) != 0 {
				challenge.EncryptedCode = domain.SealedValue{}
				changed = true
			}
			if challenge.State == domain.ChallengePending && !now.Before(challenge.ExpiresAt) {
				challenge.State = domain.ChallengeExpired
				changed = true
			}
			if cancelIssueJobs(&challenge, now, issueCancellationCode(challenge.State)) {
				changed = true
			}
		}
		if err := validateChallengeSnapshot(challenge); err != nil {
			return fmt.Errorf("pruned challenge %q is invalid: %w", id, err)
		}
		kept[id] = challenge
	}
	if !changed && !s.challengeScrubNeeded {
		return nil
	}
	if changed {
		s.challengeScrubNeeded = true
	}
	if err := s.compact(s.entries, kept); err != nil {
		s.failed = true
		return errors.Join(application.ErrStorageOutcomeUnknown, err)
	}
	return nil
}

func cancelIssueJobs(challenge *domain.EmailChallenge, now time.Time, code string) bool {
	changed := false
	for index := range challenge.Issues {
		job := &challenge.Issues[index].Job
		if job.State != domain.Pending && job.State != domain.InFlight {
			continue
		}
		job.State = domain.Failed
		job.NextAttemptAt = time.Time{}
		job.ConfirmedAt = now
		job.ErrorCode = code
		job.ProviderID = ""
		changed = true
	}
	return changed
}

func issueCancellationCode(state domain.ChallengeState) string {
	if state == domain.ChallengeVerified || state == domain.ChallengeCompleted {
		return "otp_consumed"
	}
	return "otp_expired"
}

func hasCiphertext(value domain.SealedValue) bool {
	return len(value.Ciphertext) != 0
}

func reservedForChallenge(challenge domain.EmailChallenge) int64 {
	events := 0
	if challenge.State == domain.ChallengePending {
		if remaining := maxChallengeVerifies - len(challenge.Verifications); remaining > 0 {
			events += remaining
		}
		events++
		events++
		if remaining := maxChallengeIssues - len(challenge.Issues); remaining > 0 {
			events += remaining * (1 + 2*maxChallengeAttempts)
		}
	} else if challenge.State == domain.ChallengeVerified && challenge.CompletionID == "" {
		events++
	}
	for _, issue := range challenge.Issues {
		remaining := maxChallengeAttempts - issue.Job.Attempts
		if remaining < 0 {
			remaining = 0
		}
		switch issue.Job.State {
		case domain.Pending:
			events += 2 * remaining
		case domain.InFlight:
			events += 1 + 2*remaining
		}
	}
	return int64(events) * maxChallengeRecordBytes
}

func sameChallengeBinding(left, right domain.EmailChallenge) bool {
	return left.ID == right.ID && left.RegistrationID == right.RegistrationID && left.Email == right.Email && left.Purpose == right.Purpose && left.CreatedAt.Equal(right.CreatedAt) && left.ExpiresAt.Equal(right.ExpiresAt)
}

func validateChallengeSnapshot(challenge domain.EmailChallenge) error {
	if err := domain.ValidateEmailChallenge(challenge); err != nil {
		return err
	}
	retainUntil := challenge.CreatedAt.Add(24 * time.Hour)
	if challenge.State == domain.ChallengeVerified || challenge.State == domain.ChallengeCompleted {
		retainUntil = challenge.VerifiedAt.Add(24 * time.Hour)
	}
	if !challenge.RetainUntil.Equal(retainUntil) {
		return errors.New("challenge retention deadline is invalid")
	}
	for _, issue := range challenge.Issues {
		job := issue.Job
		if len(job.ErrorCode) > maxErrorCodeSize || len(job.ProviderID) > maxProviderSize || !validErrorCode(job.ErrorCode) || !validProviderID(job.ProviderID) {
			return errors.New("challenge issuance job is invalid")
		}
	}
	return nil
}

func sameChallengeSnapshot(left, right domain.EmailChallenge) bool {
	leftLine, leftErr := encodeChallengeRecord(left)
	rightLine, rightErr := encodeChallengeRecord(right)
	return leftErr == nil && rightErr == nil && string(leftLine) == string(rightLine)
}

func encodeChallengeRecord(challenge domain.EmailChallenge) ([]byte, error) {
	line, err := encodeRecord(record{Version: formatVersion, Type: "challenge", Challenge: pointer(toWireChallenge(challenge))})
	if err != nil {
		return nil, err
	}
	if len(line) > maxChallengeRecordBytes {
		return nil, application.ErrChallengeCapacity
	}
	return line, nil
}

func toWireChallenge(challenge domain.EmailChallenge) wireChallenge {
	wire := wireChallenge{
		ID: string(challenge.ID), RegistrationID: string(challenge.RegistrationID), Email: challenge.Email, Purpose: string(challenge.Purpose),
		CreatedAt: challenge.CreatedAt, ExpiresAt: challenge.ExpiresAt, ResendAvailableAt: challenge.ResendAvailableAt,
		RetainUntil: challenge.RetainUntil, State: string(challenge.State), CodeDigest: challenge.CodeDigest,
		EncryptedCode: wireSealedValue{Nonce: challenge.EncryptedCode.Nonce, Ciphertext: challenge.EncryptedCode.Ciphertext},
		BadAttempts:   challenge.BadAttempts, ProofDigest: challenge.ProofDigest,
		EncryptedProof: wireSealedValue{Nonce: challenge.EncryptedProof.Nonce, Ciphertext: challenge.EncryptedProof.Ciphertext},
		VerifiedAt:     challenge.VerifiedAt, CompletionID: string(challenge.CompletionID), CompletedAt: challenge.CompletedAt,
	}
	wire.Verifications = make([]wireCodeVerification, len(challenge.Verifications))
	for index, verification := range challenge.Verifications {
		wire.Verifications[index] = wireCodeVerification{ID: string(verification.ID), Digest: verification.Digest, Verified: verification.Verified}
	}
	wire.Issues = make([]wireCodeIssuance, len(challenge.Issues))
	for index, issue := range challenge.Issues {
		wire.Issues[index] = wireCodeIssuance{ID: string(issue.ID), AcceptedAt: issue.AcceptedAt, ResendAvailableAt: issue.ResendAvailableAt, Job: toWireJob(issue.Job)}
	}
	return wire
}

func fromWireChallenge(wire wireChallenge) domain.EmailChallenge {
	challenge := domain.EmailChallenge{
		ID: domain.ChallengeID(wire.ID), RegistrationID: domain.RegistrationID(wire.RegistrationID), Email: wire.Email, Purpose: domain.ChallengePurpose(wire.Purpose),
		CreatedAt: wire.CreatedAt, ExpiresAt: wire.ExpiresAt, ResendAvailableAt: wire.ResendAvailableAt,
		RetainUntil: wire.RetainUntil, State: domain.ChallengeState(wire.State), CodeDigest: wire.CodeDigest,
		EncryptedCode: domain.SealedValue{Nonce: wire.EncryptedCode.Nonce, Ciphertext: wire.EncryptedCode.Ciphertext}, BadAttempts: wire.BadAttempts,
		ProofDigest: wire.ProofDigest, EncryptedProof: domain.SealedValue{Nonce: wire.EncryptedProof.Nonce, Ciphertext: wire.EncryptedProof.Ciphertext},
		VerifiedAt: wire.VerifiedAt, CompletionID: domain.CompletionID(wire.CompletionID), CompletedAt: wire.CompletedAt,
	}
	challenge.Verifications = make([]domain.CodeVerification, len(wire.Verifications))
	for index, verification := range wire.Verifications {
		challenge.Verifications[index] = domain.CodeVerification{ID: domain.VerificationID(verification.ID), Digest: verification.Digest, Verified: verification.Verified}
	}
	challenge.Issues = make([]domain.CodeIssuance, len(wire.Issues))
	for index, issue := range wire.Issues {
		challenge.Issues[index] = domain.CodeIssuance{ID: domain.IssuanceID(issue.ID), AcceptedAt: issue.AcceptedAt, ResendAvailableAt: issue.ResendAvailableAt, Job: fromWireJob(issue.Job)}
	}
	return challenge
}

func validateChallengeFields(data []byte) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return err
	}
	keys := []string{"id", "registrationID", "email", "purpose", "createdAt", "expiresAt", "resendAvailableAt", "retainUntil", "state", "codeDigest", "encryptedCode", "badAttempts", "verifications", "issues", "proofDigest", "encryptedProof", "verifiedAt", "completionID", "completedAt"}
	if err := validateObjectKeys(object, keys, keys); err != nil {
		return err
	}
	for _, name := range []string{"encryptedCode", "encryptedProof"} {
		var value map[string]json.RawMessage
		if err := json.Unmarshal(object[name], &value); err != nil {
			return err
		}
		sealedKeys := []string{"nonce", "ciphertext"}
		if err := validateObjectKeys(value, sealedKeys, sealedKeys); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	var verifications []json.RawMessage
	if err := json.Unmarshal(object["verifications"], &verifications); err != nil {
		return err
	}
	if len(verifications) > maxChallengeVerifies {
		return errors.New("challenge has too many verification records")
	}
	for _, raw := range verifications {
		var value map[string]json.RawMessage
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		verificationKeys := []string{"id", "digest", "verified"}
		if err := validateObjectKeys(value, verificationKeys, verificationKeys); err != nil {
			return err
		}
	}
	var issues []json.RawMessage
	if err := json.Unmarshal(object["issues"], &issues); err != nil {
		return err
	}
	if len(issues) > maxChallengeIssues {
		return errors.New("challenge has too many issuance records")
	}
	for _, raw := range issues {
		var value map[string]json.RawMessage
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		issueKeys := []string{"id", "acceptedAt", "resendAvailableAt", "job"}
		if err := validateObjectKeys(value, issueKeys, issueKeys); err != nil {
			return err
		}
		if err := validateJobFields(value["job"]); err != nil {
			return fmt.Errorf("issuance job: %w", err)
		}
	}
	return nil
}

func sortedChallenges(challenges map[domain.ChallengeID]domain.EmailChallenge) []domain.EmailChallenge {
	result := make([]domain.EmailChallenge, 0, len(challenges))
	for _, challenge := range challenges {
		result = append(result, challenge)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})
	return result
}
