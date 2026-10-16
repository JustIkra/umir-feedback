// Package memory is the in-memory feedback store. It exists for tests and for
// the operational fixtures (C46): production binaries never import it, because
// the service stores nothing outside PostgreSQL (R20).
package memory

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	mechanisms "github.com/JustIkra/umir-feedback/internal/adapters/runtime"
	"github.com/JustIkra/umir-feedback/internal/application"
	"github.com/JustIkra/umir-feedback/internal/domain"
)

const sweepBatch = 500

// Config configures a Store. Zero Retention and Quota values mean "no limit":
// purge never happens and the quota is unbounded.
type Config struct {
	Policy    domain.RetryPolicy
	Lease     time.Duration
	Retention application.RetentionConfig
	Quota     application.QuotaConfig
	Now       func() time.Time
	NewID     func() (string, error)
}

type entry struct {
	seq            int64
	sub            domain.Submission
	jobs           map[domain.Channel]*domain.Job
	purgeAt        time.Time
	tombstoneUntil time.Time
}

type fence struct {
	revision int64
}

// Store implements application.Store in memory.
type Store struct {
	mu      sync.Mutex
	cfg     Config
	seq     int64
	fence   int64
	entries map[domain.SubmissionID]*entry
	fences  map[application.ScopeKind]map[string]fence
	quota   map[string]int
	closed  bool
}

var _ application.Store = (*Store)(nil)

// New creates an empty store.
func New(cfg Config) *Store {
	if cfg.Lease == 0 {
		cfg.Lease = 5 * time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().In(domain.MoscowLocation()) }
	}
	if cfg.NewID == nil {
		cfg.NewID = mechanisms.NewUUIDv4
	}
	return &Store{cfg: cfg, entries: map[domain.SubmissionID]*entry{}, fences: map[application.ScopeKind]map[string]fence{application.ScopeRestaurant: {}, application.ScopeOrganization: {}}, quota: map[string]int{}}
}

// Ready reports whether the store is open.
func (s *Store) Ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed
}

// Close makes the store unavailable; it is idempotent.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// Restart simulates a process restart: in-flight jobs go back to the queue the way the lease would return them.
func (s *Store) Restart() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		for _, j := range e.jobs {
			if j.State != domain.InFlight {
				continue
			}
			if s.cfg.Policy.Valid() && s.cfg.Policy.Exhausted(j.Attempts, domain.RetryCycleStart(*j, e.sub), j.StartedAt) {
				*j = exhausted(*j, j.StartedAt)
				continue
			}
			j.State = domain.Pending
			j.NextAttemptAt = j.StartedAt.Add(s.cfg.Policy.Delay(j.Attempts))
		}
	}
}

func (s *Store) usable() error {
	if s.closed {
		return application.ErrStorageUnavailable
	}
	return nil
}

func quotaKey(ns domain.Namespace) string {
	if ns.Kind == "app" {
		return "restaurant:" + ns.TenantID
	}
	return "anonymous"
}

func (s *Store) quotaLimit(ns domain.Namespace) int {
	if ns.Kind == "app" {
		return s.cfg.Quota.Restaurant
	}
	return s.cfg.Quota.Anonymous
}

func (s *Store) fenced(ns domain.Namespace) bool {
	if ns.Kind != "app" {
		return false
	}
	_, restaurant := s.fences[application.ScopeRestaurant][ns.TenantID]
	_, organization := s.fences[application.ScopeOrganization][ns.OrgID]
	return restaurant || organization
}

func days(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

func (s *Store) Accept(ctx context.Context, sub domain.Submission) (domain.Receipt, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.Receipt{}, false, err
	}
	if sub.Namespace.Kind == "" {
		sub.Namespace = domain.Anonymous()
	}
	if sub.CommandID == "" {
		sub.CommandID = string(sub.ID)
	}
	if err := domain.ValidateSubmission(sub); err != nil {
		return domain.Receipt{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return domain.Receipt{}, false, err
	}
	key := sub.Namespace.Key(sub.ID)
	if prev, ok := s.entries[key]; ok {
		if prev.sub.Fingerprint != sub.Fingerprint {
			return domain.Receipt{}, false, application.ErrIdentityConflict
		}
		return domain.Receipt{SubmissionID: prev.sub.ID, ReceivedAt: prev.sub.ReceivedAt}, false, nil
	}
	if s.fenced(sub.Namespace) {
		return domain.Receipt{}, false, application.ErrScopeDeleted
	}
	qk := quotaKey(sub.Namespace)
	if limit := s.quotaLimit(sub.Namespace); limit > 0 && s.quota[qk] >= limit {
		return domain.Receipt{}, false, application.ErrQuotaExceeded
	}
	e := &entry{sub: sub, jobs: map[domain.Channel]*domain.Job{}}
	for _, ch := range []domain.Channel{domain.SMTP, domain.Telegram} {
		id, err := s.cfg.NewID()
		if err != nil {
			return domain.Receipt{}, false, err
		}
		e.jobs[ch] = &domain.Job{ID: id, Channel: ch, State: domain.Pending, NextAttemptAt: sub.ReceivedAt}
	}
	retention := s.cfg.Retention.AnonymousDays
	if sub.Namespace.Kind == "app" {
		retention = s.cfg.Retention.RestaurantDays
	}
	if retention > 0 {
		e.purgeAt = sub.ReceivedAt.Add(days(retention))
	}
	s.seq++
	e.seq = s.seq
	s.entries[key] = e
	s.quota[qk]++
	return domain.Receipt{SubmissionID: sub.ID, ReceivedAt: sub.ReceivedAt}, true, nil
}

func (s *Store) Lookup(ctx context.Context, id domain.SubmissionID) (domain.Submission, bool, error) {
	return s.LookupScoped(ctx, domain.Anonymous(), id)
}

func (s *Store) LookupScoped(ctx context.Context, ns domain.Namespace, id domain.SubmissionID) (domain.Submission, bool, error) {
	if !ns.Valid() {
		return domain.Submission{}, false, errors.New("invalid namespace")
	}
	if err := ctx.Err(); err != nil {
		return domain.Submission{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return domain.Submission{}, false, err
	}
	e, ok := s.entries[ns.Key(id)]
	if !ok {
		return domain.Submission{}, false, nil
	}
	return e.sub, true, nil
}

func (s *Store) ListScoped(ctx context.Context, org, tenant string, after application.ListPosition, limit int) ([]application.ListedSubmission, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit < 1 {
		return nil, errors.New("list limit must be positive")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return nil, err
	}
	rows := []*entry{}
	for _, e := range s.entries {
		n := e.sub.Namespace
		if n.Kind != "app" || n.OrgID != org || n.TenantID != tenant || e.sub.Erased {
			continue
		}
		if !after.ReceivedAt.IsZero() || after.Seq != 0 {
			if e.sub.ReceivedAt.Before(after.ReceivedAt) || (e.sub.ReceivedAt.Equal(after.ReceivedAt) && e.seq <= after.Seq) {
				continue
			}
		}
		rows = append(rows, e)
	}
	slices.SortFunc(rows, func(a, b *entry) int {
		if c := a.sub.ReceivedAt.Compare(b.sub.ReceivedAt); c != 0 {
			return c
		}
		return int(a.seq - b.seq)
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	out := make([]application.ListedSubmission, 0, len(rows))
	for _, e := range rows {
		out = append(out, application.ListedSubmission{Submission: e.sub, Position: application.ListPosition{ReceivedAt: e.sub.ReceivedAt, Seq: e.seq}})
	}
	return out, nil
}

func exhausted(job domain.Job, now time.Time) domain.Job {
	job.State = domain.Failed
	job.ErrorCode = "retry_exhausted"
	job.NextAttemptAt = time.Time{}
	job.ConfirmedAt = now
	return job
}

func (s *Store) sorted() []*entry {
	list := make([]*entry, 0, len(s.entries))
	for _, e := range s.entries {
		list = append(list, e)
	}
	slices.SortFunc(list, func(a, b *entry) int {
		if c := a.sub.ReceivedAt.Compare(b.sub.ReceivedAt); c != 0 {
			return c
		}
		return int(a.seq - b.seq)
	})
	return list
}

// Claim leases the oldest due job of the channel; see the PostgreSQL store for the rules, which are the same.
func (s *Store) Claim(ctx context.Context, channel domain.Channel, now time.Time, policy domain.RetryPolicy) (domain.Delivery, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.Delivery{}, false, err
	}
	if !domain.ValidChannel(channel) || !policy.Valid() {
		return domain.Delivery{}, false, errors.New("invalid delivery channel or retry policy")
	}
	if !domain.IsMoscowTime(now) {
		return domain.Delivery{}, false, errors.New("claim time must use Europe/Moscow")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return domain.Delivery{}, false, err
	}
	for _, e := range s.sorted() {
		if e.sub.Erased {
			continue
		}
		job := e.jobs[channel]
		cycle := domain.RetryCycleStart(*job, e.sub)
		switch {
		case job.State == domain.InFlight && !job.StartedAt.After(now.Add(-s.cfg.Lease)):
			if policy.Exhausted(job.Attempts, cycle, now) {
				*job = exhausted(*job, now)
				continue
			}
			job.State = domain.Pending
			job.NextAttemptAt = job.StartedAt.Add(policy.Delay(job.Attempts))
			if job.NextAttemptAt.After(now) {
				continue
			}
		case job.State == domain.Pending && policy.Exhausted(job.Attempts, cycle, now):
			*job = exhausted(*job, now)
			continue
		case job.State == domain.Pending && !job.NextAttemptAt.After(now):
		default:
			continue
		}
		s.fence++
		job.State = domain.InFlight
		job.Attempts++
		job.StartedAt = now
		job.NextAttemptAt = time.Time{}
		job.ConfirmedAt = time.Time{}
		job.ErrorCode = ""
		job.ProviderID = ""
		job.Fence = s.fence
		return domain.Delivery{Submission: e.sub, Job: *job}, true, nil
	}
	return domain.Delivery{}, false, nil
}

func (s *Store) Finish(ctx context.Context, id domain.SubmissionID, channel domain.Channel, job domain.Job) error {
	return s.FinishScoped(ctx, domain.Anonymous(), id, channel, job)
}

func (s *Store) FinishScoped(ctx context.Context, ns domain.Namespace, id domain.SubmissionID, channel domain.Channel, job domain.Job) error {
	if !ns.Valid() || !domain.ValidChannel(channel) {
		return errors.New("invalid delivery state transition")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return err
	}
	e, ok := s.entries[ns.Key(id)]
	if !ok || e.sub.Erased {
		return application.ErrNotFound
	}
	current := e.jobs[channel]
	if job.ID == "" {
		job.ID = current.ID
	}
	if job.ID != current.ID {
		return errors.New("job identity mismatch")
	}
	if current.State != domain.InFlight || job.Fence != current.Fence {
		return application.ErrLeaseLost
	}
	if job.Channel != channel || !domain.ValidFinish(*current, job) || !domain.ValidJobTimeline(job, e.sub) {
		return errors.New("invalid delivery state transition")
	}
	if job.State == domain.Pending && s.fenced(e.sub.Namespace) {
		job.State = domain.Failed
		job.ErrorCode = "scope_deleted"
		job.NextAttemptAt = time.Time{}
		job.ConfirmedAt = s.cfg.Now()
		if job.ConfirmedAt.Before(job.StartedAt) {
			job.ConfirmedAt = job.StartedAt
		}
	}
	job.Fence = current.Fence
	*current = job
	s.clearPayload(e)
	return nil
}

// clearPayload drops the text and the phone once both channels reported sent (C24).
func (s *Store) clearPayload(e *entry) {
	if e.sub.Erased || e.jobs[domain.SMTP].State != domain.Sent || e.jobs[domain.Telegram].State != domain.Sent {
		return
	}
	e.sub.Message, e.sub.Phone = "", ""
}

func (s *Store) Retry(ctx context.Context, ns domain.Namespace, id domain.SubmissionID, channel domain.Channel, now time.Time) error {
	if !ns.Valid() || !domain.ValidChannel(channel) {
		return application.ErrNotRetryable
	}
	if !domain.IsMoscowTime(now) {
		return errors.New("retry time must use Europe/Moscow")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return err
	}
	e, ok := s.entries[ns.Key(id)]
	if !ok || e.sub.Erased {
		return application.ErrNotFound
	}
	job := e.jobs[channel]
	if job.State != domain.Failed || s.fenced(e.sub.Namespace) {
		return application.ErrNotRetryable
	}
	next := *job
	next.State = domain.Pending
	next.Attempts = 0
	next.NextAttemptAt = now
	next.RetryStartedAt = now
	next.StartedAt = time.Time{}
	next.ConfirmedAt = time.Time{}
	next.ErrorCode = ""
	next.ProviderID = ""
	if domain.ValidateJob(next) != nil || !domain.ValidTransition(*job, next) || !domain.ValidJobTimeline(next, e.sub) {
		return errors.New("delivery job timeline is invalid")
	}
	*job = next
	return nil
}

func (s *Store) Status(ctx context.Context, afterSeq int64, limit int) ([]application.DeliveryView, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit < 1 {
		return nil, errors.New("status limit must be positive")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return nil, err
	}
	list := []*entry{}
	for _, e := range s.entries {
		if e.seq > afterSeq {
			list = append(list, e)
		}
	}
	slices.SortFunc(list, func(a, b *entry) int { return int(a.seq - b.seq) })
	if len(list) > limit {
		list = list[:limit]
	}
	views := []application.DeliveryView{}
	for _, e := range list {
		for _, ch := range []domain.Channel{domain.SMTP, domain.Telegram} {
			views = append(views, application.DeliveryView{Seq: e.seq, Namespace: e.sub.Namespace, SubmissionID: e.sub.ID, ReceivedAt: e.sub.ReceivedAt, Erased: e.sub.Erased, Job: *e.jobs[ch]})
		}
	}
	return views, nil
}

func (s *Store) EraseEmail(ctx context.Context, email string, now time.Time) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return 0, err
	}
	n := 0
	for _, e := range s.entries {
		if !e.sub.Erased && e.sub.Email == email {
			s.erase(e, now)
			n++
		}
	}
	return n, nil
}

// erase removes the personal fields, keeps identity, receipt and fingerprint as the digest tombstone, and ends open jobs.
func (s *Store) erase(e *entry, now time.Time) {
	e.sub.Name, e.sub.Email, e.sub.Phone, e.sub.Message = "", "", "", ""
	e.sub.Erased = true
	e.tombstoneUntil = now.Add(days(s.cfg.Retention.TombstoneDays))
	if k := quotaKey(e.sub.Namespace); s.quota[k] > 0 {
		s.quota[k]--
	}
	for _, j := range e.jobs {
		if j.State == domain.Pending || j.State == domain.InFlight {
			j.State = domain.Failed
			j.ErrorCode = "retention_expired"
			j.NextAttemptAt = time.Time{}
			j.ConfirmedAt = now
			if j.ConfirmedAt.Before(j.StartedAt) {
				j.ConfirmedAt = j.StartedAt
			}
		}
	}
}

func (s *Store) leaseHeld(e *entry, now time.Time) bool {
	for _, j := range e.jobs {
		if j.State == domain.InFlight && j.StartedAt.After(now.Add(-s.cfg.Lease)) {
			return true
		}
	}
	return false
}

func (s *Store) Sweep(ctx context.Context, now time.Time) (application.SweepResult, error) {
	if err := ctx.Err(); err != nil {
		return application.SweepResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return application.SweepResult{}, err
	}
	var result application.SweepResult
	for _, e := range s.sorted() {
		switch {
		case !e.sub.Erased && !e.purgeAt.IsZero() && !e.purgeAt.After(now) && !s.leaseHeld(e, now) && result.Erased < sweepBatch:
			s.erase(e, now)
			result.Erased++
		case e.sub.Erased && !e.tombstoneUntil.IsZero() && !e.tombstoneUntil.After(now) && result.Deleted < sweepBatch:
			delete(s.entries, e.sub.Namespace.Key(e.sub.ID))
			result.Deleted++
		}
	}
	return result, nil
}

func (s *Store) ApplyScopeDeleted(ctx context.Context, ev application.ScopeDeleted) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ev.Kind != application.ScopeRestaurant && ev.Kind != application.ScopeOrganization {
		return errors.New("unknown scope kind")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return err
	}
	if prev, ok := s.fences[ev.Kind][ev.ID]; ok && prev.revision > ev.Revision {
		return nil
	}
	s.fences[ev.Kind][ev.ID] = fence{revision: ev.Revision}
	for _, e := range s.entries {
		n := e.sub.Namespace
		inScope := n.Kind == "app" && ((ev.Kind == application.ScopeRestaurant && n.TenantID == ev.ID) || (ev.Kind == application.ScopeOrganization && n.OrgID == ev.ID))
		if !inScope || e.sub.Erased {
			continue
		}
		if limit := ev.DeletedAt.Add(days(s.cfg.Retention.DeletedScopeDays)); s.cfg.Retention.DeletedScopeDays > 0 && (e.purgeAt.IsZero() || limit.Before(e.purgeAt)) {
			e.purgeAt = limit
		}
		for _, j := range e.jobs {
			if j.State == domain.Pending {
				j.State = domain.Failed
				j.ErrorCode = "scope_deleted"
				j.NextAttemptAt = time.Time{}
				j.ConfirmedAt = ev.DeletedAt
				if j.ConfirmedAt.Before(j.StartedAt) {
					j.ConfirmedAt = j.StartedAt
				}
			}
		}
	}
	return nil
}

// Observe is the read-only projection the operational fixtures expose.
func (s *Store) Observe(ctx context.Context, selectors []Selector) ([]RecordView, bool, error) {
	if err := ctx.Err(); err != nil {
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
		e, ok := s.entries[p.Namespace.Key(p.ID)]
		if !ok {
			continue
		}
		row := RecordView{SubmissionID: e.sub.ID, Namespace: e.sub.Namespace, CommandID: e.sub.CommandID, Fingerprint: e.sub.Fingerprint, ReceivedAt: e.sub.ReceivedAt, Jobs: make([]JobView, 0, 2)}
		for _, ch := range []domain.Channel{domain.SMTP, domain.Telegram} {
			j := e.jobs[ch]
			row.Jobs = append(row.Jobs, JobView{JobID: j.ID, Channel: j.Channel, State: j.State, Attempts: j.Attempts, AcceptedAt: e.sub.ReceivedAt})
		}
		rows = append(rows, row)
	}
	return rows, len(rows) == len(selectors), nil
}

// Selector names one submission for Observe.
type Selector struct {
	Namespace domain.Namespace
	ID        domain.SubmissionID
}

// JobView is one job in an observation.
type JobView struct {
	JobID      string          `json:"jobID"`
	Channel    domain.Channel  `json:"channel"`
	State      domain.JobState `json:"state"`
	Attempts   int             `json:"attempts"`
	AcceptedAt time.Time       `json:"acceptedAt"`
}

// RecordView is one submission in an observation.
type RecordView struct {
	SubmissionID domain.SubmissionID `json:"submissionID"`
	Namespace    domain.Namespace    `json:"namespace"`
	CommandID    string              `json:"commandID"`
	Fingerprint  string              `json:"fingerprint"`
	ReceivedAt   time.Time           `json:"receivedAt"`
	Jobs         []JobView           `json:"jobs"`
}
