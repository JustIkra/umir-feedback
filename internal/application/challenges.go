package application

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

const (
	challengeTTL         = 10 * time.Minute
	challengeRetention   = 24 * time.Hour
	resendCooldown       = 60 * time.Second
	windowLimit          = 10 * time.Minute
	verifyWindow         = time.Minute
	maxTargetIssues      = 3
	maxChallengeIssues   = 4
	maxBadAttempts       = 5
	maxIssueEvents       = 60
	maxVerifyEvents      = 300
	defaultMaxChallenges = 4096
)

var (
	ErrChallengeEntropy = errors.New("challenge entropy unavailable")
	ErrChallengeCrypto  = errors.New("challenge cryptography failed")
)

type completionBinding struct {
	challengeID  domain.ChallengeID
	registration domain.RegistrationID
	purpose      domain.ChallengePurpose
	proofDigest  []byte
}

type Challenges struct {
	mu            sync.Mutex
	repo          ChallengeRepository
	secrets       ChallengeSecrets
	clock         Clock
	maxChallenges int
	items         map[domain.ChallengeID]domain.EmailChallenge
	registrations map[domain.RegistrationID]domain.ChallengeID
	completions   map[domain.CompletionID]completionBinding
	issueEvents   []time.Time
	verifyEvents  []time.Time
	active        map[string]context.CancelFunc
	unavailable   bool
}

func NewChallenges(ctx context.Context, repo ChallengeRepository, secrets ChallengeSecrets, clock Clock, maxChallenges int) (*Challenges, error) {
	if ctx == nil || repo == nil || secrets == nil || clock == nil || maxChallenges < 0 || maxChallenges > defaultMaxChallenges {
		return nil, ErrInvalidChallengeRequest
	}
	if maxChallenges == 0 {
		maxChallenges = defaultMaxChallenges
	}
	c := &Challenges{
		repo:          repo,
		secrets:       secrets,
		clock:         clock,
		maxChallenges: maxChallenges,
		items:         make(map[domain.ChallengeID]domain.EmailChallenge),
		registrations: make(map[domain.RegistrationID]domain.ChallengeID),
		completions:   make(map[domain.CompletionID]completionBinding),
		active:        make(map[string]context.CancelFunc),
	}
	stored, err := repo.Challenges(ctx)
	if err != nil {
		return nil, fmt.Errorf("load email challenges: %w", err)
	}
	if len(stored) > maxChallenges {
		return nil, ErrChallengeCapacity
	}
	now := moscowTime(clock.Now())
	for _, source := range stored {
		challenge := domain.CloneEmailChallenge(source)
		if err := domain.ValidateEmailChallenge(challenge); err != nil {
			return nil, ErrInvalidChallengeRequest
		}
		if _, exists := c.items[challenge.ID]; exists {
			return nil, ErrInvalidChallengeRequest
		}
		if _, exists := c.registrations[challenge.RegistrationID]; exists {
			return nil, ErrInvalidChallengeRequest
		}
		if err := c.validateRetainedSecrets(challenge); err != nil {
			return nil, err
		}
		changed := false
		if challenge.State == domain.ChallengePending && !now.Before(challenge.ExpiresAt) {
			c.expire(&challenge)
			markUnsentFailed(&challenge, now)
			changed = true
		}
		if challenge.State == domain.ChallengePending {
			for i := range challenge.Issues {
				job := &challenge.Issues[i].Job
				if job.State == domain.InFlight {
					job.StartedAt = time.Time{}
					if job.Attempts >= 3 {
						job.State = domain.Failed
						job.ConfirmedAt = now
					} else {
						job.State = domain.Pending
						job.NextAttemptAt = now
					}
					changed = true
				}
			}
		}
		if changed {
			if err := c.saveLocked(ctx, challenge); err != nil {
				return nil, err
			}
		}
		c.items[challenge.ID] = domain.CloneEmailChallenge(challenge)
		c.registrations[challenge.RegistrationID] = challenge.ID
		if challenge.State == domain.ChallengeCompleted {
			c.completions[challenge.CompletionID] = completionBinding{
				challengeID:  challenge.ID,
				registration: challenge.RegistrationID,
				purpose:      challenge.Purpose,
				proofDigest:  cloneChallengeBytes(challenge.ProofDigest),
			}
		}
	}
	return c, nil
}

func (c *Challenges) Begin(ctx context.Context, input ChallengeBegin) (domain.ChallengeReceipt, error) {
	email, ok := validBeginInput(input)
	if !ok {
		return domain.ChallengeReceipt{}, ErrInvalidChallengeRequest
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.readyLocked(); err != nil {
		return domain.ChallengeReceipt{}, err
	}
	now := moscowTime(c.clock.Now())
	if retry, limited := c.takeIssueSlot(now); limited {
		return domain.ChallengeReceipt{}, ChallengeRateLimited{RetryAfter: retry}
	}
	if id, exists := c.registrations[input.RegistrationID]; exists {
		previous := c.items[id]
		if previous.Email != email || previous.Purpose != input.Purpose {
			return domain.ChallengeReceipt{}, ErrChallengeIdentityConflict
		}
		return beginReceipt(previous), nil
	}
	if len(c.items) >= c.maxChallenges {
		if err := c.sweepLocked(ctx, now); err != nil {
			return domain.ChallengeReceipt{}, err
		}
		if len(c.items) >= c.maxChallenges {
			return domain.ChallengeReceipt{}, ChallengeRateLimited{RetryAfter: c.capacityRetryAfter(now)}
		}
	}
	if c.countTargetIssues(email, input.Purpose, now) >= maxTargetIssues {
		return domain.ChallengeReceipt{}, ChallengeRateLimited{RetryAfter: c.targetRetryAfter(email, input.Purpose, now)}
	}
	challengeID, err := c.newID()
	if err != nil {
		return domain.ChallengeReceipt{}, err
	}
	if !validUUIDv4(challengeID) {
		return domain.ChallengeReceipt{}, ErrChallengeEntropy
	}
	if _, exists := c.items[domain.ChallengeID(challengeID)]; exists {
		return domain.ChallengeReceipt{}, ErrChallengeEntropy
	}
	code, err := c.secrets.NewCode()
	if err != nil || !validCode(code) {
		return domain.ChallengeReceipt{}, ErrChallengeEntropy
	}
	codeDigest := c.secrets.Digest(codeScope(domain.ChallengeID(challengeID), input.Purpose), code)
	sealedCode, err := c.secrets.Seal(codeScope(domain.ChallengeID(challengeID), input.Purpose), code)
	if err != nil || len(codeDigest) != 32 || len(sealedCode.Nonce) != 12 || len(sealedCode.Ciphertext) != 22 {
		return domain.ChallengeReceipt{}, ErrChallengeCrypto
	}
	challenge := domain.EmailChallenge{
		ID: domain.ChallengeID(challengeID), RegistrationID: input.RegistrationID, Email: email, Purpose: input.Purpose,
		CreatedAt: now, ExpiresAt: now.Add(challengeTTL), ResendAvailableAt: now.Add(resendCooldown), RetainUntil: now.Add(challengeRetention),
		State: domain.ChallengePending, CodeDigest: cloneChallengeBytes(codeDigest), EncryptedCode: cloneChallengeSealed(sealedCode),
		Issues: []domain.CodeIssuance{{ID: domain.IssuanceID(challengeID), AcceptedAt: now, ResendAvailableAt: now.Add(resendCooldown), Job: domain.Job{Channel: domain.SMTP, State: domain.Pending, NextAttemptAt: now}}},
	}
	if err := domain.ValidateEmailChallenge(challenge); err != nil {
		return domain.ChallengeReceipt{}, ErrInvalidChallengeRequest
	}
	if err := c.saveLocked(ctx, challenge); err != nil {
		return domain.ChallengeReceipt{}, err
	}
	c.items[challenge.ID] = domain.CloneEmailChallenge(challenge)
	c.registrations[challenge.RegistrationID] = challenge.ID
	if err := c.pruneLocked(ctx, now); err != nil {
		return domain.ChallengeReceipt{}, err
	}
	return beginReceipt(challenge), nil
}

func (c *Challenges) Resend(ctx context.Context, input ChallengeResend) (domain.ChallengeReceipt, error) {
	if !validChallengeToken(string(input.ChallengeID), 128) || !validUUIDv4(string(input.RegistrationID)) || !validUUIDv4(string(input.IssuanceID)) || input.Purpose != domain.OperatorRegistration {
		return domain.ChallengeReceipt{}, ErrInvalidChallengeRequest
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.readyLocked(); err != nil {
		return domain.ChallengeReceipt{}, err
	}
	now := moscowTime(c.clock.Now())
	if retry, limited := c.takeIssueSlot(now); limited {
		return domain.ChallengeReceipt{}, ChallengeRateLimited{RetryAfter: retry}
	}
	challenge, ok := c.items[input.ChallengeID]
	if !ok || challenge.RegistrationID != input.RegistrationID || challenge.Purpose != input.Purpose {
		return domain.ChallengeReceipt{}, ErrChallengeUnavailable
	}
	challenge = domain.CloneEmailChallenge(challenge)
	for _, issue := range challenge.Issues {
		if issue.ID == input.IssuanceID {
			return issuanceReceipt(challenge, issue), nil
		}
	}
	if challenge.State != domain.ChallengePending || !now.Before(challenge.ExpiresAt) {
		if challenge.State == domain.ChallengePending {
			if err := c.expireAndSaveLocked(ctx, challenge, now); err != nil {
				return domain.ChallengeReceipt{}, err
			}
		}
		return domain.ChallengeReceipt{}, ErrChallengeUnavailable
	}
	if len(challenge.Issues) >= maxChallengeIssues {
		return domain.ChallengeReceipt{}, ChallengeRateLimited{RetryAfter: challenge.Issues[0].AcceptedAt.Add(windowLimit).Sub(now)}
	}
	if now.Before(challenge.ResendAvailableAt) {
		return domain.ChallengeReceipt{}, ChallengeRateLimited{RetryAfter: challenge.ResendAvailableAt.Sub(now)}
	}
	resends := 0
	oldest := time.Time{}
	for _, issue := range challenge.Issues[1:] {
		if now.Before(issue.AcceptedAt.Add(windowLimit)) {
			resends++
			if oldest.IsZero() || issue.AcceptedAt.Before(oldest) {
				oldest = issue.AcceptedAt
			}
		}
	}
	if resends >= 3 {
		return domain.ChallengeReceipt{}, ChallengeRateLimited{RetryAfter: oldest.Add(windowLimit).Sub(now)}
	}
	code, err := c.openCode(challenge)
	if err != nil {
		return domain.ChallengeReceipt{}, err
	}
	openedDigest := c.secrets.Digest(codeScope(challenge.ID, challenge.Purpose), code)
	if !c.secrets.Equal(openedDigest, challenge.CodeDigest) {
		return domain.ChallengeReceipt{}, ErrChallengeCrypto
	}
	challenge.Issues = append(challenge.Issues, domain.CodeIssuance{
		ID: input.IssuanceID, AcceptedAt: now, ResendAvailableAt: now.Add(resendCooldown),
		Job: domain.Job{Channel: domain.SMTP, State: domain.Pending, NextAttemptAt: now},
	})
	challenge.ResendAvailableAt = now.Add(resendCooldown)
	if err := c.persistAndPruneLocked(ctx, challenge, now); err != nil {
		return domain.ChallengeReceipt{}, err
	}
	return challengeReceipt(challenge), nil
}

func (c *Challenges) Verify(ctx context.Context, input ChallengeVerify) (domain.ChallengeProof, error) {
	if !validChallengeToken(string(input.ChallengeID), 128) || !validUUIDv4(string(input.RegistrationID)) || !validUUIDv4(string(input.VerificationID)) || input.Purpose != domain.OperatorRegistration || !validCode(input.Code) {
		return domain.ChallengeProof{}, ErrInvalidChallengeRequest
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.readyLocked(); err != nil {
		return domain.ChallengeProof{}, err
	}
	now := moscowTime(c.clock.Now())
	if retry, limited := c.takeVerifySlot(now); limited {
		return domain.ChallengeProof{}, ChallengeRateLimited{RetryAfter: retry}
	}
	challenge, ok := c.items[input.ChallengeID]
	if !ok || challenge.RegistrationID != input.RegistrationID || challenge.Purpose != input.Purpose {
		return domain.ChallengeProof{}, ErrChallengeUnavailable
	}
	challenge = domain.CloneEmailChallenge(challenge)
	attemptDigest := c.secrets.Digest(codeScope(challenge.ID, challenge.Purpose), input.Code)
	for _, previous := range challenge.Verifications {
		if previous.ID != input.VerificationID {
			continue
		}
		if !c.secrets.Equal(previous.Digest, attemptDigest) {
			return domain.ChallengeProof{}, ErrVerificationIdentityConflict
		}
		if !now.Before(challenge.RetainUntil) {
			return domain.ChallengeProof{}, ErrChallengeUnavailable
		}
		if previous.Verified {
			proof, err := c.openProof(challenge)
			if err != nil {
				return domain.ChallengeProof{}, err
			}
			return domain.ChallengeProof{ProofID: proof, VerifiedAt: challenge.VerifiedAt}, nil
		}
		return domain.ChallengeProof{}, ErrInvalidCode
	}
	if challenge.State != domain.ChallengePending {
		return domain.ChallengeProof{}, ErrChallengeUnavailable
	}
	if !now.Before(challenge.ExpiresAt) {
		if err := c.expireAndSaveLocked(ctx, challenge, now); err != nil {
			return domain.ChallengeProof{}, err
		}
		return domain.ChallengeProof{}, ErrChallengeUnavailable
	}
	if !c.secrets.Equal(attemptDigest, challenge.CodeDigest) {
		challenge.BadAttempts++
		challenge.Verifications = append(challenge.Verifications, domain.CodeVerification{ID: input.VerificationID, Digest: cloneChallengeBytes(attemptDigest)})
		if challenge.BadAttempts == maxBadAttempts {
			challenge.State = domain.ChallengeLocked
			challenge.EncryptedCode = domain.SealedValue{}
			c.cancelChallengeJobsLocked(challenge.ID)
			markUnsentFailed(&challenge, now)
		}
		if err := c.persistAndPruneLocked(ctx, challenge, now); err != nil {
			return domain.ChallengeProof{}, err
		}
		return domain.ChallengeProof{}, ErrInvalidCode
	}
	proofID, err := c.newID()
	if err != nil || !validUUIDv4(proofID) {
		return domain.ChallengeProof{}, ErrChallengeEntropy
	}
	proofDigest := c.secrets.Digest(proofScope(challenge.ID, challenge.Purpose), proofID)
	sealedProof, err := c.secrets.Seal(proofScope(challenge.ID, challenge.Purpose), proofID)
	if err != nil || len(proofDigest) != 32 || len(sealedProof.Nonce) != 12 || len(sealedProof.Ciphertext) != 52 {
		return domain.ChallengeProof{}, ErrChallengeCrypto
	}
	challenge.State = domain.ChallengeVerified
	challenge.Verifications = append(challenge.Verifications, domain.CodeVerification{ID: input.VerificationID, Digest: cloneChallengeBytes(attemptDigest), Verified: true})
	challenge.EncryptedCode = domain.SealedValue{}
	challenge.VerifiedAt = now
	challenge.RetainUntil = now.Add(challengeRetention)
	challenge.ProofDigest = cloneChallengeBytes(proofDigest)
	challenge.EncryptedProof = cloneChallengeSealed(sealedProof)
	markUnsentFailed(&challenge, now)
	c.cancelChallengeJobsLocked(challenge.ID)
	if err := c.persistAndPruneLocked(ctx, challenge, now); err != nil {
		return domain.ChallengeProof{}, err
	}
	return domain.ChallengeProof{ProofID: proofID, VerifiedAt: now}, nil
}

func (c *Challenges) Complete(ctx context.Context, input ChallengeComplete) (domain.ChallengeCompletion, error) {
	if !validChallengeToken(string(input.ChallengeID), 128) || !validUUIDv4(string(input.RegistrationID)) || !validUUIDv4(string(input.CompletionID)) || input.Purpose != domain.OperatorRegistration || !validChallengeToken(input.ProofID, 128) {
		return domain.ChallengeCompletion{}, ErrInvalidChallengeRequest
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.readyLocked(); err != nil {
		return domain.ChallengeCompletion{}, err
	}
	now := moscowTime(c.clock.Now())
	proofDigest := c.secrets.Digest(proofScope(input.ChallengeID, input.Purpose), input.ProofID)
	if previous, exists := c.completions[input.CompletionID]; exists {
		if previous.challengeID != input.ChallengeID || previous.registration != input.RegistrationID || previous.purpose != input.Purpose || !c.secrets.Equal(previous.proofDigest, proofDigest) {
			return domain.ChallengeCompletion{}, ErrCompletionIdentityConflict
		}
	}
	challenge, ok := c.items[input.ChallengeID]
	if !ok || challenge.RegistrationID != input.RegistrationID || challenge.Purpose != input.Purpose {
		return domain.ChallengeCompletion{}, ErrChallengeUnavailable
	}
	challenge = domain.CloneEmailChallenge(challenge)
	if !c.secrets.Equal(proofDigest, challenge.ProofDigest) {
		return domain.ChallengeCompletion{}, ErrChallengeUnavailable
	}
	if _, exists := c.completions[input.CompletionID]; exists && challenge.CompletionID == input.CompletionID && challenge.State == domain.ChallengeCompleted {
		if !now.Before(challenge.RetainUntil) {
			return domain.ChallengeCompletion{}, ErrChallengeUnavailable
		}
		return domain.ChallengeCompletion{CompletedAt: challenge.CompletedAt}, nil
	}
	if challenge.State == domain.ChallengeCompleted {
		return domain.ChallengeCompletion{}, ErrCompletionIdentityConflict
	}
	if challenge.State != domain.ChallengeVerified || !now.Before(challenge.RetainUntil) {
		return domain.ChallengeCompletion{}, ErrChallengeUnavailable
	}
	challenge.State = domain.ChallengeCompleted
	challenge.CompletionID = input.CompletionID
	challenge.CompletedAt = now
	if err := c.saveLocked(ctx, challenge); err != nil {
		return domain.ChallengeCompletion{}, err
	}
	c.items[challenge.ID] = domain.CloneEmailChallenge(challenge)
	c.completions[input.CompletionID] = completionBinding{
		challengeID: challenge.ID, registration: challenge.RegistrationID, purpose: challenge.Purpose,
		proofDigest: cloneChallengeBytes(challenge.ProofDigest),
	}
	if err := c.pruneLocked(ctx, now); err != nil {
		return domain.ChallengeCompletion{}, err
	}
	return domain.ChallengeCompletion{CompletedAt: now}, nil
}

func (c *Challenges) Sweep(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.readyLocked(); err != nil {
		return err
	}
	return c.sweepLocked(ctx, moscowTime(c.clock.Now()))
}

func (c *Challenges) sweepLocked(ctx context.Context, now time.Time) error {
	ids := make([]domain.ChallengeID, 0, len(c.items))
	for id := range c.items {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		challenge := domain.CloneEmailChallenge(c.items[id])
		if challenge.State == domain.ChallengePending && !now.Before(challenge.ExpiresAt) {
			c.expire(&challenge)
			markUnsentFailed(&challenge, now)
			c.cancelChallengeJobsLocked(id)
			if err := c.saveLocked(ctx, challenge); err != nil {
				return err
			}
			c.items[id] = domain.CloneEmailChallenge(challenge)
		}
	}
	if err := c.pruneLocked(ctx, now); err != nil {
		return err
	}
	for id, challenge := range c.items {
		if !now.Before(challenge.RetainUntil) {
			delete(c.items, id)
			delete(c.registrations, challenge.RegistrationID)
			delete(c.completions, challenge.CompletionID)
		}
	}
	return nil
}

func (c *Challenges) capacityRetryAfter(now time.Time) time.Duration {
	var earliest time.Time
	for _, challenge := range c.items {
		if earliest.IsZero() || challenge.RetainUntil.Before(earliest) {
			earliest = challenge.RetainUntil
		}
	}
	if earliest.IsZero() || !now.Before(earliest) {
		return time.Second
	}
	return earliest.Sub(now)
}

func (c *Challenges) validateRetainedSecrets(challenge domain.EmailChallenge) error {
	if challenge.State == domain.ChallengePending {
		code, err := c.openCode(challenge)
		if err != nil {
			return err
		}
		digest := c.secrets.Digest(codeScope(challenge.ID, challenge.Purpose), code)
		if !validCode(code) || !c.secrets.Equal(digest, challenge.CodeDigest) {
			return ErrChallengeCrypto
		}
	}
	if challenge.State == domain.ChallengeVerified || challenge.State == domain.ChallengeCompleted {
		proof, err := c.openProof(challenge)
		if err != nil {
			return err
		}
		digest := c.secrets.Digest(proofScope(challenge.ID, challenge.Purpose), proof)
		if !c.secrets.Equal(digest, challenge.ProofDigest) {
			return ErrChallengeCrypto
		}
	}
	return nil
}

func (c *Challenges) openCode(challenge domain.EmailChallenge) (string, error) {
	value, err := c.secrets.Open(codeScope(challenge.ID, challenge.Purpose), challenge.EncryptedCode)
	if err != nil {
		return "", ErrChallengeCrypto
	}
	return value, nil
}

func (c *Challenges) openProof(challenge domain.EmailChallenge) (string, error) {
	value, err := c.secrets.Open(proofScope(challenge.ID, challenge.Purpose), challenge.EncryptedProof)
	if err != nil {
		return "", ErrChallengeCrypto
	}
	return value, nil
}

func (c *Challenges) Ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.unavailable
}

func (c *Challenges) readyLocked() error {
	if c.unavailable {
		return ErrStorageOutcomeUnknown
	}
	return nil
}

func (c *Challenges) saveLocked(ctx context.Context, challenge domain.EmailChallenge) error {
	if err := domain.ValidateEmailChallenge(challenge); err != nil {
		return ErrInvalidChallengeRequest
	}
	if err := c.repo.SaveChallenge(ctx, domain.CloneEmailChallenge(challenge)); err != nil {
		if errors.Is(err, ErrStorageOutcomeUnknown) {
			c.unavailable = true
		}
		return err
	}
	return nil
}

func (c *Challenges) persistAndPruneLocked(ctx context.Context, challenge domain.EmailChallenge, now time.Time) error {
	if err := c.saveLocked(ctx, challenge); err != nil {
		return err
	}
	c.items[challenge.ID] = domain.CloneEmailChallenge(challenge)
	if err := c.pruneLocked(ctx, now); err != nil {
		if errors.Is(err, ErrStorageOutcomeUnknown) {
			c.unavailable = true
		}
		return err
	}
	return nil
}

func (c *Challenges) pruneLocked(ctx context.Context, now time.Time) error {
	if err := c.repo.PruneChallenges(context.WithoutCancel(ctx), now); err != nil {
		if errors.Is(err, ErrStorageOutcomeUnknown) {
			c.unavailable = true
		}
		return err
	}
	return nil
}

func (c *Challenges) expireAndSaveLocked(ctx context.Context, challenge domain.EmailChallenge, now time.Time) error {
	challenge = domain.CloneEmailChallenge(challenge)
	c.expire(&challenge)
	c.cancelChallengeJobsLocked(challenge.ID)
	markUnsentFailed(&challenge, now)
	if err := c.persistAndPruneLocked(ctx, challenge, now); err != nil {
		return err
	}
	return nil
}

func (c *Challenges) expire(challenge *domain.EmailChallenge) {
	challenge.State = domain.ChallengeExpired
	challenge.EncryptedCode = domain.SealedValue{}
	for i := range challenge.Issues {
		if challenge.Issues[i].Job.State == domain.Pending || challenge.Issues[i].Job.State == domain.InFlight {
			challenge.Issues[i].Job.State = domain.Failed
		}
	}
}

func markUnsentFailed(challenge *domain.EmailChallenge, now time.Time) {
	for i := range challenge.Issues {
		job := &challenge.Issues[i].Job
		if job.State == domain.Pending || job.State == domain.InFlight {
			job.State = domain.Failed
			job.ConfirmedAt = now
			job.StartedAt = time.Time{}
		}
	}
}

func (c *Challenges) cancelChallengeJobsLocked(id domain.ChallengeID) {
	prefix := string(id) + ":"
	for key, cancel := range c.active {
		if strings.HasPrefix(key, prefix) {
			cancel()
			delete(c.active, key)
		}
	}
}

func (c *Challenges) takeIssueSlot(now time.Time) (time.Duration, bool) {
	c.issueEvents = trimEvents(c.issueEvents, now, windowLimit)
	if len(c.issueEvents) >= maxIssueEvents {
		return c.issueEvents[0].Add(windowLimit).Sub(now), true
	}
	c.issueEvents = append(c.issueEvents, now)
	return 0, false
}

func (c *Challenges) takeVerifySlot(now time.Time) (time.Duration, bool) {
	c.verifyEvents = trimEvents(c.verifyEvents, now, verifyWindow)
	if len(c.verifyEvents) >= maxVerifyEvents {
		return c.verifyEvents[0].Add(verifyWindow).Sub(now), true
	}
	c.verifyEvents = append(c.verifyEvents, now)
	return 0, false
}

func trimEvents(events []time.Time, now time.Time, window time.Duration) []time.Time {
	first := 0
	for first < len(events) && !now.Before(events[first].Add(window)) {
		first++
	}
	if first == 0 {
		return events
	}
	return append([]time.Time(nil), events[first:]...)
}

func (c *Challenges) countTargetIssues(email string, purpose domain.ChallengePurpose, now time.Time) int {
	count := 0
	for _, challenge := range c.items {
		if challenge.Email == email && challenge.Purpose == purpose && now.Before(challenge.CreatedAt.Add(windowLimit)) {
			count++
		}
	}
	return count
}

func (c *Challenges) targetRetryAfter(email string, purpose domain.ChallengePurpose, now time.Time) time.Duration {
	oldest := time.Time{}
	for _, challenge := range c.items {
		if challenge.Email == email && challenge.Purpose == purpose && now.Before(challenge.CreatedAt.Add(windowLimit)) && (oldest.IsZero() || challenge.CreatedAt.Before(oldest)) {
			oldest = challenge.CreatedAt
		}
	}
	return oldest.Add(windowLimit).Sub(now)
}

func (c *Challenges) newID() (string, error) {
	id, err := c.secrets.NewID()
	if err != nil || !validChallengeToken(id, 128) {
		return "", ErrChallengeEntropy
	}
	return id, nil
}

func validBeginInput(input ChallengeBegin) (string, bool) {
	if !validUUIDv4(string(input.RegistrationID)) || input.Purpose != domain.OperatorRegistration {
		return "", false
	}
	email, err := domain.NormalizeChallengeEmail(input.Email)
	return email, err == nil
}

func validChallengeToken(value string, max int) bool {
	if value == "" || len(value) > max {
		return false
	}
	for _, r := range value {
		if r <= 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func validUUIDv4(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' || value[14] != '4' || !strings.ContainsRune("89ab", rune(value[19])) {
		return false
	}
	for i, r := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func validCode(code string) bool {
	if len(code) != 6 {
		return false
	}
	for i := range code {
		if code[i] < '0' || code[i] > '9' {
			return false
		}
	}
	return true
}

func challengeReceipt(challenge domain.EmailChallenge) domain.ChallengeReceipt {
	return domain.ChallengeReceipt{ChallengeID: challenge.ID, ExpiresAt: challenge.ExpiresAt, ResendAvailableAt: challenge.ResendAvailableAt}
}

func beginReceipt(challenge domain.EmailChallenge) domain.ChallengeReceipt {
	return domain.ChallengeReceipt{ChallengeID: challenge.ID, ExpiresAt: challenge.ExpiresAt, ResendAvailableAt: challenge.CreatedAt.Add(resendCooldown)}
}

func issuanceReceipt(challenge domain.EmailChallenge, issue domain.CodeIssuance) domain.ChallengeReceipt {
	return domain.ChallengeReceipt{ChallengeID: challenge.ID, ExpiresAt: challenge.ExpiresAt, ResendAvailableAt: issue.ResendAvailableAt}
}

func moscowTime(value time.Time) time.Time {
	return value.In(time.FixedZone("Europe/Moscow", 3*60*60))
}

func codeScope(id domain.ChallengeID, purpose domain.ChallengePurpose) string {
	return string(id) + ":" + string(purpose) + ":code"
}

func proofScope(id domain.ChallengeID, purpose domain.ChallengePurpose) string {
	return string(id) + ":" + string(purpose) + ":proof"
}

func cloneChallengeBytes(value []byte) []byte {
	if value == nil {
		return nil
	}
	return append([]byte(nil), value...)
}

func cloneChallengeSealed(value domain.SealedValue) domain.SealedValue {
	return domain.SealedValue{Nonce: cloneChallengeBytes(value.Nonce), Ciphertext: cloneChallengeBytes(value.Ciphertext)}
}
