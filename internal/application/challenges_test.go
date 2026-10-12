package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

type challengeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *challengeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *challengeClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type challengeRepo struct {
	mu       sync.Mutex
	items    map[domain.ChallengeID]domain.EmailChallenge
	saveErr  error
	pruneErr error
}

func newChallengeRepo() *challengeRepo {
	return &challengeRepo{items: make(map[domain.ChallengeID]domain.EmailChallenge)}
}

func (r *challengeRepo) Challenges(context.Context) ([]domain.EmailChallenge, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]domain.EmailChallenge, 0, len(r.items))
	for _, c := range r.items {
		items = append(items, domain.CloneEmailChallenge(c))
	}
	return items, nil
}

func (r *challengeRepo) SaveChallenge(_ context.Context, c domain.EmailChallenge) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.saveErr != nil {
		return r.saveErr
	}
	r.items[c.ID] = domain.CloneEmailChallenge(c)
	return nil
}

func (r *challengeRepo) PruneChallenges(context.Context, time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pruneErr
}

func (r *challengeRepo) setSaveError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.saveErr = err
}

func (r *challengeRepo) only() domain.EmailChallenge {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.items {
		return domain.CloneEmailChallenge(c)
	}
	return domain.EmailChallenge{}
}

type challengeSecrets struct {
	mu     sync.Mutex
	id     int
	code   string
	sealed map[string]string
}

func newChallengeSecrets() *challengeSecrets {
	return &challengeSecrets{code: "004281", sealed: make(map[string]string)}
}

func (s *challengeSecrets) NewID() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.id++
	return testUUID(fmt.Sprintf("generated-%d", s.id)), nil
}

func (s *challengeSecrets) NewCode() (string, error) { return s.code, nil }

func (s *challengeSecrets) Digest(purpose, value string) []byte {
	sum := sha256.Sum256([]byte(purpose + ":" + value))
	return sum[:]
}

func (s *challengeSecrets) Equal(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

func (s *challengeSecrets) Seal(id, value string) (domain.SealedValue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := fmt.Sprintf("%012d", len(s.sealed)+1)
	s.sealed[id+":"+key] = value
	return domain.SealedValue{Nonce: []byte(key), Ciphertext: make([]byte, len(value)+16)}, nil
}

func (s *challengeSecrets) Open(id string, v domain.SealedValue) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.sealed[id+":"+string(v.Nonce)]
	if !ok {
		return "", errors.New("sealed value unavailable")
	}
	return value, nil
}

type captureCodeSender struct {
	mu      sync.Mutex
	calls   int
	results []domain.DeliveryResult
	started chan struct{}
	release chan struct{}
}

func (s *captureCodeSender) SendOTP(ctx context.Context, mail domain.CodeMail) domain.DeliveryResult {
	s.mu.Lock()
	s.calls++
	index := s.calls - 1
	var result domain.DeliveryResult
	if index < len(s.results) {
		result = s.results[index]
	}
	started, release := s.started, s.release
	s.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return domain.DeliveryResult{Code: "canceled"}
		}
	}
	if mail.Code == "" || mail.Email == "" {
		return domain.DeliveryResult{Permanent: true, Code: "invalid mail"}
	}
	return result
}

func testUUID(value string) string {
	sum := sha256.Sum256([]byte(value))
	encoded := hex.EncodeToString(sum[:16])
	return encoded[:8] + "-" + encoded[8:12] + "-4" + encoded[13:16] + "-8" + encoded[17:20] + "-" + encoded[20:32]
}

func validBegin(id string) ChallengeBegin {
	return ChallengeBegin{RegistrationID: domain.RegistrationID(testUUID(id)), Email: " Operator@Example.COM ", Purpose: domain.OperatorRegistration}
}

func newTestChallenges(t *testing.T, repo *challengeRepo, clock *challengeClock, secrets *challengeSecrets, max int) *Challenges {
	t.Helper()
	c, err := NewChallenges(context.Background(), repo, secrets, clock, max)
	if err != nil {
		t.Fatalf("NewChallenges() error = %v", err)
	}
	return c
}

func TestBeginExactReplayAndBindingConflict(t *testing.T) {
	clock := &challengeClock{now: time.Date(2026, 10, 12, 12, 0, 0, 0, time.FixedZone("MSK", 3*60*60))}
	repo, secrets := newChallengeRepo(), newChallengeSecrets()
	c := newTestChallenges(t, repo, clock, secrets, 4096)
	first, err := c.Begin(context.Background(), validBegin("reg-a"))
	if err != nil {
		t.Fatal(err)
	}
	clock.Add(2 * time.Minute)
	replay, err := c.Begin(context.Background(), validBegin("reg-a"))
	if err != nil {
		t.Fatal(err)
	}
	if replay != first {
		t.Fatalf("replay = %#v, want original %#v", replay, first)
	}
	conflict := validBegin("reg-a")
	conflict.Email = "other@example.com"
	if _, err := c.Begin(context.Background(), conflict); !errors.Is(err, ErrChallengeIdentityConflict) {
		t.Fatalf("conflict error = %v", err)
	}
	stored := repo.only()
	if got := stored.Email; got != "Operator@example.com" {
		t.Fatalf("stored normalized email = %q", got)
	}
	if len(stored.Issues) != 1 {
		t.Fatalf("issues = %d, want one original issuance", len(stored.Issues))
	}
}

func TestBeginTargetAndProcessCapsAtExactBoundary(t *testing.T) {
	clock := &challengeClock{now: time.Date(2026, 10, 12, 12, 0, 0, 0, time.FixedZone("MSK", 3*60*60))}
	repo, secrets := newChallengeRepo(), newChallengeSecrets()
	c := newTestChallenges(t, repo, clock, secrets, 10)
	for i := 0; i < 3; i++ {
		in := validBegin(fmt.Sprintf("reg-%d", i))
		if _, err := c.Begin(context.Background(), in); err != nil {
			t.Fatalf("begin %d: %v", i, err)
		}
		clock.Add(10 * time.Minute)
	}
	in := validBegin("reg-four")
	if _, err := c.Begin(context.Background(), in); err != nil {
		t.Fatalf("target window boundary should permit issuance: %v", err)
	}
	for i := 0; i < 59; i++ {
		_, err := c.Begin(context.Background(), validBegin(fmt.Sprintf("other-%d", i)))
		if err != nil {
			var limited ChallengeRateLimited
			if !errors.As(err, &limited) {
				t.Fatalf("begin for process cap = %v", err)
			}
		}
	}
	if _, err := c.Begin(context.Background(), validBegin("process-cap")); err == nil {
		t.Fatal("expected process issue cap")
	} else {
		var limited ChallengeRateLimited
		if !errors.As(err, &limited) || limited.RetryAfter <= 0 {
			t.Fatalf("cap error = %v, want ChallengeRateLimited", err)
		}
	}
}

func TestVerifyWrongAttemptLimitReplayAndCompleteBinding(t *testing.T) {
	ctx := context.Background()
	clock := &challengeClock{now: time.Date(2026, 10, 12, 12, 0, 0, 0, time.FixedZone("MSK", 3*60*60))}
	repo, secrets := newChallengeRepo(), newChallengeSecrets()
	c := newTestChallenges(t, repo, clock, secrets, 4096)
	receipt, err := c.Begin(ctx, validBegin("reg-verify"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 5; i++ {
		attempt := ChallengeVerify{ChallengeID: receipt.ChallengeID, RegistrationID: domain.RegistrationID(testUUID("reg-verify")), Purpose: domain.OperatorRegistration, VerificationID: domain.VerificationID(testUUID(fmt.Sprintf("v%d", i))), Code: "999999"}
		if _, err := c.Verify(ctx, attempt); !errors.Is(err, ErrInvalidCode) {
			t.Fatalf("wrong attempt %d error = %v", i, err)
		}
		if i == 1 {
			if _, err := c.Verify(ctx, attempt); !errors.Is(err, ErrInvalidCode) {
				t.Fatalf("exact wrong replay error = %v", err)
			}
		}
	}
	locked := repo.only()
	if locked.State != domain.ChallengeLocked || locked.BadAttempts != 5 || len(locked.Verifications) != 5 {
		t.Fatalf("locked state = %q bad=%d records=%d", locked.State, locked.BadAttempts, len(locked.Verifications))
	}
}

func TestVerifyProofReplayAndCompletionIdentityBinding(t *testing.T) {
	ctx := context.Background()
	clock := &challengeClock{now: time.Date(2026, 10, 12, 12, 0, 0, 0, time.FixedZone("MSK", 3*60*60))}
	repo, secrets := newChallengeRepo(), newChallengeSecrets()
	c := newTestChallenges(t, repo, clock, secrets, 4096)
	receipt, err := c.Begin(ctx, validBegin("reg-proof"))
	if err != nil {
		t.Fatal(err)
	}
	attempt := ChallengeVerify{ChallengeID: receipt.ChallengeID, RegistrationID: domain.RegistrationID(testUUID("reg-proof")), Purpose: domain.OperatorRegistration, VerificationID: domain.VerificationID(testUUID("v-good")), Code: "004281"}
	proof, err := c.Verify(ctx, attempt)
	if err != nil {
		t.Fatal(err)
	}
	clock.Add(11 * time.Minute)
	replay, err := c.Verify(ctx, attempt)
	if err != nil || replay != proof {
		t.Fatalf("proof replay = %#v, %v; want %#v", replay, err, proof)
	}
	completion := ChallengeComplete{ChallengeID: receipt.ChallengeID, RegistrationID: domain.RegistrationID(testUUID("reg-proof")), Purpose: domain.OperatorRegistration, CompletionID: domain.CompletionID(testUUID("complete-1")), ProofID: proof.ProofID}
	completed, err := c.Complete(ctx, completion)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := c.Complete(ctx, completion)
	if err != nil || replayed != completed {
		t.Fatalf("complete replay = %#v, %v", replayed, err)
	}
	completion.ProofID = testUUID("different-proof")
	if _, err := c.Complete(ctx, completion); !errors.Is(err, ErrCompletionIdentityConflict) {
		t.Fatalf("completion conflict error = %v", err)
	}
}

func TestVerifyExpiryAtExactBoundaryAndStorageFailureDoesNotMutate(t *testing.T) {
	ctx := context.Background()
	clock := &challengeClock{now: time.Date(2026, 10, 12, 12, 0, 0, 0, time.FixedZone("MSK", 3*60*60))}
	repo, secrets := newChallengeRepo(), newChallengeSecrets()
	c := newTestChallenges(t, repo, clock, secrets, 4096)
	receipt, err := c.Begin(ctx, validBegin("reg-expiry"))
	if err != nil {
		t.Fatal(err)
	}
	clock.Add(10 * time.Minute)
	attempt := ChallengeVerify{ChallengeID: receipt.ChallengeID, RegistrationID: domain.RegistrationID(testUUID("reg-expiry")), Purpose: domain.OperatorRegistration, VerificationID: domain.VerificationID(testUUID("v-expired")), Code: "004281"}
	if _, err := c.Verify(ctx, attempt); !errors.Is(err, ErrChallengeUnavailable) {
		t.Fatalf("verify at expiresAt error = %v", err)
	}
	stored := repo.only()
	if stored.State != domain.ChallengeExpired || len(stored.EncryptedCode.Ciphertext) != 0 {
		t.Fatalf("expiry not durably scrubbed: state=%q ciphertext=%d", stored.State, len(stored.EncryptedCode.Ciphertext))
	}

	repo2, secrets2 := newChallengeRepo(), newChallengeSecrets()
	c2 := newTestChallenges(t, repo2, clock, secrets2, 4096)
	repo2.setSaveError(ErrStorageOutcomeUnknown)
	if _, err := c2.Begin(ctx, validBegin("reg-unknown")); !errors.Is(err, ErrStorageOutcomeUnknown) {
		t.Fatalf("begin storage error = %v", err)
	}
	if len(repo2.items) != 0 {
		t.Fatal("failed begin mutated durable repository")
	}
	if _, err := c2.Begin(ctx, validBegin("reg-after-unknown")); !errors.Is(err, ErrStorageOutcomeUnknown) {
		t.Fatalf("service did not fail closed after unknown outcome: %v", err)
	}
}

func TestRestoreRetriesInflightAndCapacityDoesNotEvict(t *testing.T) {
	ctx := context.Background()
	clock := &challengeClock{now: time.Date(2026, 10, 12, 12, 0, 0, 0, time.FixedZone("MSK", 3*60*60))}
	repo, secrets := newChallengeRepo(), newChallengeSecrets()
	first := newTestChallenges(t, repo, clock, secrets, 1)
	receipt, err := first.Begin(ctx, validBegin("reg-restore"))
	if err != nil {
		t.Fatal(err)
	}
	stored := repo.only()
	stored.Issues[0].Job.State = domain.InFlight
	stored.Issues[0].Job.Attempts = 1
	if err := repo.SaveChallenge(ctx, stored); err != nil {
		t.Fatal(err)
	}
	restored := newTestChallenges(t, repo, clock, secrets, 1)
	if _, err := restored.Begin(ctx, validBegin("reg-restore")); err != nil {
		t.Fatalf("replayed begin after restore: %v", err)
	}
	if _, err := restored.Begin(ctx, validBegin("reg-new")); err == nil {
		t.Fatal("capacity accepted before retention window")
	} else {
		var limited ChallengeRateLimited
		if !errors.As(err, &limited) || limited.RetryAfter != 24*time.Hour {
			t.Fatalf("capacity error = %v, retry %v", err, limited.RetryAfter)
		}
	}
	job := repo.only().Issues[0].Job
	if job.State != domain.Pending || job.Attempts != 1 || job.NextAttemptAt.After(clock.Now()) {
		t.Fatalf("restored job = %#v", job)
	}
	clock.Add(24 * time.Hour)
	if _, err := restored.Begin(ctx, validBegin("reg-new")); err != nil {
		t.Fatalf("capacity not released at retention boundary: %v", err)
	}
	if len(restored.items) != 1 {
		t.Fatalf("retained state count = %d, want one", len(restored.items))
	}
	_ = receipt
}

func TestProcessOneRetriesAtMostThreeTimesAndStopsAtExpiry(t *testing.T) {
	ctx := context.Background()
	clock := &challengeClock{now: time.Date(2026, 10, 12, 12, 0, 0, 0, time.FixedZone("MSK", 3*60*60))}
	repo, secrets := newChallengeRepo(), newChallengeSecrets()
	c := newTestChallenges(t, repo, clock, secrets, 4096)
	if _, err := c.Begin(ctx, validBegin("reg-worker")); err != nil {
		t.Fatal(err)
	}
	sender := &captureCodeSender{results: []domain.DeliveryResult{{Code: "temporary"}, {Code: "temporary"}, {Code: "temporary"}}}
	for i := 0; i < 3; i++ {
		worked, err := c.ProcessOne(ctx, sender)
		if err != nil || !worked {
			t.Fatalf("process %d worked=%v err=%v", i, worked, err)
		}
		clock.Add(time.Minute)
	}
	if worked, err := c.ProcessOne(ctx, sender); err != nil || worked {
		t.Fatalf("fourth process worked=%v err=%v", worked, err)
	}
	stored := repo.only()
	if stored.Issues[0].Job.State != domain.Failed || stored.Issues[0].Job.Attempts != 3 {
		t.Fatalf("final job = %#v", stored.Issues[0].Job)
	}
}

func TestProcessOneFinishMergesAfterConcurrentVerify(t *testing.T) {
	ctx := context.Background()
	clock := &challengeClock{now: time.Date(2026, 10, 12, 12, 0, 0, 0, time.FixedZone("MSK", 3*60*60))}
	repo, secrets := newChallengeRepo(), newChallengeSecrets()
	c := newTestChallenges(t, repo, clock, secrets, 4096)
	receipt, err := c.Begin(ctx, validBegin("reg-race"))
	if err != nil {
		t.Fatal(err)
	}
	sender := &captureCodeSender{started: make(chan struct{}, 1), release: make(chan struct{}), results: []domain.DeliveryResult{{Success: true}}}
	workerDone := make(chan error, 1)
	go func() {
		_, err := c.ProcessOne(ctx, sender)
		workerDone <- err
	}()
	<-sender.started
	proof, err := c.Verify(ctx, ChallengeVerify{ChallengeID: receipt.ChallengeID, RegistrationID: domain.RegistrationID(testUUID("reg-race")), Purpose: domain.OperatorRegistration, VerificationID: domain.VerificationID(testUUID("v-race")), Code: "004281"})
	if err != nil {
		t.Fatal(err)
	}
	close(sender.release)
	if err := <-workerDone; err != nil {
		t.Fatal(err)
	}
	stored := repo.only()
	if len(stored.EncryptedCode.Ciphertext) != 0 || stored.State != domain.ChallengeVerified || proof.ProofID == "" {
		t.Fatalf("worker restored scrubbed state: %#v", stored)
	}
}

func TestTargetLimitAllowsFourthOnlyAtSlidingWindowBoundary(t *testing.T) {
	clock := &challengeClock{now: time.Date(2026, 10, 12, 12, 0, 0, 0, time.FixedZone("MSK", 3*60*60))}
	repo, secrets := newChallengeRepo(), newChallengeSecrets()
	c := newTestChallenges(t, repo, clock, secrets, 10)
	for i := 0; i < 3; i++ {
		if _, err := c.Begin(context.Background(), validBegin(fmt.Sprintf("target-%d", i))); err != nil {
			t.Fatalf("begin %d: %v", i, err)
		}
	}
	if _, err := c.Begin(context.Background(), validBegin("target-blocked")); err == nil {
		t.Fatal("expected target rate limit")
	} else {
		var limited ChallengeRateLimited
		if !errors.As(err, &limited) || limited.RetryAfter != 10*time.Minute {
			t.Fatalf("target limit = %v, retry %v", err, limited.RetryAfter)
		}
	}
	clock.Add(10 * time.Minute)
	if _, err := c.Begin(context.Background(), validBegin("target-boundary")); err != nil {
		t.Fatalf("begin at exact target window boundary: %v", err)
	}
}

func TestResendCooldownLimitAndReplay(t *testing.T) {
	ctx := context.Background()
	clock := &challengeClock{now: time.Date(2026, 10, 12, 12, 0, 0, 0, time.FixedZone("MSK", 3*60*60))}
	repo, secrets := newChallengeRepo(), newChallengeSecrets()
	c := newTestChallenges(t, repo, clock, secrets, 4096)
	registration := domain.RegistrationID(testUUID("resend-registration"))
	receipt, err := c.Begin(ctx, ChallengeBegin{RegistrationID: registration, Email: "a@example.com", Purpose: domain.OperatorRegistration})
	if err != nil {
		t.Fatal(err)
	}
	resend := func(name string) (domain.ChallengeReceipt, error) {
		return c.Resend(ctx, ChallengeResend{ChallengeID: receipt.ChallengeID, RegistrationID: registration, Purpose: domain.OperatorRegistration, IssuanceID: domain.IssuanceID(testUUID(name))})
	}
	if _, err := resend("resend-1"); err == nil {
		t.Fatal("resend before cooldown succeeded")
	} else {
		var limited ChallengeRateLimited
		if !errors.As(err, &limited) || limited.RetryAfter != time.Minute {
			t.Fatalf("cooldown error = %v", err)
		}
	}
	clock.Add(time.Minute)
	first, err := resend("resend-1")
	if err != nil {
		t.Fatalf("resend at exact cooldown boundary: %v", err)
	}
	if first.ExpiresAt != receipt.ExpiresAt || first.ChallengeID != receipt.ChallengeID {
		t.Fatalf("resend changed challenge receipt: %#v", first)
	}
	if replay, err := resend("resend-1"); err != nil || replay != first {
		t.Fatalf("resend replay = %#v, %v", replay, err)
	}
	clock.Add(time.Minute)
	if _, err := resend("resend-2"); err != nil {
		t.Fatalf("second resend: %v", err)
	}
	if replay, err := resend("resend-1"); err != nil || replay != first {
		t.Fatalf("older resend replay changed receipt = %#v, %v", replay, err)
	}
	clock.Add(time.Minute)
	if _, err := resend("resend-3"); err != nil {
		t.Fatalf("third resend: %v", err)
	}
	clock.Add(time.Minute)
	if _, err := resend("resend-4"); err == nil {
		t.Fatal("fourth resend succeeded")
	} else {
		var limited ChallengeRateLimited
		if !errors.As(err, &limited) || limited.RetryAfter <= 0 {
			t.Fatalf("resend limit error = %v", err)
		}
	}
	stored := repo.only()
	if len(stored.Issues) != 4 || stored.Issues[0].Job.State != domain.Pending {
		t.Fatalf("issue snapshot = %d, first job %q", len(stored.Issues), stored.Issues[0].Job.State)
	}
	code, err := secrets.Open(string(stored.ID)+":"+string(stored.Purpose)+":code", stored.EncryptedCode)
	if err != nil || code != "004281" {
		t.Fatalf("resend did not preserve original encrypted code: %q, %v", code, err)
	}
}

func TestVerifyProcessLimitIsBeforeUnknownChallengeLookup(t *testing.T) {
	ctx := context.Background()
	clock := &challengeClock{now: time.Date(2026, 10, 12, 12, 0, 0, 0, time.FixedZone("MSK", 3*60*60))}
	c := newTestChallenges(t, newChallengeRepo(), clock, newChallengeSecrets(), 4096)
	for i := 0; i < 300; i++ {
		_, err := c.Verify(ctx, ChallengeVerify{ChallengeID: "unknown", RegistrationID: domain.RegistrationID(testUUID("unknown-registration")), Purpose: domain.OperatorRegistration, VerificationID: domain.VerificationID(testUUID(fmt.Sprintf("attempt-%d", i))), Code: "000000"})
		if !errors.Is(err, ErrChallengeUnavailable) {
			t.Fatalf("unknown verify %d = %v", i, err)
		}
	}
	_, err := c.Verify(ctx, ChallengeVerify{ChallengeID: "unknown", RegistrationID: domain.RegistrationID(testUUID("unknown-registration")), Purpose: domain.OperatorRegistration, VerificationID: domain.VerificationID(testUUID("attempt-cap")), Code: "000000"})
	var limited ChallengeRateLimited
	if !errors.As(err, &limited) || limited.RetryAfter != time.Minute {
		t.Fatalf("301st verify = %v, want one-minute process limit", err)
	}
	if len(c.items) != 0 {
		t.Fatalf("unknown challenge allocated state: %d", len(c.items))
	}
}

func TestConcurrentBeginAndVerifySerializeState(t *testing.T) {
	ctx := context.Background()
	clock := &challengeClock{now: time.Date(2026, 10, 12, 12, 0, 0, 0, time.FixedZone("MSK", 3*60*60))}
	repo, secrets := newChallengeRepo(), newChallengeSecrets()
	c := newTestChallenges(t, repo, clock, secrets, 4096)
	input := validBegin("concurrent-begin")
	var wg sync.WaitGroup
	receipts := make([]domain.ChallengeReceipt, 20)
	errs := make([]error, len(receipts))
	for i := range receipts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			receipts[i], errs[i] = c.Begin(ctx, input)
		}(i)
	}
	wg.Wait()
	for i := range receipts {
		if errs[i] != nil || receipts[i] != receipts[0] {
			t.Fatalf("concurrent begin %d = %#v, %v; first %#v", i, receipts[i], errs[i], receipts[0])
		}
	}
	if len(repo.only().Issues) != 1 {
		t.Fatal("concurrent begin created duplicate issuance")
	}

	wrong := make([]error, 10)
	for i := range wrong {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, wrong[i] = c.Verify(ctx, ChallengeVerify{ChallengeID: receipts[0].ChallengeID, RegistrationID: input.RegistrationID, Purpose: domain.OperatorRegistration, VerificationID: domain.VerificationID(testUUID(fmt.Sprintf("concurrent-verify-%d", i))), Code: "999999"})
		}(i)
	}
	wg.Wait()
	invalid, unavailable := 0, 0
	for _, err := range wrong {
		if errors.Is(err, ErrInvalidCode) {
			invalid++
		} else if errors.Is(err, ErrChallengeUnavailable) {
			unavailable++
		} else {
			t.Fatalf("unexpected concurrent verify error: %v", err)
		}
	}
	if invalid != 5 || unavailable != 5 {
		t.Fatalf("wrong result counts invalid=%d unavailable=%d", invalid, unavailable)
	}
	if stored := repo.only(); stored.BadAttempts != 5 || len(stored.Verifications) != 5 || stored.State != domain.ChallengeLocked {
		t.Fatalf("concurrent verification state = bad %d, records %d, state %q", stored.BadAttempts, len(stored.Verifications), stored.State)
	}
}

func TestCompletionIDConflictsAcrossChallengesAndRestartReplaysProof(t *testing.T) {
	ctx := context.Background()
	clock := &challengeClock{now: time.Date(2026, 10, 12, 12, 0, 0, 0, time.FixedZone("MSK", 3*60*60))}
	repo, secrets := newChallengeRepo(), newChallengeSecrets()
	c := newTestChallenges(t, repo, clock, secrets, 4096)
	beginA := validBegin("global-completion-a")
	beginB := validBegin("global-completion-b")
	beginB.Email = "b@example.com"
	receiptA, err := c.Begin(ctx, beginA)
	if err != nil {
		t.Fatal(err)
	}
	clock.Add(time.Minute)
	receiptB, err := c.Begin(ctx, beginB)
	if err != nil {
		t.Fatal(err)
	}
	attemptA := ChallengeVerify{ChallengeID: receiptA.ChallengeID, RegistrationID: beginA.RegistrationID, Purpose: domain.OperatorRegistration, VerificationID: domain.VerificationID(testUUID("global-verify-a")), Code: "004281"}
	proofA, err := c.Verify(ctx, attemptA)
	if err != nil {
		t.Fatal(err)
	}
	attemptB := ChallengeVerify{ChallengeID: receiptB.ChallengeID, RegistrationID: beginB.RegistrationID, Purpose: domain.OperatorRegistration, VerificationID: domain.VerificationID(testUUID("global-verify-b")), Code: "004281"}
	proofB, err := c.Verify(ctx, attemptB)
	if err != nil {
		t.Fatal(err)
	}
	completionID := domain.CompletionID(testUUID("global-completion-id"))
	completionA := ChallengeComplete{ChallengeID: receiptA.ChallengeID, RegistrationID: beginA.RegistrationID, Purpose: domain.OperatorRegistration, CompletionID: completionID, ProofID: proofA.ProofID}
	if _, err := c.Complete(ctx, completionA); err != nil {
		t.Fatal(err)
	}
	completionB := ChallengeComplete{ChallengeID: receiptB.ChallengeID, RegistrationID: beginB.RegistrationID, Purpose: domain.OperatorRegistration, CompletionID: completionID, ProofID: proofB.ProofID}
	if _, err := c.Complete(ctx, completionB); !errors.Is(err, ErrCompletionIdentityConflict) {
		t.Fatalf("global completion binding error = %v", err)
	}

	restarted := newTestChallenges(t, repo, clock, secrets, 4096)
	replay, err := restarted.Verify(ctx, attemptA)
	if err != nil || replay != proofA {
		t.Fatalf("verification after restart = %#v, %v", replay, err)
	}
	completed, err := restarted.Complete(ctx, completionA)
	if err != nil {
		t.Fatalf("completion replay after restart = %#v, %v", completed, err)
	}
	clock.Add(24 * time.Hour)
	if _, err := restarted.Verify(ctx, attemptA); !errors.Is(err, ErrChallengeUnavailable) {
		t.Fatalf("proof replay after recovery deadline = %v", err)
	}
	if _, err := restarted.Complete(ctx, completionA); !errors.Is(err, ErrChallengeUnavailable) {
		t.Fatalf("completion replay after recovery deadline = %v", err)
	}
}

func TestKnownStorageFailureLeavesStateRetryable(t *testing.T) {
	ctx := context.Background()
	clock := &challengeClock{now: time.Date(2026, 10, 12, 12, 0, 0, 0, time.FixedZone("MSK", 3*60*60))}
	repo, secrets := newChallengeRepo(), newChallengeSecrets()
	c := newTestChallenges(t, repo, clock, secrets, 4096)
	known := errors.New("storage unavailable")
	repo.setSaveError(known)
	input := validBegin("known-storage")
	if _, err := c.Begin(ctx, input); !errors.Is(err, known) {
		t.Fatalf("known storage error = %v", err)
	}
	repo.setSaveError(nil)
	if _, err := c.Begin(ctx, input); err != nil {
		t.Fatalf("retry after known failed commit: %v", err)
	}
}

func TestProcessOneNeverSendsAtOrAfterExpiry(t *testing.T) {
	ctx := context.Background()
	clock := &challengeClock{now: time.Date(2026, 10, 12, 12, 0, 0, 0, time.FixedZone("MSK", 3*60*60))}
	repo, secrets := newChallengeRepo(), newChallengeSecrets()
	c := newTestChallenges(t, repo, clock, secrets, 4096)
	if _, err := c.Begin(ctx, validBegin("worker-expiry")); err != nil {
		t.Fatal(err)
	}
	clock.Add(10 * time.Minute)
	sender := &captureCodeSender{}
	if worked, err := c.ProcessOne(ctx, sender); err != nil || worked {
		t.Fatalf("expired ProcessOne worked=%v err=%v", worked, err)
	}
	sender.mu.Lock()
	calls := sender.calls
	sender.mu.Unlock()
	if calls != 0 {
		t.Fatalf("expired issuance sent %d times", calls)
	}
	stored := repo.only()
	if stored.State != domain.ChallengeExpired || len(stored.EncryptedCode.Ciphertext) != 0 || stored.Issues[0].Job.State != domain.Failed {
		t.Fatalf("expiry state was not scrubbed: %#v", stored)
	}
}

func TestRestoreFailsClosedWhenRetainedCodeDigestDoesNotMatch(t *testing.T) {
	ctx := context.Background()
	clock := &challengeClock{now: time.Date(2026, 10, 12, 12, 0, 0, 0, time.FixedZone("MSK", 3*60*60))}
	repo, secrets := newChallengeRepo(), newChallengeSecrets()
	c := newTestChallenges(t, repo, clock, secrets, 4096)
	if _, err := c.Begin(ctx, validBegin("tampered-restore")); err != nil {
		t.Fatal(err)
	}
	stored := repo.only()
	stored.CodeDigest[0] ^= 1
	if err := repo.SaveChallenge(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if _, err := NewChallenges(ctx, repo, secrets, clock, 4096); !errors.Is(err, ErrChallengeCrypto) {
		t.Fatalf("tampered code digest restore error = %v", err)
	}
}

func TestProcessOneScrubsWhenDeliveryFinishesAtExpiry(t *testing.T) {
	ctx := context.Background()
	clock := &challengeClock{now: time.Date(2026, 10, 12, 12, 0, 0, 0, time.FixedZone("MSK", 3*60*60))}
	repo, secrets := newChallengeRepo(), newChallengeSecrets()
	c := newTestChallenges(t, repo, clock, secrets, 4096)
	if _, err := c.Begin(ctx, validBegin("worker-finish-expiry")); err != nil {
		t.Fatal(err)
	}
	sender := &captureCodeSender{started: make(chan struct{}, 1), release: make(chan struct{}), results: []domain.DeliveryResult{{Success: true}}}
	workerDone := make(chan error, 1)
	go func() {
		_, err := c.ProcessOne(ctx, sender)
		workerDone <- err
	}()
	<-sender.started
	clock.Add(10 * time.Minute)
	close(sender.release)
	if err := <-workerDone; err != nil {
		t.Fatal(err)
	}
	stored := repo.only()
	if stored.State != domain.ChallengeExpired || len(stored.EncryptedCode.Ciphertext) != 0 || stored.Issues[0].Job.State != domain.Failed {
		t.Fatalf("worker completion restored expired state: %#v", stored)
	}
}

func TestWrongVerificationReplayClosesAtRetentionBoundary(t *testing.T) {
	ctx := context.Background()
	clock := &challengeClock{now: time.Date(2026, 10, 12, 12, 0, 0, 0, time.FixedZone("MSK", 3*60*60))}
	c := newTestChallenges(t, newChallengeRepo(), clock, newChallengeSecrets(), 4096)
	receipt, err := c.Begin(ctx, validBegin("wrong-replay-retention"))
	if err != nil {
		t.Fatal(err)
	}
	attempt := ChallengeVerify{ChallengeID: receipt.ChallengeID, RegistrationID: domain.RegistrationID(testUUID("wrong-replay-retention")), Purpose: domain.OperatorRegistration, VerificationID: domain.VerificationID(testUUID("wrong-replay-id")), Code: "999999"}
	if _, err := c.Verify(ctx, attempt); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("initial wrong verification error = %v", err)
	}
	clock.Add(24 * time.Hour)
	if _, err := c.Verify(ctx, attempt); !errors.Is(err, ErrChallengeUnavailable) {
		t.Fatalf("wrong replay at retention boundary = %v", err)
	}
}

func TestKnownVerifyCommitFailureKeepsAcceptedMailPending(t *testing.T) {
	ctx := context.Background()
	clock := &challengeClock{now: time.Date(2026, 10, 12, 12, 0, 0, 0, time.FixedZone("MSK", 3*60*60))}
	repo, secrets := newChallengeRepo(), newChallengeSecrets()
	c := newTestChallenges(t, repo, clock, secrets, 4096)
	registration := testUUID("known-verify-commit")
	receipt, err := c.Begin(ctx, ChallengeBegin{RegistrationID: domain.RegistrationID(registration), Email: "operator@example.com", Purpose: domain.OperatorRegistration})
	if err != nil {
		t.Fatal(err)
	}
	known := errors.New("known storage failure")
	repo.setSaveError(known)
	_, verifyErr := c.Verify(ctx, ChallengeVerify{ChallengeID: receipt.ChallengeID, RegistrationID: domain.RegistrationID(registration), Purpose: domain.OperatorRegistration, VerificationID: domain.VerificationID(testUUID("known-verify-id")), Code: "004281"})
	if !errors.Is(verifyErr, known) {
		t.Fatalf("verify error = %v", verifyErr)
	}
	repo.setSaveError(nil)
	sender := &captureCodeSender{results: []domain.DeliveryResult{{Success: true}}}
	worked, err := c.ProcessOne(ctx, sender)
	if err != nil || !worked {
		t.Fatalf("accepted job after known verify failure worked=%v err=%v", worked, err)
	}
	sender.mu.Lock()
	calls := sender.calls
	sender.mu.Unlock()
	if calls != 1 {
		t.Fatalf("accepted mail send calls = %d, want 1", calls)
	}
}

func TestKnownWorkerClaimFailureLeavesJobPending(t *testing.T) {
	ctx := context.Background()
	clock := &challengeClock{now: time.Date(2026, 10, 12, 12, 0, 0, 0, time.FixedZone("MSK", 3*60*60))}
	repo, secrets := newChallengeRepo(), newChallengeSecrets()
	c := newTestChallenges(t, repo, clock, secrets, 4096)
	if _, err := c.Begin(ctx, validBegin("known-claim-commit")); err != nil {
		t.Fatal(err)
	}
	known := errors.New("known storage failure")
	repo.setSaveError(known)
	if worked, err := c.ProcessOne(ctx, &captureCodeSender{}); worked || !errors.Is(err, known) {
		t.Fatalf("claim failure worked=%v err=%v", worked, err)
	}
	repo.setSaveError(nil)
	sender := &captureCodeSender{results: []domain.DeliveryResult{{Success: true}}}
	if worked, err := c.ProcessOne(ctx, sender); err != nil || !worked {
		t.Fatalf("job after known claim failure worked=%v err=%v", worked, err)
	}
	sender.mu.Lock()
	calls := sender.calls
	sender.mu.Unlock()
	if calls != 1 {
		t.Fatalf("mail send calls = %d, want 1", calls)
	}
}
