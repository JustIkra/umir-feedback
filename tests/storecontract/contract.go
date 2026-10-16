// Package storecontract holds the behaviour every feedback store adapter must
// share. The memory adapter runs it in unit tests, the PostgreSQL adapter under
// the integration tag.
package storecontract

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/JustIkra/umir-feedback/internal/application"
	"github.com/JustIkra/umir-feedback/internal/domain"
)

// Store is the surface shared by the adapters.
type Store interface {
	application.Store
	Ready() bool
	Close() error
}

// Clock is the movable time source the contract gives to the store under test.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// Settings are what a harness must configure the store with.
type Settings struct {
	Clock     *Clock
	Lease     time.Duration
	Retention application.RetentionConfig
	Quota     application.QuotaConfig
}

// Harness creates stores for the contract.
type Harness struct {
	// Open returns a store over empty storage configured with Policy() and the settings.
	Open func(t *testing.T, s Settings) Store
	// Reopen closes the store and opens a new one over the same storage, as after a process restart.
	Reopen func(t *testing.T, s Store, settings Settings) Store
}

// Policy is the retry policy the harness must configure.
func Policy() domain.RetryPolicy { return domain.DefaultRetryPolicy() }

var base = time.Date(2026, 10, 15, 10, 0, 0, 0, domain.MoscowLocation())

const (
	idA = "123e4567-e89b-42d3-a456-426614174001"
	idB = "123e4567-e89b-42d3-a456-426614174002"
	idC = "123e4567-e89b-42d3-a456-426614174003"
)

func settings() Settings {
	c := &Clock{now: base}
	return Settings{Clock: c, Lease: 5 * time.Minute, Retention: application.RetentionConfig{AnonymousDays: 30, RestaurantDays: 180, DeletedScopeDays: 7, TombstoneDays: 180}}
}

const (
	fpA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	fpB = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

func submission(id string, at time.Time) domain.Submission {
	return domain.Submission{
		Namespace: domain.Anonymous(), CommandID: id, ID: domain.SubmissionID(id),
		Name: "Ada Example", Email: "ada@example.test", Phone: "+79990000000", Message: "A useful message",
		ConsentVersion: "2026-10-12", ConsentPurpose: "feedback-reply", AcceptedAt: at, ReceivedAt: at, Fingerprint: fpA,
	}
}

func appNamespace(org, tenant string) domain.Namespace {
	return domain.Namespace{Kind: "app", UserID: "123e4567-e89b-42d3-a456-426614174100", OrgID: org, TenantID: tenant}
}

const (
	orgOne      = "123e4567-e89b-42d3-a456-426614174201"
	orgTwo      = "123e4567-e89b-42d3-a456-426614174202"
	tenantOne   = "123e4567-e89b-42d3-a456-426614174301"
	tenantTwo   = "123e4567-e89b-42d3-a456-426614174302"
	tenantThree = "123e4567-e89b-42d3-a456-426614174303"
)

func appSubmission(ns domain.Namespace, id string, at time.Time) domain.Submission {
	s := submission(id, at)
	s.Namespace = ns
	return s
}

// Run executes the shared contract against the harness.
func Run(t *testing.T, h Harness) {
	t.Helper()
	cases := []struct {
		name string
		run  func(*testing.T, Harness)
	}{
		{"AcceptIsIdempotentAndDetectsConflict", acceptIdempotent},
		{"AcceptRejectsInvalidSubmission", acceptInvalid},
		{"ConcurrentAcceptCreatesOnce", concurrentAccept},
		{"LookupIsScoped", lookupScoped},
		{"ClaimOrdersByReceiptAndSeparatesChannels", claimOrder},
		{"ClaimHonoursNextAttempt", claimNextAttempt},
		{"FinishValidatesTransitions", finishTransitions},
		{"FinishRejectsStaleFence", finishFence},
		{"ClaimFailsExhaustedJobs", claimExhausted},
		{"RetryReopensFailedJobOfAnyNamespace", retryFailed},
		{"EraseEmailKeepsOtherScopesAndReplayAnswersFromTombstone", eraseEmail},
		{"ListScopedIsKeysetAndStable", listScoped},
		{"ConcurrentClaimsNeverShareAJob", concurrentClaim},
		{"RestartKeepsStateAndRequeuesInFlight", restart},
		{"ExpiredLeaseIsRequeued", leaseExpires},
		{"ReadyReflectsOpenStore", ready},
		{"QuotaIsPerNamespaceAndLeavesNothing", quota},
		{"TextAndPhoneClearedAfterBothSent", payloadCleared},
		{"SweepErasesThenDeletesOnTheClock", sweepClock},
		{"SweepSkipsLiveLease", sweepSkipsLease},
		{"ScopeDeletionFencesIntakeAndFailsPendingJobs", scopeDeleted},
		{"ScopeDeletionIgnoresStaleRevisionAndShortensRetention", scopeRevision},
		{"StatusIsPagedAndHasNoPersonalData", statusPaged},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.run(t, h) })
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func accept(t *testing.T, s Store, sub domain.Submission) {
	t.Helper()
	_, created, err := s.Accept(context.Background(), sub)
	must(t, err)
	if !created {
		t.Fatalf("submission %s was not created", sub.ID)
	}
}

func claim(t *testing.T, s Store, channel domain.Channel, at time.Time) (domain.Delivery, bool) {
	t.Helper()
	d, ok, err := s.Claim(context.Background(), channel, at, Policy())
	must(t, err)
	return d, ok
}

func statusAll(t *testing.T, s Store) []application.DeliveryView {
	t.Helper()
	var all []application.DeliveryView
	var after int64
	for {
		page, err := s.Status(context.Background(), after, 100)
		must(t, err)
		if len(page) == 0 {
			return all
		}
		all = append(all, page...)
		after = page[len(page)-1].Seq
	}
}

func acceptIdempotent(t *testing.T, h Harness) {
	s := h.Open(t, settings())
	ctx := context.Background()
	sub := submission(idA, base)
	receipt, created, err := s.Accept(ctx, sub)
	if err != nil || !created || receipt.SubmissionID != idA || !receipt.ReceivedAt.Equal(base) {
		t.Fatalf("first accept: %+v %v %v", receipt, created, err)
	}
	later := sub
	later.AcceptedAt, later.ReceivedAt = base.Add(time.Hour), base.Add(time.Hour)
	receipt, created, err = s.Accept(ctx, later)
	if err != nil || created || !receipt.ReceivedAt.Equal(base) {
		t.Fatalf("replay must return the original receipt: %+v %v %v", receipt, created, err)
	}
	conflict := sub
	conflict.Message = "Another message"
	conflict.Fingerprint = fpB
	if _, _, err := s.Accept(ctx, conflict); !errors.Is(err, application.ErrIdentityConflict) {
		t.Fatalf("conflict: %v", err)
	}
	got, found, err := s.Lookup(ctx, idA)
	if err != nil || !found || got.Message != sub.Message || got.Email != sub.Email || !got.ReceivedAt.Equal(base) || got.Namespace.Kind != "anonymous" || got.Erased {
		t.Fatalf("lookup: %+v %v %v", got, found, err)
	}
	statuses := statusAll(t, s)
	if len(statuses) != 2 || statuses[0].Job.Channel != domain.SMTP || statuses[1].Job.Channel != domain.Telegram {
		t.Fatalf("status: %+v", statuses)
	}
	for _, d := range statuses {
		if d.Job.State != domain.Pending || d.Job.Attempts != 0 || !d.Job.NextAttemptAt.Equal(base) || d.Job.ID == "" {
			t.Fatalf("initial job: %+v", d.Job)
		}
	}
	if statuses[0].Job.ID == statuses[1].Job.ID {
		t.Fatal("jobs must have distinct identities")
	}
}

func acceptInvalid(t *testing.T, h Harness) {
	s := h.Open(t, settings())
	bad := submission(idA, base)
	bad.Email = "not-an-email"
	if _, _, err := s.Accept(context.Background(), bad); err == nil {
		t.Fatal("invalid submission accepted")
	}
	if _, found, err := s.Lookup(context.Background(), idA); err != nil || found {
		t.Fatalf("invalid submission stored: %v %v", found, err)
	}
}

func concurrentAccept(t *testing.T, h Harness) {
	s := h.Open(t, settings())
	var wg sync.WaitGroup
	var mu sync.Mutex
	created := 0
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ok, err := s.Accept(context.Background(), submission(idA, base))
			if err != nil {
				t.Error(err)
				return
			}
			if ok {
				mu.Lock()
				created++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if created != 1 {
		t.Fatalf("created %d times", created)
	}
	if got := len(statusAll(t, s)); got != 2 {
		t.Fatalf("jobs: %d", got)
	}
}

func lookupScoped(t *testing.T, h Harness) {
	s := h.Open(t, settings())
	ctx := context.Background()
	one := appNamespace(orgOne, tenantOne)
	two := appNamespace(orgOne, tenantTwo)
	accept(t, s, submission(idA, base))
	accept(t, s, appSubmission(one, idA, base.Add(time.Minute)))
	got, found, err := s.LookupScoped(ctx, one, idA)
	if err != nil || !found || got.Namespace != one || !got.ReceivedAt.Equal(base.Add(time.Minute)) {
		t.Fatalf("scoped lookup: %+v %v %v", got, found, err)
	}
	if _, found, err := s.LookupScoped(ctx, two, idA); err != nil || found {
		t.Fatalf("other restaurant must not see the submission: %v %v", found, err)
	}
	anonymous, found, err := s.Lookup(ctx, idA)
	if err != nil || !found || anonymous.Namespace.Kind != "anonymous" {
		t.Fatalf("anonymous lookup: %+v %v %v", anonymous, found, err)
	}
	if _, _, err := s.LookupScoped(ctx, domain.Namespace{Kind: "app"}, idA); err == nil {
		t.Fatal("invalid namespace accepted")
	}
}

func claimOrder(t *testing.T, h Harness) {
	s := h.Open(t, settings())
	accept(t, s, submission(idB, base.Add(time.Minute)))
	accept(t, s, submission(idA, base))
	now := base.Add(time.Hour)
	first, ok := claim(t, s, domain.SMTP, now)
	if !ok || first.Submission.ID != idA || first.Job.State != domain.InFlight || first.Job.Attempts != 1 || !first.Job.StartedAt.Equal(now) || first.Job.Channel != domain.SMTP || first.Job.Fence == 0 {
		t.Fatalf("first claim: %+v %v", first, ok)
	}
	telegram, ok := claim(t, s, domain.Telegram, now)
	if !ok || telegram.Submission.ID != idA || telegram.Job.Channel != domain.Telegram || telegram.Job.ID == first.Job.ID || telegram.Job.Fence == first.Job.Fence {
		t.Fatalf("telegram claim must be independent: %+v %v", telegram, ok)
	}
	second, ok := claim(t, s, domain.SMTP, now)
	if !ok || second.Submission.ID != idB {
		t.Fatalf("second claim: %+v %v", second, ok)
	}
	if _, ok := claim(t, s, domain.SMTP, now); ok {
		t.Fatal("nothing left to claim")
	}
	if _, _, err := s.Claim(context.Background(), "carrier-pigeon", now, Policy()); err == nil {
		t.Fatal("unknown channel accepted")
	}
	if _, _, err := s.Claim(context.Background(), domain.SMTP, now.UTC(), Policy()); err == nil {
		t.Fatal("non-Moscow time accepted")
	}
}

func claimNextAttempt(t *testing.T, h Harness) {
	s := h.Open(t, settings())
	ctx := context.Background()
	accept(t, s, submission(idA, base))
	d, ok := claim(t, s, domain.SMTP, base)
	if !ok {
		t.Fatal("expected a claim")
	}
	retryAt := base.Add(time.Minute)
	job := d.Job
	job.State, job.ErrorCode, job.NextAttemptAt = domain.Pending, "smtp_transient", retryAt
	must(t, s.Finish(ctx, idA, domain.SMTP, job))
	if _, ok := claim(t, s, domain.SMTP, retryAt.Add(-time.Second)); ok {
		t.Fatal("claimed before the retry time")
	}
	again, ok := claim(t, s, domain.SMTP, retryAt)
	if !ok || again.Job.Attempts != 2 || again.Job.ID != d.Job.ID || again.Job.ErrorCode != "" || again.Job.Fence <= d.Job.Fence {
		t.Fatalf("retry claim: %+v %v", again, ok)
	}
}

func finishTransitions(t *testing.T, h Harness) {
	s := h.Open(t, settings())
	ctx := context.Background()
	accept(t, s, submission(idA, base))
	never := domain.Job{Channel: domain.SMTP, State: domain.Sent, Attempts: 1, StartedAt: base, ConfirmedAt: base}
	if err := s.Finish(ctx, idA, domain.SMTP, never); err == nil {
		t.Fatal("finishing a job that was never claimed must fail")
	}
	d, ok := claim(t, s, domain.SMTP, base)
	if !ok {
		t.Fatal("expected a claim")
	}
	wrongAttempts := d.Job
	wrongAttempts.State, wrongAttempts.ConfirmedAt, wrongAttempts.Attempts = domain.Sent, base, 2
	if err := s.Finish(ctx, idA, domain.SMTP, wrongAttempts); err == nil {
		t.Fatal("attempt count must not change")
	}
	wrongID := d.Job
	wrongID.ID, wrongID.State, wrongID.ConfirmedAt = "123e4567-e89b-42d3-a456-426614174999", domain.Sent, base
	if err := s.Finish(ctx, idA, domain.SMTP, wrongID); err == nil {
		t.Fatal("job identity must not change")
	}
	if err := s.Finish(ctx, idB, domain.SMTP, d.Job); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("unknown submission: %v", err)
	}
	sent := d.Job
	sent.State, sent.ConfirmedAt, sent.ProviderID = domain.Sent, base.Add(time.Second), "message-1"
	must(t, s.Finish(ctx, idA, domain.SMTP, sent))
	if err := s.Finish(ctx, idA, domain.SMTP, sent); err == nil {
		t.Fatal("a finished job cannot be finished again")
	}
	statuses := statusAll(t, s)
	if got := statuses[0].Job; got.State != domain.Sent || got.ProviderID != "message-1" || !got.ConfirmedAt.Equal(base.Add(time.Second)) || got.Attempts != 1 {
		t.Fatalf("sent job: %+v", got)
	}
	if got := statuses[1].Job; got.State != domain.Pending {
		t.Fatalf("telegram job must be untouched: %+v", got)
	}
	if _, ok := claim(t, s, domain.SMTP, base.Add(time.Hour)); ok {
		t.Fatal("sent job claimed again")
	}
}

func finishFence(t *testing.T, h Harness) {
	st := settings()
	s := h.Open(t, st)
	ctx := context.Background()
	accept(t, s, submission(idA, base))
	first, ok := claim(t, s, domain.SMTP, base)
	if !ok {
		t.Fatal("expected a claim")
	}
	// The lease expires and another worker takes the job over.
	takeover := base.Add(st.Lease + 30*time.Minute)
	second, ok := claim(t, s, domain.SMTP, takeover)
	if !ok || second.Job.ID != first.Job.ID || second.Job.Fence <= first.Job.Fence {
		t.Fatalf("takeover: %+v %v", second, ok)
	}
	stale := first.Job
	stale.State, stale.ConfirmedAt = domain.Sent, takeover
	if err := s.Finish(ctx, idA, domain.SMTP, stale); !errors.Is(err, application.ErrLeaseLost) {
		t.Fatalf("stale owner must lose: %v", err)
	}
	// Even a stale result with the right attempt count after an administrative retry is refused.
	fail := second.Job
	fail.State, fail.ErrorCode, fail.ConfirmedAt = domain.Failed, "smtp_permanent", takeover.Add(time.Second)
	must(t, s.Finish(ctx, idA, domain.SMTP, fail))
	retryAt := takeover.Add(time.Minute)
	must(t, s.Retry(ctx, domain.Anonymous(), idA, domain.SMTP, retryAt))
	third, ok := claim(t, s, domain.SMTP, retryAt)
	if !ok || third.Job.Attempts != 1 || third.Job.Fence <= second.Job.Fence {
		t.Fatalf("claim after retry: %+v %v", third, ok)
	}
	replay := second.Job
	replay.Attempts, replay.State, replay.ConfirmedAt = 1, domain.Sent, retryAt
	replay.StartedAt, replay.RetryStartedAt = third.Job.StartedAt, third.Job.RetryStartedAt
	if err := s.Finish(ctx, idA, domain.SMTP, replay); !errors.Is(err, application.ErrLeaseLost) {
		t.Fatalf("a result from before the retry must lose: %v", err)
	}
	ok2 := third.Job
	ok2.State, ok2.ConfirmedAt = domain.Sent, retryAt
	must(t, s.Finish(ctx, idA, domain.SMTP, ok2))
}

func claimExhausted(t *testing.T, h Harness) {
	s := h.Open(t, settings())
	accept(t, s, submission(idA, base))
	late := base.Add(Policy().MaxAge + time.Minute)
	if _, ok := claim(t, s, domain.SMTP, late); ok {
		t.Fatal("an expired job must not be delivered")
	}
	statuses := statusAll(t, s)
	smtp := statuses[0].Job
	if smtp.State != domain.Failed || smtp.ErrorCode != "retry_exhausted" || smtp.Attempts != 0 || !smtp.ConfirmedAt.Equal(late) {
		t.Fatalf("exhausted job: %+v", smtp)
	}
	if statuses[1].Job.State != domain.Pending {
		t.Fatalf("other channel must be untouched until claimed: %+v", statuses[1].Job)
	}
}

func retryFailed(t *testing.T, h Harness) {
	s := h.Open(t, settings())
	ctx := context.Background()
	ns := appNamespace(orgOne, tenantOne)
	accept(t, s, submission(idA, base))
	accept(t, s, appSubmission(ns, idA, base))
	if err := s.Retry(ctx, domain.Anonymous(), idA, domain.SMTP, base); !errors.Is(err, application.ErrNotRetryable) {
		t.Fatalf("only a failed delivery can be retried: %v", err)
	}
	late := base.Add(Policy().MaxAge + time.Minute)
	for range 2 {
		claim(t, s, domain.SMTP, late)
	}
	if err := s.Retry(ctx, domain.Anonymous(), idB, domain.SMTP, late); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("unknown submission: %v", err)
	}
	must(t, s.Retry(ctx, ns, idA, domain.SMTP, late))
	d, ok := claim(t, s, domain.SMTP, late)
	if !ok || d.Job.Attempts != 1 || !d.Job.RetryStartedAt.Equal(late) || d.Submission.ID != idA || d.Submission.Namespace != ns {
		t.Fatalf("retried claim must be the restaurant submission: %+v %v", d, ok)
	}
	for _, v := range statusAll(t, s) {
		if v.Namespace.Kind == "anonymous" && v.Job.Channel == domain.SMTP && v.Job.State != domain.Failed {
			t.Fatalf("the anonymous twin must be untouched: %+v", v)
		}
	}
}

func eraseEmail(t *testing.T, h Harness) {
	st := settings()
	s := h.Open(t, st)
	ctx := context.Background()
	ns := appNamespace(orgOne, tenantOne)
	grace := submission(idB, base)
	grace.Email = "grace@example.test"
	accept(t, s, submission(idA, base))
	accept(t, s, grace)
	accept(t, s, appSubmission(ns, idC, base))
	n, err := s.EraseEmail(ctx, "grace@example.test", base)
	if err != nil || n != 1 {
		t.Fatalf("erase: %d %v", n, err)
	}
	got, found, err := s.Lookup(ctx, idB)
	if err != nil || !found || !got.Erased || got.Email != "" || got.Name != "" || got.Phone != "" || got.Message != "" || got.Fingerprint != fpA {
		t.Fatalf("an erased submission keeps only identity and fingerprint: %+v %v %v", got, found, err)
	}
	if v, _ := lookup(t, s, idA); v.Erased {
		t.Fatal("other address erased")
	}
	if v, _ := lookupScopedOK(t, s, ns, idC); v.Erased {
		t.Fatal("the restaurant submission has another address and must stay")
	}
	// The same content replays to the original receipt, other content conflicts.
	receipt, created, err := s.Accept(ctx, grace)
	if err != nil || created || !receipt.ReceivedAt.Equal(base) {
		t.Fatalf("replay after erase: %+v %v %v", receipt, created, err)
	}
	other := grace
	other.Fingerprint = fpB
	if _, _, err := s.Accept(ctx, other); !errors.Is(err, application.ErrIdentityConflict) {
		t.Fatalf("different content after erase: %v", err)
	}
	// An erased submission is not delivered any more.
	var erasedJobs int
	for _, v := range statusAll(t, s) {
		if v.SubmissionID == idB {
			erasedJobs++
			if !v.Erased || v.Job.State != domain.Failed || v.Job.ErrorCode != "retention_expired" {
				t.Fatalf("jobs of an erased submission: %+v", v)
			}
		}
	}
	if erasedJobs != 2 {
		t.Fatalf("jobs of an erased submission stay for the report: %d", erasedJobs)
	}
	if n, err := s.EraseEmail(ctx, "nobody@example.test", base); err != nil || n != 0 {
		t.Fatalf("unknown address: %d %v", n, err)
	}
	if err := s.Retry(ctx, domain.Anonymous(), idB, domain.SMTP, base); err == nil {
		t.Fatal("an erased submission has nothing to deliver")
	}
}

func listScoped(t *testing.T, h Harness) {
	s := h.Open(t, settings())
	ctx := context.Background()
	one := appNamespace(orgOne, tenantOne)
	accept(t, s, appSubmission(one, idB, base.Add(time.Minute)))
	accept(t, s, appSubmission(one, idA, base))
	accept(t, s, appSubmission(appNamespace(orgOne, tenantTwo), idC, base))
	accept(t, s, appSubmission(appNamespace(orgTwo, tenantThree), idC, base))
	accept(t, s, submission(idC, base))
	rows, err := s.ListScoped(ctx, orgOne, tenantOne, application.ListPosition{}, 10)
	must(t, err)
	if len(rows) != 2 || rows[0].Submission.ID != idA || rows[1].Submission.ID != idB {
		t.Fatalf("scoped rows: %+v", rows)
	}
	first, err := s.ListScoped(ctx, orgOne, tenantOne, application.ListPosition{}, 1)
	must(t, err)
	// A row inserted between pages, earlier than the cursor, changes nothing for the next page.
	accept(t, s, appSubmission(one, "123e4567-e89b-42d3-a456-426614174004", base.Add(-time.Hour)))
	next, err := s.ListScoped(ctx, orgOne, tenantOne, first[0].Position, 10)
	must(t, err)
	if len(first) != 1 || first[0].Submission.ID != idA || len(next) != 1 || next[0].Submission.ID != idB {
		t.Fatalf("keyset paging: %+v %+v", first, next)
	}
	rows, err = s.ListScoped(ctx, orgTwo, tenantTwo, application.ListPosition{}, 10)
	must(t, err)
	if rows == nil || len(rows) != 0 {
		t.Fatalf("empty scope must be an empty list: %#v", rows)
	}
}

func concurrentClaim(t *testing.T, h Harness) {
	s := h.Open(t, settings())
	const total = 12
	for i := range total {
		accept(t, s, submission(fmt.Sprintf("123e4567-e89b-42d3-a456-4266141741%02d", i), base.Add(time.Duration(i)*time.Second)))
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	claimed := map[string]int{}
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				d, ok, err := s.Claim(context.Background(), domain.SMTP, base.Add(time.Hour), Policy())
				if err != nil {
					t.Error(err)
					return
				}
				if !ok {
					return
				}
				mu.Lock()
				claimed[d.Job.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(claimed) != total {
		t.Fatalf("claimed %d of %d jobs", len(claimed), total)
	}
	for id, n := range claimed {
		if n != 1 {
			t.Fatalf("job %s claimed %d times", id, n)
		}
	}
}

func restart(t *testing.T, h Harness) {
	st := settings()
	s := h.Open(t, st)
	ctx := context.Background()
	ns := appNamespace(orgOne, tenantOne)
	accept(t, s, submission(idA, base))
	accept(t, s, appSubmission(ns, idB, base))
	claimedAt := base.Add(time.Minute)
	d, ok := claim(t, s, domain.SMTP, claimedAt)
	if !ok {
		t.Fatal("expected a claim")
	}
	s = h.Reopen(t, s, st)
	if got, found, err := s.Lookup(ctx, idA); err != nil || !found || !got.ReceivedAt.Equal(base) || got.Email != "ada@example.test" {
		t.Fatalf("submission lost across restart: %+v %v %v", got, found, err)
	}
	if _, found, err := s.LookupScoped(ctx, ns, idB); err != nil || !found {
		t.Fatalf("scoped submission lost across restart: %v %v", found, err)
	}
	if got := len(statusAll(t, s)); got != 4 {
		t.Fatalf("jobs lost across restart: %d", got)
	}
	// The worker died before Finish: the job must be delivered again, with the same identity and a higher attempt.
	again, ok := claim(t, s, domain.SMTP, claimedAt.Add(time.Hour))
	if !ok || again.Job.ID != d.Job.ID || again.Job.Attempts != 2 || again.Submission.ID != idA || again.Job.Fence <= d.Job.Fence {
		t.Fatalf("in-flight job was not requeued: %+v %v", again, ok)
	}
}

func leaseExpires(t *testing.T, h Harness) {
	st := settings()
	s := h.Open(t, st)
	accept(t, s, submission(idA, base))
	d, ok := claim(t, s, domain.SMTP, base)
	if !ok {
		t.Fatal("expected a claim")
	}
	if _, ok := claim(t, s, domain.SMTP, base.Add(st.Lease-time.Second)); ok {
		t.Fatal("a live lease must not be claimed")
	}
	again, ok := claim(t, s, domain.SMTP, base.Add(st.Lease+time.Minute))
	if !ok || again.Job.ID != d.Job.ID || again.Job.Attempts != 2 {
		t.Fatalf("expired lease: %+v %v", again, ok)
	}
}

func ready(t *testing.T, h Harness) {
	s := h.Open(t, settings())
	if !s.Ready() {
		t.Fatal("open store must be ready")
	}
	must(t, s.Close())
	if s.Ready() {
		t.Fatal("closed store must not be ready")
	}
	must(t, s.Close())
}

func quota(t *testing.T, h Harness) {
	st := settings()
	st.Quota = application.QuotaConfig{Anonymous: 2, Restaurant: 1}
	s := h.Open(t, st)
	ctx := context.Background()
	one, two := appNamespace(orgOne, tenantOne), appNamespace(orgOne, tenantTwo)
	accept(t, s, submission(idA, base))
	accept(t, s, submission(idB, base))
	if _, _, err := s.Accept(ctx, submission(idC, base)); !errors.Is(err, application.ErrQuotaExceeded) {
		t.Fatalf("anonymous quota: %v", err)
	}
	if _, found := lookup(t, s, idC); found {
		t.Fatal("a refused submission left a row")
	}
	if got := len(statusAll(t, s)); got != 4 {
		t.Fatalf("a refused submission left jobs: %d", got)
	}
	accept(t, s, appSubmission(one, idA, base))
	accept(t, s, appSubmission(two, idA, base))
	if _, _, err := s.Accept(ctx, appSubmission(one, idB, base)); !errors.Is(err, application.ErrQuotaExceeded) {
		t.Fatalf("restaurant quota: %v", err)
	}
	// A replay of an accepted submission is not a new row and passes at the limit.
	if _, created, err := s.Accept(ctx, submission(idA, base)); err != nil || created {
		t.Fatalf("replay at the limit: %v %v", created, err)
	}
	// Erasing frees the room.
	if n, err := s.EraseEmail(ctx, "ada@example.test", base); err != nil || n != 4 {
		t.Fatalf("erase: %d %v", n, err)
	}
	accept(t, s, submission(idC, base))
}

func payloadCleared(t *testing.T, h Harness) {
	s := h.Open(t, settings())
	ctx := context.Background()
	accept(t, s, submission(idA, base))
	send := func(ch domain.Channel, state domain.JobState) {
		d, ok := claim(t, s, ch, base)
		if !ok {
			t.Fatalf("claim %s", ch)
		}
		j := d.Job
		j.State, j.ConfirmedAt = state, base
		if state == domain.Failed {
			j.ErrorCode = "smtp_permanent"
		}
		must(t, s.Finish(ctx, idA, ch, j))
	}
	send(domain.SMTP, domain.Sent)
	if got, _ := lookup(t, s, idA); got.Message == "" || got.Phone == "" {
		t.Fatal("the text is needed until both channels are sent")
	}
	send(domain.Telegram, domain.Sent)
	got, _ := lookup(t, s, idA)
	if got.Message != "" || got.Phone != "" || got.Name == "" || got.Email == "" || got.Erased {
		t.Fatalf("after two sent only text and phone go: %+v", got)
	}
	// A failed channel keeps the text for a manual retry.
	accept(t, s, submission(idB, base))
	for _, ch := range []domain.Channel{domain.SMTP, domain.Telegram} {
		d, ok := claim(t, s, ch, base)
		if !ok || d.Submission.ID != idB {
			t.Fatalf("claim %s: %+v", ch, d)
		}
		j := d.Job
		j.State, j.ConfirmedAt = domain.Sent, base
		if ch == domain.Telegram {
			j.State, j.ErrorCode = domain.Failed, "telegram_permanent"
		}
		must(t, s.Finish(ctx, idB, ch, j))
	}
	if got, _ := lookup(t, s, idB); got.Message == "" {
		t.Fatal("a failed channel keeps the text")
	}
}

func sweepClock(t *testing.T, h Harness) {
	st := settings()
	s := h.Open(t, st)
	ctx := context.Background()
	ns := appNamespace(orgOne, tenantOne)
	accept(t, s, submission(idA, base))
	accept(t, s, appSubmission(ns, idB, base))
	before := base.AddDate(0, 0, 30).Add(-time.Second)
	res, err := s.Sweep(ctx, before)
	if err != nil || res.Erased != 0 {
		t.Fatalf("too early: %+v %v", res, err)
	}
	at30 := base.AddDate(0, 0, 30)
	res, err = s.Sweep(ctx, at30)
	if err != nil || res.Erased != 1 {
		t.Fatalf("anonymous at 30 days: %+v %v", res, err)
	}
	if got, _ := lookup(t, s, idA); !got.Erased || got.Email != "" {
		t.Fatalf("anonymous not erased: %+v", got)
	}
	if got, _ := lookupScopedOK(t, s, ns, idB); got.Erased {
		t.Fatal("the restaurant submission lives 180 days")
	}
	res, err = s.Sweep(ctx, base.AddDate(0, 0, 180))
	if err != nil || res.Erased != 1 {
		t.Fatalf("restaurant at 180 days: %+v %v", res, err)
	}
	// Tombstones last 180 days from the erasure.
	res, err = s.Sweep(ctx, at30.AddDate(0, 0, 180).Add(-time.Second))
	if err != nil || res.Deleted != 0 {
		t.Fatalf("tombstone too early: %+v %v", res, err)
	}
	res, err = s.Sweep(ctx, at30.AddDate(0, 0, 180))
	if err != nil || res.Deleted != 1 {
		t.Fatalf("tombstone after 180 days: %+v %v", res, err)
	}
	if _, found := lookup(t, s, idA); found {
		t.Fatal("expired tombstone must be gone")
	}
	// After the tombstone the same id is a new submission.
	accept(t, s, submission(idA, at30.AddDate(0, 0, 181)))
}

func sweepSkipsLease(t *testing.T, h Harness) {
	st := settings()
	st.Retention.AnonymousDays = 1
	s := h.Open(t, st)
	ctx := context.Background()
	accept(t, s, submission(idA, base))
	due := base.AddDate(0, 0, 1)
	// A worker holds a live lease on the job when the purge time comes.
	if _, ok := claim(t, s, domain.SMTP, due.Add(-time.Minute)); !ok {
		t.Fatal("expected a claim")
	}
	res, err := s.Sweep(ctx, due)
	if err != nil || res.Erased != 0 {
		t.Fatalf("a submission with a running attempt must wait: %+v %v", res, err)
	}
	res, err = s.Sweep(ctx, due.Add(st.Lease+time.Minute))
	if err != nil || res.Erased != 1 {
		t.Fatalf("after the lease it is erased: %+v %v", res, err)
	}
}

func scopeDeleted(t *testing.T, h Harness) {
	st := settings()
	s := h.Open(t, st)
	ctx := context.Background()
	one, two := appNamespace(orgOne, tenantOne), appNamespace(orgOne, tenantTwo)
	accept(t, s, appSubmission(one, idA, base))
	accept(t, s, appSubmission(two, idB, base))
	inFlight, ok := claim(t, s, domain.SMTP, base)
	if !ok || inFlight.Submission.Namespace != one {
		t.Fatalf("claim: %+v", inFlight)
	}
	deletedAt := base.Add(time.Hour)
	ev := application.ScopeDeleted{Kind: application.ScopeRestaurant, ID: tenantOne, EventID: "evt-1", Revision: 5, DeletedAt: deletedAt}
	must(t, s.ApplyScopeDeleted(ctx, ev))
	must(t, s.ApplyScopeDeleted(ctx, ev)) // idempotent
	if _, _, err := s.Accept(ctx, appSubmission(one, idC, deletedAt)); !errors.Is(err, application.ErrScopeDeleted) {
		t.Fatalf("intake into a deleted restaurant: %v", err)
	}
	accept(t, s, appSubmission(two, idC, deletedAt))
	// Replay of an existing submission is still answered (no new row).
	if _, created, err := s.Accept(ctx, appSubmission(one, idA, base)); err != nil || created {
		t.Fatalf("replay: %v %v", created, err)
	}
	for _, v := range statusAll(t, s) {
		if v.Namespace.TenantID != tenantOne {
			if v.Job.State == domain.Failed {
				t.Fatalf("other restaurant touched: %+v", v)
			}
			continue
		}
		if v.Job.Channel == domain.Telegram && (v.Job.State != domain.Failed || v.Job.ErrorCode != "scope_deleted") {
			t.Fatalf("pending job of a deleted scope must fail: %+v", v)
		}
		if v.Job.Channel == domain.SMTP && v.Job.State != domain.InFlight {
			t.Fatalf("a running attempt may finish: %+v", v)
		}
	}
	// The running attempt that would be retried does not come back to life.
	retry := inFlight.Job
	retry.State, retry.ErrorCode, retry.NextAttemptAt = domain.Pending, "smtp_transient", deletedAt.Add(time.Minute)
	must(t, s.FinishScoped(ctx, one, idA, domain.SMTP, retry))
	for _, v := range statusAll(t, s) {
		if v.Namespace.TenantID == tenantOne && v.Job.State != domain.Failed {
			t.Fatalf("no job of a deleted scope may stay open: %+v", v)
		}
	}
	if err := s.Retry(ctx, one, idA, domain.SMTP, deletedAt.Add(time.Hour)); err == nil {
		t.Fatal("a deleted scope cannot be retried")
	}
	// An organization fence covers its restaurants.
	must(t, s.ApplyScopeDeleted(ctx, application.ScopeDeleted{Kind: application.ScopeOrganization, ID: orgOne, EventID: "evt-2", Revision: 1, DeletedAt: deletedAt}))
	if _, _, err := s.Accept(ctx, appSubmission(two, "123e4567-e89b-42d3-a456-426614174005", deletedAt)); !errors.Is(err, application.ErrScopeDeleted) {
		t.Fatalf("intake into a deleted organization: %v", err)
	}
}

func scopeRevision(t *testing.T, h Harness) {
	st := settings()
	s := h.Open(t, st)
	ctx := context.Background()
	one := appNamespace(orgOne, tenantOne)
	accept(t, s, appSubmission(one, idA, base))
	deletedAt := base.Add(time.Hour)
	must(t, s.ApplyScopeDeleted(ctx, application.ScopeDeleted{Kind: application.ScopeRestaurant, ID: tenantOne, EventID: "new", Revision: 9, DeletedAt: deletedAt}))
	// A stale event with an older revision and a later time does not move anything.
	must(t, s.ApplyScopeDeleted(ctx, application.ScopeDeleted{Kind: application.ScopeRestaurant, ID: tenantOne, EventID: "old", Revision: 3, DeletedAt: deletedAt.Add(72 * time.Hour)}))
	// 7 days after the deletion the submission is erased, long before its 180 days.
	if res, err := s.Sweep(ctx, deletedAt.AddDate(0, 0, 7).Add(-time.Second)); err != nil || res.Erased != 0 {
		t.Fatalf("too early: %+v %v", res, err)
	}
	if res, err := s.Sweep(ctx, deletedAt.AddDate(0, 0, 7)); err != nil || res.Erased != 1 {
		t.Fatalf("7 days after deletion: %+v %v", res, err)
	}
	if got, _ := lookupScopedOK(t, s, one, idA); !got.Erased {
		t.Fatal("not erased")
	}
}

func statusPaged(t *testing.T, h Harness) {
	s := h.Open(t, settings())
	ctx := context.Background()
	for i := range 5 {
		accept(t, s, submission(fmt.Sprintf("123e4567-e89b-42d3-a456-4266141742%02d", i), base.Add(time.Duration(i)*time.Second)))
	}
	page, err := s.Status(ctx, 0, 2)
	must(t, err)
	if len(page) != 4 || page[0].Job.Channel != domain.SMTP || page[1].Job.Channel != domain.Telegram || page[0].Seq != page[1].Seq {
		t.Fatalf("a page holds whole submissions: %+v", page)
	}
	rest, err := s.Status(ctx, page[len(page)-1].Seq, 10)
	must(t, err)
	if len(rest) != 6 || rest[0].Seq <= page[len(page)-1].Seq {
		t.Fatalf("second page: %d", len(rest))
	}
}

func lookup(t *testing.T, s Store, id domain.SubmissionID) (domain.Submission, bool) {
	t.Helper()
	sub, found, err := s.Lookup(context.Background(), id)
	must(t, err)
	return sub, found
}

func lookupScopedOK(t *testing.T, s Store, ns domain.Namespace, id domain.SubmissionID) (domain.Submission, bool) {
	t.Helper()
	sub, found, err := s.LookupScoped(context.Background(), ns, id)
	must(t, err)
	return sub, found
}
