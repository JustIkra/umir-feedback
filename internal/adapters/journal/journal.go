package journal

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/JustIkra/umir-feedback/internal/application"
	"github.com/JustIkra/umir-feedback/internal/domain"
)

const (
	formatVersion     = 1
	scopedVersion     = 2
	maxIdentifierSize = 128
	maxErrorCodeSize  = domain.MaxErrorCodeSize
	maxProviderSize   = domain.MaxProviderSize
	maxChallengeCount = 4096
	compactTempPrefix = ".feedback-compact-"
)

var errCapacity = errors.New("feedback journal capacity exhausted")

type Config struct {
	Path     string
	MaxBytes int64
	UID      int
	GID      int
	Policy   domain.RetryPolicy
	Now      func() time.Time
}

type Store struct {
	mu                   sync.Mutex
	config               Config
	file                 *os.File
	lock                 *os.File
	entries              map[domain.SubmissionID]*entry
	jobOwners            map[string]domain.SubmissionID
	challenges           map[legacyChallengeID]legacyEmailChallenge
	bytes                int64
	closed               bool
	failed               bool
	challengeScrubNeeded bool
}

type entry struct {
	submission domain.Submission
	jobs       map[domain.Channel]domain.Job
}

type record struct {
	Version      int               `json:"version"`
	Type         string            `json:"type"`
	Submission   *wireSubmission   `json:"submission,omitempty"`
	Jobs         []wireJob         `json:"jobs,omitempty"`
	SubmissionID string            `json:"submissionID,omitempty"`
	Namespace    *domain.Namespace `json:"namespace,omitempty"`
	Job          *wireJob          `json:"job,omitempty"`
	Challenge    *wireChallenge    `json:"challenge,omitempty"`
}

type wireSubmission struct {
	Namespace      *domain.Namespace `json:"namespace,omitempty"`
	CommandID      string            `json:"commandID,omitempty"`
	Legacy         bool              `json:"legacy,omitempty"`
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	Email          string            `json:"email"`
	Phone          string            `json:"phone"`
	Message        string            `json:"message"`
	ConsentVersion string            `json:"consentVersion"`
	ConsentPurpose string            `json:"consentPurpose"`
	AcceptedAt     time.Time         `json:"acceptedAt"`
	ReceivedAt     time.Time         `json:"receivedAt"`
	Fingerprint    string            `json:"fingerprint"`
}

type wireJob struct {
	ID             string    `json:"jobID,omitempty"`
	Channel        string    `json:"channel"`
	State          string    `json:"state"`
	Attempts       int       `json:"attempts"`
	NextAttemptAt  time.Time `json:"nextAttemptAt,omitempty,omitzero"`
	RetryStartedAt time.Time `json:"retryStartedAt,omitempty,omitzero"`
	StartedAt      time.Time `json:"startedAt,omitempty,omitzero"`
	ConfirmedAt    time.Time `json:"confirmedAt,omitempty,omitzero"`
	ErrorCode      string    `json:"errorCode,omitempty"`
	ProviderID     string    `json:"providerID,omitempty"`
}

func Open(config Config) (*Store, error) {
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	config.Path = filepath.Clean(config.Path)
	if config.Now == nil {
		config.Now = time.Now
	}
	if err := validateParent(config.Path, config.UID, config.GID); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(filepath.Dir(config.Path), "feedback.lock")
	_, lockStatErr := os.Lstat(lockPath)
	lockCreated := errors.Is(lockStatErr, os.ErrNotExist)
	lock, err := openSecureFile(lockPath, config.UID, config.GID, true)
	if err != nil {
		return nil, fmt.Errorf("open journal lock: %w", err)
	}
	if lockCreated {
		if err := syncDirectory(filepath.Dir(config.Path)); err != nil {
			return nil, errors.Join(fmt.Errorf("sync journal lock directory: %w", err), closeLock(lock))
		}
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		closeErr := lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errors.Join(application.ErrBusy, closeErr)
		}
		return nil, errors.Join(fmt.Errorf("lock journal: %w", err), closeErr)
	}
	if err := cleanupCompactionTemps(filepath.Dir(config.Path), config.UID, config.GID); err != nil {
		return nil, errors.Join(fmt.Errorf("clean abandoned compaction files: %w", err), closeLock(lock))
	}
	_, journalStatErr := os.Lstat(config.Path)
	journalCreated := errors.Is(journalStatErr, os.ErrNotExist)
	file, err := openSecureFile(config.Path, config.UID, config.GID, true)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("open journal: %w", err), closeLock(lock))
	}
	if journalCreated {
		if err := syncDirectory(filepath.Dir(config.Path)); err != nil {
			return nil, errors.Join(fmt.Errorf("sync journal directory: %w", err), releaseFiles(file, lock))
		}
	}
	store := &Store{config: config, file: file, lock: lock, entries: make(map[domain.SubmissionID]*entry), jobOwners: make(map[string]domain.SubmissionID), challenges: make(map[legacyChallengeID]legacyEmailChallenge)}
	if err := store.recover(); err != nil {
		return nil, errors.Join(err, store.closeFiles())
	}
	if err := store.recoverInFlight(); err != nil {
		return nil, errors.Join(err, store.closeFiles())
	}
	return store, nil
}

func validateConfig(config Config) error {
	if !filepath.IsAbs(config.Path) || config.Path == string(filepath.Separator) {
		return errors.New("journal path must be an absolute file path")
	}
	if config.MaxBytes <= 0 {
		return errors.New("journal maximum size must be positive")
	}
	if config.UID < 0 || config.GID < 0 {
		return errors.New("journal owner must be non-negative")
	}
	if config.UID != os.Getuid() || config.GID != os.Getgid() {
		return errors.New("journal owner must match the process owner")
	}
	if !config.Policy.Valid() {
		return errors.New("journal retry policy is invalid")
	}
	return nil
}

func validateParent(path string, uid, gid int) error {
	dir := filepath.Dir(path)
	canonicalDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("resolve journal directory: %w", err)
	}
	if filepath.Clean(canonicalDir) != dir {
		return errors.New("journal path ancestors must not contain symlinks")
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect journal directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("journal parent must be a real directory")
	}
	if info.Mode().Perm() != 0o700 {
		return errors.New("journal directory must have mode 0700")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != uid || int(stat.Gid) != gid {
		return errors.New("journal directory owner does not match configured owner")
	}
	return nil
}

func openSecureFile(path string, uid, gid int, create bool) (*os.File, error) {
	flags := syscall.O_RDWR | syscall.O_CLOEXEC | syscall.O_NOFOLLOW
	if create {
		flags |= syscall.O_CREAT
	}
	fd, err := syscall.Open(path, flags, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || int(stat.Uid) != uid || int(stat.Gid) != gid {
		return nil, errors.Join(errors.New("journal file must be regular, mode 0600, and owned by configured UID/GID"), file.Close())
	}
	return file, nil
}

func (s *Store) Accept(ctx context.Context, submission domain.Submission) (domain.Receipt, bool, error) {
	if err := contextError(ctx); err != nil {
		return domain.Receipt{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return domain.Receipt{}, false, err
	}
	if submission.Namespace.Kind == "" {
		submission.Namespace = domain.Anonymous()
	}
	if submission.CommandID == "" {
		submission.CommandID = string(submission.ID)
	}
	if err := domain.ValidateSubmission(submission); err != nil {
		return domain.Receipt{}, false, err
	}
	if existing, ok := s.entries[submission.Namespace.Key(submission.ID)]; ok {
		if domain.SameContent(existing.submission, submission) {
			return domain.Receipt{SubmissionID: submission.ID, ReceivedAt: existing.submission.ReceivedAt}, false, nil
		}
		return domain.Receipt{}, false, application.ErrIdentityConflict
	}
	smtpID, err := newJobID()
	if err != nil {
		return domain.Receipt{}, false, err
	}
	telegramID, err := newJobID()
	if err != nil {
		return domain.Receipt{}, false, err
	}
	initial := &entry{submission: submission, jobs: map[domain.Channel]domain.Job{
		domain.SMTP:     {ID: smtpID, Channel: domain.SMTP, State: domain.Pending, NextAttemptAt: submission.ReceivedAt},
		domain.Telegram: {ID: telegramID, Channel: domain.Telegram, State: domain.Pending, NextAttemptAt: submission.ReceivedAt},
	}}
	rec := record{Version: scopedVersion, Type: "accept", Submission: toWireSubmission(submission), Jobs: []wireJob{toWireJob(initial.jobs[domain.SMTP]), toWireJob(initial.jobs[domain.Telegram])}}
	line, err := encodeRecord(rec)
	if err != nil {
		return domain.Receipt{}, false, err
	}
	if err := s.append(line, initial, 0); err != nil {
		return domain.Receipt{}, false, err
	}
	return domain.Receipt{SubmissionID: submission.ID, ReceivedAt: submission.ReceivedAt}, true, nil
}

func (s *Store) Lookup(ctx context.Context, id domain.SubmissionID) (domain.Submission, bool, error) {
	return s.LookupScoped(ctx, domain.Anonymous(), id)
}

func (s *Store) LookupScoped(ctx context.Context, ns domain.Namespace, id domain.SubmissionID) (domain.Submission, bool, error) {
	if !ns.Valid() {
		return domain.Submission{}, false, errors.New("invalid namespace")
	}
	if err := contextError(ctx); err != nil {
		return domain.Submission{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return domain.Submission{}, false, err
	}
	item, ok := s.entries[ns.Key(id)]
	if !ok {
		return domain.Submission{}, false, nil
	}
	return item.submission, true, nil
}

func (s *Store) Claim(ctx context.Context, channel domain.Channel, now time.Time, policy domain.RetryPolicy) (domain.Delivery, bool, error) {
	if err := contextError(ctx); err != nil {
		return domain.Delivery{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return domain.Delivery{}, false, err
	}
	if !domain.ValidChannel(channel) || !policy.Valid() || policy != s.config.Policy {
		return domain.Delivery{}, false, errors.New("invalid delivery channel or retry policy")
	}
	if !domain.IsMoscowTime(now) {
		return domain.Delivery{}, false, errors.New("claim time must use Europe/Moscow")
	}
	items := sortedEntries(s.entries)
	for _, item := range items {
		job := item.jobs[channel]
		if job.State != domain.Pending {
			continue
		}
		if policy.Exhausted(job.Attempts, domain.RetryCycleStart(job, item.submission), now) {
			job.State = domain.Failed
			job.ErrorCode = "retry_exhausted"
			job.NextAttemptAt = time.Time{}
			job.ConfirmedAt = now
			if err := s.appendState(item, job); err != nil {
				return domain.Delivery{}, false, err
			}
			continue
		}
		if job.NextAttemptAt.After(now) {
			continue
		}
		job.State = domain.InFlight
		job.Attempts++
		job.StartedAt = now
		job.NextAttemptAt = time.Time{}
		job.ConfirmedAt = time.Time{}
		job.ErrorCode = ""
		job.ProviderID = ""
		if err := s.appendState(item, job); err != nil {
			return domain.Delivery{}, false, err
		}
		return domain.Delivery{Submission: item.submission, Job: job}, true, nil
	}
	return domain.Delivery{}, false, nil
}

func (s *Store) Finish(ctx context.Context, id domain.SubmissionID, channel domain.Channel, job domain.Job) error {
	return s.FinishScoped(ctx, domain.Anonymous(), id, channel, job)
}

func (s *Store) FinishScoped(ctx context.Context, ns domain.Namespace, id domain.SubmissionID, channel domain.Channel, job domain.Job) error {
	if !ns.Valid() {
		return errors.New("invalid namespace")
	}
	if err := contextError(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return err
	}
	item, ok := s.entries[ns.Key(id)]
	if !ok {
		return application.ErrNotFound
	}
	current := item.jobs[channel]
	if job.ID == "" {
		job.ID = current.ID
	}
	if job.ID != current.ID {
		return errors.New("job identity mismatch")
	}
	if !domain.ValidChannel(channel) || current.State != domain.InFlight || job.Channel != channel || job.Attempts != current.Attempts || !domain.ValidFinish(current, job) || !domain.ValidJobTimeline(job, item.submission) {
		return errors.New("invalid delivery state transition")
	}
	if err := domain.ValidateJob(job); err != nil {
		return err
	}
	return s.appendState(item, job)
}

func (s *Store) Export(ctx context.Context, since time.Time) ([]domain.Submission, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return nil, err
	}
	items := sortedEntries(s.entries)
	result := make([]domain.Submission, 0, len(items))
	for _, item := range items {
		if item.submission.Namespace.Kind == "anonymous" && (since.IsZero() || !item.submission.ReceivedAt.Before(since)) {
			result = append(result, item.submission)
		}
	}
	return result, nil
}

func (s *Store) DeleteEmail(ctx context.Context, email string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return err
	}
	kept := make(map[domain.SubmissionID]*entry, len(s.entries))
	normalizedEmail := domain.NormalizeEmail(email)
	for id, item := range s.entries {
		if item.submission.Namespace.Kind == "app" || item.submission.Email != normalizedEmail {
			kept[id] = item
		}
	}
	if len(kept) == len(s.entries) {
		return nil
	}
	return s.compact(kept, s.challenges)
}

func (s *Store) Purge(ctx context.Context, before time.Time) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return err
	}
	kept := make(map[domain.SubmissionID]*entry, len(s.entries))
	for id, item := range s.entries {
		if item.submission.Namespace.Kind == "app" || !item.submission.ReceivedAt.Before(before) {
			kept[id] = item
		}
	}
	if len(kept) == len(s.entries) {
		return nil
	}
	return s.compact(kept, s.challenges)
}

func (s *Store) Status(ctx context.Context) ([]domain.Delivery, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return nil, err
	}
	items := sortedEntries(s.entries)
	result := make([]domain.Delivery, 0, len(items)*2)
	for _, item := range items {
		for _, channel := range []domain.Channel{domain.SMTP, domain.Telegram} {
			result = append(result, domain.Delivery{Submission: item.submission, Job: item.jobs[channel]})
		}
	}
	return result, nil
}

func (s *Store) Retry(ctx context.Context, id domain.SubmissionID, channel domain.Channel, now time.Time) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return err
	}
	item, ok := s.entries[id]
	if !ok {
		return application.ErrNotFound
	}
	job := item.jobs[channel]
	if !domain.ValidChannel(channel) || job.State != domain.Failed {
		return errors.New("only a failed delivery can be retried")
	}
	if !domain.IsMoscowTime(now) {
		return errors.New("retry time must use Europe/Moscow")
	}
	job.State = domain.Pending
	job.Attempts = 0
	job.NextAttemptAt = now
	job.RetryStartedAt = now
	job.StartedAt = time.Time{}
	job.ConfirmedAt = time.Time{}
	job.ErrorCode = ""
	job.ProviderID = ""
	if err := domain.ValidateJob(job); err != nil {
		return err
	}
	return s.appendState(item, job)
}

func (s *Store) Ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed && !s.failed && s.file != nil && s.lock != nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.closeFiles()
}

func (s *Store) closeFiles() error {
	var result error
	if s.file != nil {
		if err := s.file.Close(); err != nil {
			result = errors.Join(result, err)
		}
		s.file = nil
	}
	if s.lock != nil {
		if err := syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN); err != nil {
			result = errors.Join(result, err)
		}
		if err := s.lock.Close(); err != nil {
			result = errors.Join(result, err)
		}
		s.lock = nil
	}
	return result
}

func (s *Store) recover() error {
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	data, err := io.ReadAll(s.file)
	if err != nil {
		return fmt.Errorf("read journal: %w", err)
	}
	if int64(len(data)) > s.config.MaxBytes {
		return errors.New("journal exceeds configured maximum size")
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		boundary := bytes.LastIndexByte(data, '\n') + 1
		if err := s.file.Truncate(int64(boundary)); err != nil {
			return fmt.Errorf("repair incomplete journal tail: %w", err)
		}
		if err := s.file.Sync(); err != nil {
			return fmt.Errorf("sync repaired journal tail: %w", err)
		}
		data = data[:boundary]
	}
	if len(data) == 0 {
		header, err := encodeRecord(record{Version: formatVersion, Type: "header"})
		if err != nil {
			return err
		}
		if err := s.append(header, nil, s.reservedBytes()); err != nil {
			return err
		}
		return nil
	}
	lines := bytes.Split(data[:len(data)-1], []byte{'\n'})
	for index, line := range lines {
		var rec record
		if err := decodeStrict(line, &rec); err != nil {
			return fmt.Errorf("journal record %d is invalid: %w", index+1, err)
		}
		if rec.Version != formatVersion && rec.Version != scopedVersion {
			return fmt.Errorf("journal record %d has unsupported version", index+1)
		}
		if err := validateRecordFields(line, rec.Type); err != nil {
			return fmt.Errorf("journal record %d is incomplete: %w", index+1, err)
		}
		if rec.Version == scopedVersion {
			if err := validateScopedRecord(line, rec); err != nil {
				return err
			}
		} else {
			if rec.Submission != nil && rec.Submission.Namespace != nil && rec.Submission.Namespace.Kind != "anonymous" {
				return errors.New("legacy record cannot acquire app scope")
			}
			if rec.Namespace != nil && rec.Namespace.Kind != "anonymous" {
				return errors.New("legacy state cannot acquire app scope")
			}
		}
		if err := s.applyRecord(rec, index == 0); err != nil {
			return fmt.Errorf("journal record %d is invalid: %w", index+1, err)
		}
	}
	s.bytes = int64(len(data))
	if _, err := s.file.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	if !withinCapacityEnvelope(s.bytes, s.reservedBytes(), s.config.MaxBytes) {
		return errors.New("journal and reserved delivery capacity exceed configured maximum size")
	}
	return nil
}

func validateRecordFields(data []byte, recordType string) error {
	if recordType == "challenge" && len(data)+1 > maxChallengeRecordBytes {
		return errors.New("challenge snapshot exceeds its maximum size")
	}
	var requiredTop []string
	var allowedTop []string
	switch recordType {
	case "header":
		requiredTop = []string{"version", "type"}
		allowedTop = requiredTop
	case "accept", "snapshot":
		requiredTop = []string{"version", "type", "submission", "jobs"}
		allowedTop = requiredTop
	case "state":
		requiredTop = []string{"version", "type", "submissionID", "job"}
		allowedTop = append(append([]string{}, requiredTop...), "namespace")
	case "challenge":
		requiredTop = []string{"version", "type", "challenge"}
		allowedTop = requiredTop
	default:
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return err
	}
	if err := validateObjectKeys(object, allowedTop, requiredTop); err != nil {
		return err
	}
	if recordType == "accept" || recordType == "snapshot" {
		var submission map[string]json.RawMessage
		if err := json.Unmarshal(object["submission"], &submission); err != nil {
			return err
		}
		submissionKeys := []string{"id", "name", "email", "phone", "message", "consentVersion", "consentPurpose", "acceptedAt", "receivedAt", "fingerprint"}
		if err := validateObjectKeys(submission, append(append([]string{}, submissionKeys...), "namespace", "commandID", "legacy"), submissionKeys); err != nil {
			return fmt.Errorf("submission: %w", err)
		}
		var jobs []json.RawMessage
		if err := json.Unmarshal(object["jobs"], &jobs); err != nil {
			return err
		}
		for _, rawJob := range jobs {
			if err := validateJobFields(rawJob); err != nil {
				return err
			}
		}
	}
	if recordType == "state" {
		return validateJobFields(object["job"])
	}
	if recordType == "challenge" {
		return validateChallengeFields(object["challenge"])
	}
	return nil
}

func validateJobFields(data []byte) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return err
	}
	required := []string{"channel", "state", "attempts"}
	allowed := []string{"jobID", "channel", "state", "attempts", "nextAttemptAt", "retryStartedAt", "startedAt", "confirmedAt", "errorCode", "providerID"}
	return validateObjectKeys(object, allowed, required)
}

func validateObjectKeys(object map[string]json.RawMessage, allowed, required []string) error {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allowedSet[key] = struct{}{}
	}
	for key := range object {
		if _, ok := allowedSet[key]; !ok {
			return fmt.Errorf("unexpected field %q", key)
		}
	}
	for _, key := range required {
		if _, ok := object[key]; !ok {
			return fmt.Errorf("required field %q is missing", key)
		}
	}
	return nil
}

func (s *Store) applyRecord(rec record, first bool) error {
	if first {
		if rec.Type != "header" || rec.Submission != nil || len(rec.Jobs) != 0 || rec.Job != nil || rec.Challenge != nil || rec.SubmissionID != "" {
			return errors.New("first record must be an empty header")
		}
		return nil
	}
	switch rec.Type {
	case "accept":
		if rec.Submission == nil || rec.Job != nil || rec.SubmissionID != "" || len(rec.Jobs) != 2 {
			return errors.New("accept record is incomplete")
		}
		submission := fromWireSubmission(*rec.Submission)
		if err := domain.ValidateSubmission(submission); err != nil {
			return err
		}
		if _, exists := s.entries[submission.Namespace.Key(submission.ID)]; exists {
			return errors.New("duplicate submission ID")
		}
		jobs := make(map[domain.Channel]domain.Job, 2)
		for _, wire := range rec.Jobs {
			job := fromWireJob(wire)
			if rec.Version == formatVersion {
				job.ID = legacyJobID(submission, job.Channel)
			}
			if !domain.ValidChannel(job.Channel) || job.State != domain.Pending || job.Attempts != 0 || !job.NextAttemptAt.Equal(submission.ReceivedAt) || job.RetryStartedAt != (time.Time{}) || job.StartedAt != (time.Time{}) || job.ConfirmedAt != (time.Time{}) || job.ErrorCode != "" || job.ProviderID != "" {
				return errors.New("accept record contains an invalid initial job")
			}
			if _, exists := jobs[job.Channel]; exists {
				return errors.New("accept record has duplicate channels")
			}
			jobs[job.Channel] = job
		}
		if _, ok := jobs[domain.SMTP]; !ok {
			return errors.New("accept record is missing SMTP job")
		}
		if _, ok := jobs[domain.Telegram]; !ok {
			return errors.New("accept record is missing Telegram job")
		}
		if e := s.registerJobs(submission, jobs); e != nil {
			return e
		}
		s.entries[submission.Namespace.Key(submission.ID)] = &entry{submission: submission, jobs: jobs}
		return nil
	case "state":
		if rec.Submission != nil || len(rec.Jobs) != 0 || rec.Job == nil || rec.SubmissionID == "" {
			return errors.New("state record is incomplete")
		}
		ns := domain.Anonymous()
		if rec.Namespace != nil {
			ns = *rec.Namespace
		}
		if !ns.Valid() {
			return errors.New("invalid state namespace")
		}
		id := ns.Key(domain.SubmissionID(rec.SubmissionID))
		item, exists := s.entries[id]
		if !exists {
			return errors.New("state record refers to unknown submission")
		}
		job := fromWireJob(*rec.Job)
		if rec.Version == formatVersion {
			job.ID = legacyJobID(item.submission, job.Channel)
		}
		if job.ID != item.jobs[job.Channel].ID {
			return errors.New("job identity changed")
		}
		if !domain.ValidChannel(job.Channel) || !domain.ValidTransition(item.jobs[job.Channel], job) || !domain.ValidJobTimeline(job, item.submission) {
			return errors.New("impossible delivery state transition")
		}
		item.jobs[job.Channel] = job
		return nil
	case "snapshot":
		if rec.Submission == nil || rec.Job != nil || rec.SubmissionID != "" || len(rec.Jobs) != 2 {
			return errors.New("snapshot record is incomplete")
		}
		submission := fromWireSubmission(*rec.Submission)
		if err := domain.ValidateSubmission(submission); err != nil {
			return err
		}
		jobs := make(map[domain.Channel]domain.Job, 2)
		for _, wire := range rec.Jobs {
			job := fromWireJob(wire)
			if rec.Version == formatVersion {
				job.ID = legacyJobID(submission, job.Channel)
			}
			if !domain.ValidChannel(job.Channel) || domain.ValidateJob(job) != nil || !domain.ValidJobTimeline(job, submission) {
				return errors.New("snapshot contains an invalid job")
			}
			if _, exists := jobs[job.Channel]; exists {
				return errors.New("snapshot has duplicate channels")
			}
			jobs[job.Channel] = job
		}
		if len(jobs) != 2 {
			return errors.New("snapshot must contain both channels")
		}
		if _, exists := s.entries[submission.Namespace.Key(submission.ID)]; exists {
			return errors.New("duplicate snapshot ID")
		}
		if err := s.registerJobs(submission, jobs); err != nil {
			return err
		}
		s.entries[submission.Namespace.Key(submission.ID)] = &entry{submission: submission, jobs: jobs}
		return nil
	case "challenge":
		if rec.Submission != nil || len(rec.Jobs) != 0 || rec.Job != nil || rec.SubmissionID != "" || rec.Challenge == nil {
			return errors.New("challenge record is incomplete")
		}
		challenge := fromWireChallenge(*rec.Challenge)
		if err := validateChallengeSnapshot(challenge); err != nil {
			return err
		}
		if current, exists := s.challenges[challenge.ID]; exists {
			if !sameChallengeBinding(current, challenge) {
				return errLegacyChallengeBinding
			}
			if hasCiphertext(current.EncryptedCode) && !hasCiphertext(challenge.EncryptedCode) {
				s.challengeScrubNeeded = true
			}
		} else {
			if len(s.challenges) >= maxChallengeCount {
				return errLegacyChallengeCapacity
			}
			for id, existing := range s.challenges {
				if id != challenge.ID && existing.RegistrationID == challenge.RegistrationID {
					return errLegacyChallengeBinding
				}
			}
		}
		s.challenges[challenge.ID] = legacyCloneEmailChallenge(challenge)
		return nil
	default:
		return errors.New("unknown journal record type")
	}
}

func (s *Store) recoverInFlight() error {
	items := sortedEntries(s.entries)
	now := s.config.Now().In(domain.MoscowLocation())
	for _, item := range items {
		for _, channel := range []domain.Channel{domain.SMTP, domain.Telegram} {
			job := item.jobs[channel]
			if job.State != domain.InFlight {
				continue
			}
			if s.config.Policy.Exhausted(job.Attempts, domain.RetryCycleStart(job, item.submission), now) {
				job.State = domain.Failed
				job.ErrorCode = "retry_exhausted"
				job.NextAttemptAt = time.Time{}
				job.ConfirmedAt = now
			} else {
				job.State = domain.Pending
				job.NextAttemptAt = job.StartedAt.Add(s.config.Policy.Delay(job.Attempts))
			}
			if err := s.appendState(item, job); err != nil {
				return fmt.Errorf("recover in-flight delivery: %w", err)
			}
		}
	}
	return nil
}

func (s *Store) appendState(item *entry, job domain.Job) error {
	if err := domain.ValidateJob(job); err != nil {
		return err
	}
	if !domain.ValidTransition(item.jobs[job.Channel], job) {
		return errors.New("impossible delivery state transition")
	}
	if !domain.ValidJobTimeline(job, item.submission) {
		return errors.New("delivery job timeline is invalid")
	}
	line, err := encodeRecord(record{Version: scopedVersion, Type: "state", Namespace: pointer(item.submission.Namespace), SubmissionID: string(item.submission.ID), Job: pointer(toWireJob(job))})
	if err != nil {
		return err
	}
	projected := make(map[domain.SubmissionID]*entry, len(s.entries))
	for id, existing := range s.entries {
		projected[id] = existing
	}
	projectedEntry := &entry{submission: item.submission, jobs: make(map[domain.Channel]domain.Job, 2)}
	for currentChannel, currentJob := range item.jobs {
		projectedEntry.jobs[currentChannel] = currentJob
	}
	projectedEntry.jobs[job.Channel] = job
	projected[item.submission.Namespace.Key(item.submission.ID)] = projectedEntry
	if err := s.append(line, nil, reservedForState(projected, s.challenges, s.config.Policy)); err != nil {
		return err
	}
	item.jobs[job.Channel] = job
	return nil
}

func (s *Store) append(line []byte, accepted *entry, targetReserve int64) error {
	oldSize := s.bytes
	if accepted != nil {
		projected := make(map[domain.SubmissionID]*entry, len(s.entries)+1)
		for id, existing := range s.entries {
			projected[id] = existing
		}
		projected[accepted.submission.Namespace.Key(accepted.submission.ID)] = accepted
		targetReserve = reservedForState(projected, s.challenges, s.config.Policy)
	}
	lineBytes := int64(len(line))
	if oldSize > s.config.MaxBytes || lineBytes > s.config.MaxBytes-oldSize {
		return errCapacity
	}
	newSize := oldSize + lineBytes
	if !withinCapacityEnvelope(newSize, targetReserve, s.config.MaxBytes) {
		return errCapacity
	}
	if _, err := s.file.Seek(oldSize, io.SeekStart); err != nil {
		return s.rollbackAppend(oldSize, fmt.Errorf("seek journal: %w", err))
	}
	remaining := line
	for len(remaining) > 0 {
		n, err := s.file.Write(remaining)
		if err != nil {
			return s.rollbackAppend(oldSize, fmt.Errorf("append journal: %w", err))
		}
		if n == 0 {
			return s.rollbackAppend(oldSize, io.ErrShortWrite)
		}
		remaining = remaining[n:]
	}
	if err := s.file.Sync(); err != nil {
		return s.rollbackAppend(oldSize, fmt.Errorf("sync journal: %w", err))
	}
	s.bytes = oldSize + int64(len(line))
	if accepted != nil {
		if e := s.registerJobs(accepted.submission, accepted.jobs); e != nil {
			s.failed = true
			return application.ErrStorageUnavailable
		}
		s.entries[accepted.submission.Namespace.Key(accepted.submission.ID)] = accepted
	}
	return nil
}

func (s *Store) rollbackAppend(size int64, cause error) error {
	truncateErr := s.file.Truncate(size)
	seekErr := error(nil)
	if truncateErr == nil {
		_, seekErr = s.file.Seek(size, io.SeekStart)
	}
	syncErr := error(nil)
	if truncateErr == nil && seekErr == nil {
		syncErr = s.file.Sync()
	}
	if truncateErr != nil || seekErr != nil || syncErr != nil {
		s.failed = true
		return errors.Join(application.ErrStorageUnavailable, cause, truncateErr, seekErr, syncErr)
	}
	return errors.Join(application.ErrStorageUnavailable, cause)
}

func (s *Store) compact(kept map[domain.SubmissionID]*entry, keptChallenges map[legacyChallengeID]legacyEmailChallenge) (returnErr error) {
	dir := filepath.Dir(s.config.Path)
	temp, err := os.CreateTemp(dir, compactTempPrefix+"*")
	if err != nil {
		return fmt.Errorf("create compacted journal: %w", err)
	}
	tempPath := temp.Name()
	if err := syscall.Flock(int(temp.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.Join(fmt.Errorf("lock compacted journal: %w", err), temp.Close())
	}
	tempClosed := false
	defer func() {
		if !tempClosed {
			returnErr = errors.Join(returnErr, temp.Close())
		}
		if err := os.Remove(tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			returnErr = errors.Join(returnErr, err)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return err
	}
	if err := temp.Chown(s.config.UID, s.config.GID); err != nil {
		return err
	}
	var buffer bytes.Buffer
	header, err := encodeRecord(record{Version: formatVersion, Type: "header"})
	if err != nil {
		return err
	}
	if _, err := buffer.Write(header); err != nil {
		return err
	}
	items := sortedEntries(kept)
	for _, item := range items {
		jobs := []wireJob{toWireJob(item.jobs[domain.SMTP]), toWireJob(item.jobs[domain.Telegram])}
		line, err := encodeRecord(record{Version: scopedVersion, Type: "snapshot", Submission: toWireSubmission(item.submission), Jobs: jobs})
		if err != nil {
			return err
		}
		if _, err := buffer.Write(line); err != nil {
			return err
		}
	}
	challengeItems := sortedChallenges(keptChallenges)
	for _, challenge := range challengeItems {
		line, err := encodeChallengeRecord(challenge)
		if err != nil {
			return err
		}
		if _, err := buffer.Write(line); err != nil {
			return err
		}
	}
	compactedSize := int64(buffer.Len())
	remainingReserve := reservedForState(kept, keptChallenges, s.config.Policy)
	if !withinCapacityEnvelope(compactedSize, remainingReserve, s.config.MaxBytes) || !fitsCompactionPeak(s.bytes, compactedSize, remainingReserve, s.config.MaxBytes) {
		return errCapacity
	}
	if _, err := temp.Write(buffer.Bytes()); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		tempClosed = true
		return err
	}
	tempClosed = true
	if err := os.Rename(tempPath, s.config.Path); err != nil {
		return err
	}
	dirFile, err := os.Open(dir)
	if err != nil {
		s.failed = true
		return err
	}
	if err := dirFile.Sync(); err != nil {
		s.failed = true
		return errors.Join(err, dirFile.Close())
	}
	if err := dirFile.Close(); err != nil {
		s.failed = true
		return err
	}
	newFile, err := openSecureFile(s.config.Path, s.config.UID, s.config.GID, false)
	if err != nil {
		s.failed = true
		return err
	}
	oldFile := s.file
	s.file = newFile
	if err := oldFile.Close(); err != nil {
		s.failed = true
		return err
	}
	s.entries = kept
	s.challenges = keptChallenges
	s.bytes = int64(buffer.Len())
	s.challengeScrubNeeded = false
	return nil
}

func (s *Store) reservedBytes() int64 {
	return reservedForState(s.entries, s.challenges, s.config.Policy)
}

func reservedForState(entries map[domain.SubmissionID]*entry, _ map[legacyChallengeID]legacyEmailChallenge, policy domain.RetryPolicy) int64 {
	result := reservedForEntries(entries, policy)

	return result
}

func reservedForEntries(entries map[domain.SubmissionID]*entry, policy domain.RetryPolicy) int64 {
	var events int64
	for _, item := range entries {
		for _, job := range item.jobs {
			remainingAttempts := policy.MaxAttempts - job.Attempts
			if remainingAttempts < 0 {
				remainingAttempts = 0
			}
			switch job.State {
			case domain.Pending:
				jobEvents := int64(remainingAttempts) * 2
				if jobEvents == 0 {
					jobEvents = 1
				}
				events += jobEvents
			case domain.InFlight:
				events++
				events += int64(remainingAttempts) * 2
			}
		}
	}
	return events * maxStateRecordBytes()
}

func withinCapacityEnvelope(activeBytes, reserveBytes, maximum int64) bool {
	if activeBytes < 0 || reserveBytes < 0 || maximum <= 0 || activeBytes > maximum || reserveBytes > maximum-activeBytes {
		return false
	}
	return activeBytes+reserveBytes <= maximum/2
}

func fitsCompactionPeak(activeBytes, compactedBytes, reserveBytes, maximum int64) bool {
	if activeBytes < 0 || compactedBytes < 0 || reserveBytes < 0 || maximum <= 0 || activeBytes > maximum {
		return false
	}
	remaining := maximum - activeBytes
	if compactedBytes > remaining {
		return false
	}
	remaining -= compactedBytes
	return reserveBytes <= remaining
}

func maxStateRecordBytes() int64 {
	maximumTime := time.Date(9999, time.December, 31, 23, 59, 59, 999999999, domain.MoscowLocation())
	job := domain.Job{ID: strings.Repeat("x", 36), Channel: domain.Telegram, State: domain.Failed, Attempts: 10, NextAttemptAt: maximumTime, RetryStartedAt: maximumTime, StartedAt: maximumTime, ConfirmedAt: maximumTime, ErrorCode: strings.Repeat("x", maxErrorCodeSize), ProviderID: strings.Repeat("x", maxProviderSize)}
	line, err := encodeRecord(record{Version: scopedVersion, Namespace: pointer(domain.Namespace{Kind: "app", UserID: strings.Repeat("x", 36), OrgID: strings.Repeat("x", 36), TenantID: strings.Repeat("x", 36)}), Type: "state", SubmissionID: strings.Repeat("x", maxIdentifierSize), Job: pointer(toWireJob(job))})
	if err != nil {
		return 1 << 62
	}
	return int64(len(line))
}

func (s *Store) usable() error {
	if s.closed || s.failed || s.file == nil || s.lock == nil {
		return application.ErrStorageUnavailable
	}
	return nil
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return errors.New("nil context")
	}
	return ctx.Err()
}

func encodeRecord(rec record) ([]byte, error) {
	line, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	return append(line, '\n'), nil
}

func decodeStrict(data []byte, target any) error {
	if err := checkJSONStructure(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("journal record has trailing data")
	}
	return nil
}

func checkJSONStructure(data []byte) error {
	if !utf8.Valid(data) {
		return errors.New("journal record contains invalid UTF-8")
	}
	if err := validateUnicodeEscapes(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := scanJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("journal record has trailing data")
	}
	return nil
}

func validateUnicodeEscapes(data []byte) error {
	inString := false
	for index := 0; index < len(data); index++ {
		if !inString {
			if data[index] == '"' {
				inString = true
			}
			continue
		}
		if data[index] == '"' {
			inString = false
			continue
		}
		if data[index] != '\\' || index+1 >= len(data) {
			continue
		}
		index++
		if data[index] != 'u' || index+4 >= len(data) {
			continue
		}
		codeUnit, ok := parseHexQuad(data[index+1 : index+5])
		if !ok {
			continue
		}
		switch {
		case codeUnit >= 0xd800 && codeUnit <= 0xdbff:
			if index+10 >= len(data) || data[index+5] != '\\' || data[index+6] != 'u' {
				return errors.New("journal record contains an unpaired high surrogate")
			}
			low, ok := parseHexQuad(data[index+7 : index+11])
			if !ok || low < 0xdc00 || low > 0xdfff {
				return errors.New("journal record contains an invalid surrogate pair")
			}
			index += 10
		case codeUnit >= 0xdc00 && codeUnit <= 0xdfff:
			return errors.New("journal record contains an unpaired low surrogate")
		}
	}
	return nil
}

func parseHexQuad(data []byte) (uint16, bool) {
	if len(data) != 4 {
		return 0, false
	}
	var value uint16
	for _, digit := range data {
		value <<= 4
		switch {
		case digit >= '0' && digit <= '9':
			value |= uint16(digit - '0')
		case digit >= 'a' && digit <= 'f':
			value |= uint16(digit-'a') + 10
		case digit >= 'A' && digit <= 'F':
			value |= uint16(digit-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("journal object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("journal record repeats field %q", key)
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("journal object is incomplete")
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("journal array is incomplete")
		}
	default:
		return errors.New("journal record has an invalid JSON delimiter")
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		return errors.Join(err, directory.Close())
	}
	return directory.Close()
}

func cleanupCompactionTemps(dir string, uid, gid int) (returnErr error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	removed := false
	defer func() {
		if removed {
			returnErr = errors.Join(returnErr, syncDirectory(dir))
		}
	}()
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), compactTempPrefix) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		before, err := os.Lstat(path)
		if err != nil {
			return err
		}
		stat, ok := before.Sys().(*syscall.Stat_t)
		if !ok || !before.Mode().IsRegular() || before.Mode().Perm() != 0o600 || int(stat.Uid) != uid || int(stat.Gid) != gid || stat.Nlink != 1 {
			return fmt.Errorf("unsafe compaction temporary %q", entry.Name())
		}
		file, err := openSecureFile(path, uid, gid, false)
		if err != nil {
			return fmt.Errorf("open compaction temporary %q: %w", entry.Name(), err)
		}
		after, err := file.Stat()
		if err != nil {
			return errors.Join(err, file.Close())
		}
		afterStat, ok := after.Sys().(*syscall.Stat_t)
		if !ok || !os.SameFile(before, after) || afterStat.Nlink != 1 {
			return errors.Join(fmt.Errorf("compaction temporary %q changed during inspection", entry.Name()), file.Close())
		}
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			return errors.Join(fmt.Errorf("compaction temporary %q is still locked", entry.Name()), file.Close())
		}
		if err := os.Remove(path); err != nil {
			return errors.Join(err, closeLock(file))
		}
		removed = true
		if err := closeLock(file); err != nil {
			return err
		}
	}
	return nil
}

func closeLock(lock *os.File) error {
	if lock == nil {
		return nil
	}
	return errors.Join(syscall.Flock(int(lock.Fd()), syscall.LOCK_UN), lock.Close())
}

func releaseFiles(file, lock *os.File) error {
	var result error
	if file != nil {
		result = errors.Join(result, file.Close())
	}
	result = errors.Join(result, closeLock(lock))
	return result
}

func toWireSubmission(submission domain.Submission) *wireSubmission {
	if submission.Namespace.Kind == "" {
		submission.Namespace = domain.Anonymous()
	}
	if submission.CommandID == "" {
		submission.CommandID = string(submission.ID)
	}
	return &wireSubmission{Namespace: pointer(submission.Namespace), CommandID: submission.CommandID, Legacy: submission.Legacy, ID: string(submission.ID), Name: submission.Name, Email: submission.Email, Phone: submission.Phone, Message: submission.Message, ConsentVersion: submission.ConsentVersion, ConsentPurpose: submission.ConsentPurpose, AcceptedAt: submission.AcceptedAt, ReceivedAt: submission.ReceivedAt, Fingerprint: submission.Fingerprint}
}

func fromWireSubmission(wire wireSubmission) domain.Submission {
	ns := domain.Anonymous()
	legacy := wire.Namespace == nil || wire.Legacy
	cmd := wire.CommandID
	if wire.Namespace != nil {
		ns = *wire.Namespace
	}
	if cmd == "" && legacy {
		cmd = wire.ID
	}
	return domain.Submission{Namespace: ns, CommandID: cmd, Legacy: legacy, ID: domain.SubmissionID(wire.ID), Name: wire.Name, Email: wire.Email, Phone: wire.Phone, Message: wire.Message, ConsentVersion: wire.ConsentVersion, ConsentPurpose: wire.ConsentPurpose, AcceptedAt: wire.AcceptedAt, ReceivedAt: wire.ReceivedAt, Fingerprint: wire.Fingerprint}
}

func toWireJob(job domain.Job) wireJob {
	return wireJob{ID: job.ID, Channel: string(job.Channel), State: string(job.State), Attempts: job.Attempts, NextAttemptAt: job.NextAttemptAt, RetryStartedAt: job.RetryStartedAt, StartedAt: job.StartedAt, ConfirmedAt: job.ConfirmedAt, ErrorCode: job.ErrorCode, ProviderID: job.ProviderID}
}

func fromWireJob(wire wireJob) domain.Job {
	return domain.Job{ID: wire.ID, Channel: domain.Channel(wire.Channel), State: domain.JobState(wire.State), Attempts: wire.Attempts, NextAttemptAt: wire.NextAttemptAt, RetryStartedAt: wire.RetryStartedAt, StartedAt: wire.StartedAt, ConfirmedAt: wire.ConfirmedAt, ErrorCode: wire.ErrorCode, ProviderID: wire.ProviderID}
}

func sortedEntries(entries map[domain.SubmissionID]*entry) []*entry {
	items := make([]*entry, 0, len(entries))
	for _, item := range entries {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].submission.ReceivedAt.Equal(items[j].submission.ReceivedAt) {
			return items[i].submission.Namespace.Key(items[i].submission.ID) < items[j].submission.Namespace.Key(items[j].submission.ID)
		}
		return items[i].submission.ReceivedAt.Before(items[j].submission.ReceivedAt)
	})
	return items
}

func pointer[T any](value T) *T { return &value }

func newJobID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&15 | 64
	b[8] = b[8]&63 | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}
func legacyJobID(s domain.Submission, c domain.Channel) string {
	b := sha256.Sum256([]byte("legacy-feedback-job:" + string(s.ID) + ":" + s.Fingerprint + ":" + string(c)))
	b[6] = b[6]&15 | 64
	b[8] = b[8]&63 | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
func validateScopedRecord(_ []byte, r record) error {
	if r.Type == "header" || r.Type == "challenge" {
		return errors.New("invalid version 2 record")
	}
	if r.Type == "state" {
		if r.Namespace == nil || !r.Namespace.Valid() || r.Job == nil || !domain.ValidID(domain.SubmissionID(r.Job.ID)) {
			return errors.New("missing scoped state identity")
		}
		return nil
	}
	if r.Submission == nil || r.Submission.Namespace == nil || !r.Submission.Namespace.Valid() || r.Submission.CommandID != r.Submission.ID {
		return errors.New("missing scoped submission identity")
	}
	if len(r.Jobs) != 2 || r.Jobs[0].ID == r.Jobs[1].ID {
		return errors.New("aliased jobs")
	}
	for _, j := range r.Jobs {
		if !domain.ValidID(domain.SubmissionID(j.ID)) {
			return errors.New("invalid durable job identity")
		}
	}
	return nil
}

func (s *Store) registerJobs(sub domain.Submission, jobs map[domain.Channel]domain.Job) error {
	owner := sub.Namespace.Key(sub.ID)
	seen := map[string]bool{}
	for _, j := range jobs {
		if j.ID == "" || seen[j.ID] {
			return errors.New("invalid job ownership")
		}
		seen[j.ID] = true
		if old, ok := s.jobOwners[j.ID]; ok && old != owner {
			return errors.New("job aliases another namespace")
		}
	}
	for _, j := range jobs {
		s.jobOwners[j.ID] = owner
	}
	return nil
}
