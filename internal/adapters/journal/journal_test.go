package journal

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/JustIkra/umir-feedback/internal/application"
	"github.com/JustIkra/umir-feedback/internal/domain"
)

type journalTestClock struct{ now time.Time }

func (clock journalTestClock) Now() time.Time { return clock.now }

type journalTestJitter struct{}

func (journalTestJitter) Delay(delay time.Duration) time.Duration { return delay }

type journalTestSender struct{ result domain.DeliveryResult }

func (sender journalTestSender) Send(context.Context, domain.Delivery) domain.DeliveryResult {
	return sender.result
}

func testConfig(t *testing.T) Config {
	t.Helper()

	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return Config{
		Path:     filepath.Join(dir, "feedback.jsonl"),
		MaxBytes: 8 << 20,
		UID:      os.Getuid(),
		GID:      os.Getgid(),
		Policy:   domain.RetryPolicy{MaxAttempts: 3, MaxAge: 24 * time.Hour, MinDelay: time.Second, MaxDelay: time.Minute},
	}
}

func testSubmission(id domain.SubmissionID, accepted time.Time) domain.Submission {
	return domain.Submission{
		ID:             id,
		Name:           "Ada Example",
		Email:          "ada@example.test",
		Phone:          "+79990000000",
		Message:        "A useful message",
		ConsentVersion: "2026-10-12",
		ConsentPurpose: "feedback-reply",
		AcceptedAt:     accepted,
		ReceivedAt:     accepted,
		Fingerprint:    "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
}

func TestOpenHoldsStableExclusiveLock(t *testing.T) {
	cfg := testConfig(t)
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})

	if _, err := Open(cfg); !errors.Is(err, application.ErrBusy) {
		t.Fatalf("second Open error = %v, want ErrBusy", err)
	}
	for path, want := range map[string]os.FileMode{cfg.Path: 0o600, filepath.Join(filepath.Dir(cfg.Path), "feedback.lock"): 0o600} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s mode = %04o, want %04o", path, info.Mode().Perm(), want)
		}
	}
}

func TestConcurrentSameIdentityCreatesOneAcceptance(t *testing.T) {
	cfg := testConfig(t)
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	submission := testSubmission("77777777-7777-4777-8777-777777777777", time.Now().In(time.FixedZone("MSK", 3*60*60)))
	const callers = 20
	type result struct {
		created bool
		err     error
	}
	results := make(chan result, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, created, err := store.Accept(context.Background(), submission)
			results <- result{created: created, err: err}
		}()
	}
	wait.Wait()
	close(results)
	created := 0
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.created {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("created %d acceptances, want exactly one", created)
	}
}

func TestAcceptIsAtomicAndDeliveryStateSurvivesReopen(t *testing.T) {
	cfg := testConfig(t)
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().In(time.FixedZone("MSK", 3*60*60))
	submission := testSubmission("11111111-1111-4111-8111-111111111111", now)

	receipt, created, err := store.Accept(ctx, submission)
	if err != nil || !created || receipt.SubmissionID != submission.ID {
		t.Fatalf("Accept() = (%+v, %v, %v)", receipt, created, err)
	}
	replayed, created, err := store.Accept(ctx, submission)
	if err != nil || created || replayed != receipt {
		t.Fatalf("replay Accept() = (%+v, %v, %v), want original receipt", replayed, created, err)
	}
	changed := submission
	changed.Message = "Different"
	if _, _, err := store.Accept(ctx, changed); !errors.Is(err, application.ErrIdentityConflict) {
		t.Fatalf("conflicting Accept() error = %v, want ErrIdentityConflict", err)
	}

	for _, channel := range []domain.Channel{domain.SMTP, domain.Telegram} {
		delivery, found, err := store.Claim(ctx, channel, now, cfg.Policy)
		if err != nil || !found || delivery.Job.State != domain.InFlight || delivery.Job.Attempts != 1 {
			t.Fatalf("Claim(%s) = (%+v, %v, %v)", channel, delivery, found, err)
		}
		finished := delivery.Job
		finished.State = domain.Sent
		finished.ConfirmedAt = now.Add(time.Second)
		finished.ProviderID = "provider-42"
		if err := store.Finish(ctx, submission.ID, channel, finished); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, found, err := store.Claim(ctx, domain.SMTP, now.Add(time.Hour), cfg.Policy); err != nil || found {
		t.Fatalf("sent job was claimable: found=%v err=%v", found, err)
	}
	status, err := store.Status(ctx)
	if err != nil || len(status) != 2 {
		t.Fatalf("Status() = %d deliveries, %v", len(status), err)
	}
	for _, delivery := range status {
		if delivery.Job.State != domain.Sent || delivery.Job.ProviderID != "provider-42" {
			t.Errorf("reopened state = %+v, want sent provider-42", delivery.Job)
		}
	}
}

func TestOpenRepairsOnlyIncompleteTail(t *testing.T) {
	cfg := testConfig(t)
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	validSize := info.Size()
	file, err := os.OpenFile(cfg.Path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"type":"broken"}`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(cfg)
	if err != nil {
		t.Fatalf("Open() rejected an incomplete tail: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	info, err = os.Stat(cfg.Path)
	if err != nil || info.Size() != validSize {
		t.Fatalf("repaired size = %v, %v; want %d", info, err, validSize)
	}
}

func TestOpenRejectsCorruptCompleteRecordWithoutReset(t *testing.T) {
	cfg := testConfig(t)
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(cfg.Path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("not-json\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(cfg); err == nil {
		t.Fatal("Open() accepted a corrupt complete record")
	}
	after, err := os.ReadFile(cfg.Path)
	if err != nil || string(after) != string(before) {
		t.Fatal("Open() changed the corrupt journal")
	}
}

func TestOpenRejectsDuplicateKnownJSONField(t *testing.T) {
	cfg := testConfig(t)
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	submission := testSubmission("88888888-8888-4888-8888-888888888888", time.Now().In(time.FixedZone("MSK", 3*60*60)))
	if _, _, err := store.Accept(context.Background(), submission); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	acceptLine := bytes.Split(data, []byte{'\n'})[1]
	duplicate := bytes.Replace(acceptLine, []byte(`"type":"accept"`), []byte(`"type":"invalid","type":"accept"`), 1)
	file, err := os.OpenFile(cfg.Path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(append(duplicate, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(cfg); err == nil {
		t.Fatal("Open() accepted a duplicate JSON field")
	}
}

func TestOpenRejectsInvalidUTF8AndUnpairedSurrogate(t *testing.T) {
	for name, replacement := range map[string][]byte{
		"invalid_utf8":       {'A', 'd', 'a', 0xff, ' ', 'E', 'x', 'a', 'm', 'p', 'l', 'e'},
		"unpaired_surrogate": []byte(`Ada\ud800 Example`),
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(t)
			store, err := Open(cfg)
			if err != nil {
				t.Fatal(err)
			}
			submission := testSubmission("abababab-abab-4bab-8bab-abababababab", time.Now().In(time.FixedZone("MSK", 3*60*60)))
			if _, _, err := store.Accept(context.Background(), submission); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(cfg.Path)
			if err != nil {
				t.Fatal(err)
			}
			data = bytes.Replace(data, []byte("Ada Example"), replacement, 1)
			if err := os.WriteFile(cfg.Path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(cfg); err == nil {
				t.Fatal("Open() accepted invalid Unicode in a complete record")
			}
		})
	}
}

func TestOpenRejectsImpossibleCompleteStateTransition(t *testing.T) {
	cfg := testConfig(t)
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().In(time.FixedZone("MSK", 3*60*60))
	submission := testSubmission("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", now)
	if _, _, err := store.Accept(context.Background(), submission); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	job := domain.Job{Channel: domain.SMTP, State: domain.Sent, Attempts: 1, StartedAt: now, ConfirmedAt: now.Add(time.Second), ProviderID: "provider-42"}
	line, err := encodeRecord(record{Version: formatVersion, Type: "state", SubmissionID: string(submission.ID), Job: pointer(toWireJob(job))})
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(cfg.Path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(line); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(cfg); err == nil {
		t.Fatal("Open() accepted a direct pending-to-sent transition")
	}
}

func TestOpenRejectsCompleteRecordWithMissingZeroValueField(t *testing.T) {
	cfg := testConfig(t)
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	submission := testSubmission("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", time.Now().In(time.FixedZone("MSK", 3*60*60)))
	if _, _, err := store.Accept(context.Background(), submission); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	broken := bytes.Replace(data, []byte(`"attempts":0,`), nil, 1)
	if bytes.Equal(data, broken) {
		t.Fatal("test did not find serialized attempts field")
	}
	if err := os.WriteFile(cfg.Path, broken, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(cfg); err == nil {
		t.Fatal("Open() accepted a complete acceptance record missing attempts")
	}
}

func TestOpenRejectsSymlinkedJournal(t *testing.T) {
	cfg := testConfig(t)
	target := filepath.Join(filepath.Dir(cfg.Path), "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, cfg.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(cfg); err == nil {
		t.Fatal("Open() accepted a symlinked journal")
	}
}

func TestOpenRejectsSymlinkedPathAncestor(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(base, "child")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(base, alias); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t)
	cfg.Path = filepath.Join(alias, "child", "feedback.jsonl")
	if _, err := Open(cfg); err == nil {
		t.Fatal("Open() accepted a path with a symlinked ancestor")
	}
}

func TestFailedJournalWriteMakesStoreUnreadyWhenRollbackCannotBeConfirmed(t *testing.T) {
	cfg := testConfig(t)
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.file.Close(); err != nil {
		t.Fatal(err)
	}
	store.file = nil
	now := time.Now().In(time.FixedZone("MSK", 3*60*60))
	if _, _, err := store.Accept(context.Background(), testSubmission("99999999-9999-4999-8999-999999999999", now)); err == nil {
		t.Fatal("Accept() succeeded after the journal file closed")
	}
	if store.Ready() {
		t.Fatal("store remained ready after append rollback could not be confirmed")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAppendFailureRollsBackAndKeepsStoreReady(t *testing.T) {
	if os.Getenv("JOURNAL_ROLLBACK_CHILD") != "1" {
		command := exec.Command(os.Args[0], "-test.run=^TestAppendFailureRollsBackAndKeepsStoreReady$")
		command.Env = append(os.Environ(), "JOURNAL_ROLLBACK_CHILD=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("rollback subprocess failed: %v\n%s", err, output)
		}
		return
	}

	signal.Ignore(syscall.SIGXFSZ)
	cfg := testConfig(t)
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatal(err)
	}
	limit.Cur = uint64(info.Size() + 16)
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatal(err)
	}
	now := time.Now().In(time.FixedZone("MSK", 3*60*60))
	if _, _, err := store.Accept(context.Background(), testSubmission("cccccccc-cccc-4ccc-8ccc-cccccccccccc", now)); !errors.Is(err, application.ErrStorageUnavailable) {
		t.Fatalf("Accept() error = %v, want storage error", err)
	}
	if !store.Ready() {
		t.Fatal("store became unready after a confirmed append rollback")
	}
	info, err = os.Stat(cfg.Path)
	if err != nil || info.Size() != int64(len(`{"version":1,"type":"header"}`)+1) {
		t.Fatalf("journal size = %v, %v after rollback", info, err)
	}
	if _, found, err := store.Lookup(context.Background(), "cccccccc-cccc-4ccc-8ccc-cccccccccccc"); err != nil || found {
		t.Fatalf("rolled-back submission found=%v err=%v", found, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryRequeuesInflightWithoutResettingAttempt(t *testing.T) {
	cfg := testConfig(t)
	ctx := context.Background()
	now := time.Now().In(time.FixedZone("MSK", 3*60*60))
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	submission := testSubmission("22222222-2222-4222-8222-222222222222", now)
	if _, _, err := store.Accept(ctx, submission); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.Claim(ctx, domain.SMTP, now, cfg.Policy); err != nil || !found {
		t.Fatalf("Claim() found=%v err=%v", found, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	status, err := store.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status[0].Job.State != domain.Pending || status[0].Job.Attempts != 1 || status[0].Job.StartedAt.IsZero() {
		t.Fatalf("recovered job = %+v, want pending with attempt 1", status[0].Job)
	}
	retryAt := status[0].Job.NextAttemptAt
	delivery, found, err := store.Claim(ctx, domain.SMTP, retryAt, cfg.Policy)
	if err != nil || !found || delivery.Job.Attempts != 2 {
		t.Fatalf("second Claim() = (%+v, %v, %v), want attempt 2", delivery, found, err)
	}
}

func TestRecoveryFailsInflightAtInjectedRetryAgeLimit(t *testing.T) {
	cfg := testConfig(t)
	ctx := context.Background()
	fakeNow := time.Now().In(time.FixedZone("MSK", 3*60*60))
	accepted := fakeNow.Add(-25 * time.Hour)
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	submission := testSubmission("abababab-abab-4bab-8bab-abababababab", accepted)
	if _, _, err := store.Accept(ctx, submission); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.Claim(ctx, domain.SMTP, accepted, cfg.Policy); err != nil || !found {
		t.Fatalf("Claim() found=%v err=%v", found, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.Now = func() time.Time { return fakeNow }
	store, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	status, err := store.Status(ctx)
	if err != nil || status[0].Job.State != domain.Failed || status[0].Job.Attempts != 1 || !status[0].Job.ConfirmedAt.Equal(fakeNow) {
		t.Fatalf("recovered status = %+v, err=%v", status[0].Job, err)
	}
}

func TestClaimMarksExpiredPendingJobFailedDurably(t *testing.T) {
	cfg := testConfig(t)
	ctx := context.Background()
	now := time.Now().In(time.FixedZone("MSK", 3*60*60))
	accepted := now.Add(-25 * time.Hour)
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	submission := testSubmission("dddddddd-dddd-4ddd-8ddd-dddddddddddd", accepted)
	if _, _, err := store.Accept(ctx, submission); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.Claim(ctx, domain.SMTP, now, cfg.Policy); err != nil || found {
		t.Fatalf("Claim() found=%v err=%v, want expired failure", found, err)
	}
	status, err := store.Status(ctx)
	if err != nil || status[0].Job.State != domain.Failed || status[0].Job.ErrorCode != "retry_exhausted" {
		t.Fatalf("expired status = %+v, err=%v", status[0].Job, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	status, err = store.Status(ctx)
	if err != nil || status[0].Job.State != domain.Failed {
		t.Fatalf("reopened expired status = %+v, err=%v", status[0].Job, err)
	}
}

func TestLateRetryAfterExpiresJobAtPolicyDeadline(t *testing.T) {
	cfg := testConfig(t)
	ctx := context.Background()
	accepted := time.Now().In(time.FixedZone("MSK", 3*60*60))
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	submission := testSubmission("dededede-dede-4ede-8ede-dededededede", accepted)
	if _, _, err := store.Accept(ctx, submission); err != nil {
		t.Fatal(err)
	}
	lastAttempt := accepted.Add(23 * time.Hour)
	worker := application.NewWorker(store, journalTestClock{now: lastAttempt}, journalTestJitter{}, journalTestSender{result: domain.DeliveryResult{Code: "telegram_rate_limited", RetryAfter: 2 * time.Hour}}, domain.Telegram, cfg.Policy)
	if worked, err := worker.ProcessOne(ctx); err != nil || !worked {
		t.Fatalf("ProcessOne() worked=%v err=%v", worked, err)
	}
	deadline := accepted.Add(24 * time.Hour)
	if _, found, err := store.Claim(ctx, domain.Telegram, deadline, cfg.Policy); err != nil || found {
		t.Fatalf("Claim() at policy deadline found=%v err=%v", found, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	status, err := store.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, delivery := range status {
		if delivery.Job.Channel == domain.Telegram {
			if delivery.Job.State != domain.Failed || !delivery.Job.ConfirmedAt.Equal(deadline) {
				t.Fatalf("Telegram status = %+v, want failed at policy deadline", delivery.Job)
			}
			return
		}
	}
	t.Fatal("Telegram delivery missing from status")
}

func TestStateReserveUsesMaximumMoscowTimestampLengths(t *testing.T) {
	maximumTime := time.Date(9999, time.December, 31, 23, 59, 59, 999999999, moscowLocation())
	job := domain.Job{Channel: domain.Telegram, State: domain.Failed, Attempts: 10, NextAttemptAt: maximumTime, RetryStartedAt: maximumTime, StartedAt: maximumTime, ConfirmedAt: maximumTime, ErrorCode: strings.Repeat("x", maxErrorCodeSize), ProviderID: strings.Repeat("x", maxProviderSize)}
	line, err := encodeRecord(record{Version: formatVersion, Type: "state", SubmissionID: strings.Repeat("x", maxIdentifierSize), Job: pointer(toWireJob(job))})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := maxStateRecordBytes(), int64(len(line)); got != want {
		t.Fatalf("state reserve = %d bytes, maximal record = %d bytes", got, want)
	}
}

func TestJournalOmitsUnsetTimestampsAndKeepsMoscowTimes(t *testing.T) {
	cfg := testConfig(t)
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().In(time.FixedZone("MSK", 3*60*60))
	if _, _, err := store.Accept(context.Background(), testSubmission("abababab-caca-4aca-8aca-abababababab", now)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range [][]byte{[]byte("0001-01-01T00:00:00Z"), []byte(`"retryStartedAt"`), []byte(`"startedAt"`), []byte(`"confirmedAt"`)} {
		if bytes.Contains(data, absent) {
			t.Errorf("journal contains unset timestamp representation %s", absent)
		}
	}
	if !bytes.Contains(data, []byte(`"receivedAt":"`)) || !bytes.Contains(data, []byte("+03:00")) {
		t.Fatal("journal does not contain Moscow timestamps")
	}
	if _, err := Open(cfg); err != nil {
		t.Fatalf("strict replay rejected omitted zero timestamps: %v", err)
	}
}

func TestOpenCleansCrashLeftCompactionFilesBeforeDeletingData(t *testing.T) {
	for _, operation := range []string{"delete", "purge"} {
		t.Run(operation, func(t *testing.T) {
			cfg := testConfig(t)
			marker := "private abandoned compaction copy"
			stalePath := filepath.Join(filepath.Dir(cfg.Path), ".feedback-compact-crash")
			if err := os.WriteFile(stalePath, []byte(marker), 0o600); err != nil {
				t.Fatal(err)
			}
			store, err := Open(cfg)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			now := time.Now().In(time.FixedZone("MSK", 3*60*60))
			submission := testSubmission("abababab-caca-4aca-8aca-abababababab", now)
			submission.Message = marker
			if _, _, err := store.Accept(ctx, submission); err != nil {
				t.Fatal(err)
			}
			if operation == "delete" {
				err = store.DeleteEmail(ctx, submission.Email)
			} else {
				err = store.Purge(ctx, now.Add(time.Second))
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(stalePath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stale compaction file remains: %v", err)
			}
			entries, err := os.ReadDir(filepath.Dir(cfg.Path))
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				data, err := os.ReadFile(filepath.Join(filepath.Dir(cfg.Path), entry.Name()))
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(data, []byte(marker)) {
					t.Errorf("personal data remains in %s", entry.Name())
				}
			}
		})
	}
}

func TestOpenRejectsUnsafeCrashLeftCompactionFile(t *testing.T) {
	cfg := testConfig(t)
	stalePath := filepath.Join(filepath.Dir(cfg.Path), ".feedback-compact-unexpected")
	target := filepath.Join(filepath.Dir(cfg.Path), "unexpected-target")
	if err := os.WriteFile(target, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, stalePath); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(cfg); err == nil {
		t.Fatal("Open() accepted a symlinked compaction temporary")
	}
	info, err := os.Lstat(stalePath)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("unsafe temporary was removed or changed: %v, %v", info, err)
	}
}

func TestOpenPreservesUnexpectedlyPermissiveCompactionFile(t *testing.T) {
	cfg := testConfig(t)
	stalePath := filepath.Join(filepath.Dir(cfg.Path), ".feedback-compact-public")
	if err := os.WriteFile(stalePath, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stalePath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(cfg); err == nil {
		t.Fatal("Open() accepted a compaction temporary with unsafe permissions")
	}
	info, err := os.Lstat(stalePath)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("unsafe temporary was removed or changed: %v, %v", info, err)
	}
}

func TestReservationSurvivesRetryPolicyDowngrade(t *testing.T) {
	policy := domain.RetryPolicy{MaxAttempts: 1, MaxAge: 24 * time.Hour, MinDelay: time.Second, MaxDelay: time.Minute}
	item := &entry{jobs: map[domain.Channel]domain.Job{
		domain.SMTP:     {Channel: domain.SMTP, State: domain.Pending, Attempts: 5},
		domain.Telegram: {Channel: domain.Telegram, State: domain.Pending, Attempts: 3},
	}}
	got := reservedForEntries(map[domain.SubmissionID]*entry{"submission": item}, policy)
	if want := 2 * maxStateRecordBytes(); got < want {
		t.Fatalf("pending terminal state reserve = %d bytes, want at least %d", got, want)
	}
}

func TestAdmissionReservesCapacityForCompactionCopy(t *testing.T) {
	cfg := testConfig(t)
	cfg.Policy = domain.DefaultRetryPolicy()
	cfg.MaxBytes = 40_000
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	now := time.Now().In(time.FixedZone("MSK", 3*60*60))
	if _, _, err := store.Accept(context.Background(), testSubmission("abababab-dada-4ada-8ada-abababababab", now)); !errors.Is(err, errCapacity) {
		t.Fatalf("Accept() error = %v, want capacity error for future compaction copy", err)
	}
	exported, err := store.Export(context.Background(), time.Time{})
	if err != nil || len(exported) != 0 {
		t.Fatalf("Export() after capacity rejection returned %d records, err=%v", len(exported), err)
	}
}

func TestAllTenRetryCyclesFitUnderCapacityEnvelope(t *testing.T) {
	cfg := testConfig(t)
	cfg.Policy = domain.DefaultRetryPolicy()
	cfg.MaxBytes = 64 << 10
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	accepted := time.Now().In(time.FixedZone("MSK", 3*60*60))
	submission := testSubmission("abababab-dada-4ada-8ada-abababababab", accepted)
	if _, _, err := store.Accept(ctx, submission); err != nil {
		t.Fatalf("Accept() did not reserve all delivery states and compaction copy: %v", err)
	}
	for attempt := 1; attempt <= cfg.Policy.MaxAttempts; attempt++ {
		attemptTime := accepted.Add(time.Duration(attempt-1) * 2 * time.Second)
		for _, channel := range []domain.Channel{domain.SMTP, domain.Telegram} {
			delivery, found, err := store.Claim(ctx, channel, attemptTime, cfg.Policy)
			if err != nil || !found {
				t.Fatalf("Claim(%s) attempt=%d found=%v err=%v", channel, attempt, found, err)
			}
			if attempt == cfg.Policy.MaxAttempts {
				delivery.Job.State = domain.Failed
				delivery.Job.ConfirmedAt = attemptTime.Add(time.Second)
				delivery.Job.ErrorCode = string(channel) + "_temporary"
			} else {
				delivery.Job.State = domain.Pending
				delivery.Job.NextAttemptAt = attemptTime.Add(time.Second)
				delivery.Job.ErrorCode = string(channel) + "_temporary"
			}
			if err := store.Finish(ctx, submission.ID, channel, delivery.Job); err != nil {
				t.Fatalf("Finish(%s) attempt=%d: %v", channel, attempt, err)
			}
		}
	}
	status, err := store.Status(ctx)
	if err != nil || len(status) != 2 {
		t.Fatalf("Status() returned %d deliveries, err=%v", len(status), err)
	}
	for _, delivery := range status {
		if delivery.Job.State != domain.Failed || delivery.Job.Attempts != cfg.Policy.MaxAttempts {
			t.Errorf("exhausted job = %+v", delivery.Job)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCompactionRejectsInsufficientTwoFileBudget(t *testing.T) {
	cfg := testConfig(t)
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().In(time.FixedZone("MSK", 3*60*60))
	submission := testSubmission("cdcdcdcd-cdcd-4dcd-8dcd-cdcdcdcdcdcd", now)
	submission.Message = "retained until compaction fits"
	if _, _, err := store.Accept(ctx, submission); err != nil {
		t.Fatal(err)
	}
	header, err := encodeRecord(record{Version: formatVersion, Type: "header"})
	if err != nil {
		t.Fatal(err)
	}
	store.config.MaxBytes = store.bytes + int64(len(header)) - 1
	if err := store.DeleteEmail(ctx, submission.Email); !errors.Is(err, errCapacity) {
		t.Fatalf("DeleteEmail() error = %v, want capacity error", err)
	}
	entries, err := os.ReadDir(filepath.Dir(cfg.Path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), compactTempPrefix) {
			t.Fatalf("temporary compaction copy %q remains after rejected compaction", entry.Name())
		}
	}
	stored, found, err := store.Lookup(ctx, submission.ID)
	if err != nil || !found || stored.Message != submission.Message {
		t.Fatalf("submission after rejected compaction found=%v err=%v", found, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReopenWithLowerAttemptLimitFailsPreviouslyPendingJob(t *testing.T) {
	cfg := testConfig(t)
	ctx := context.Background()
	now := time.Now().In(time.FixedZone("MSK", 3*60*60))
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	submission := testSubmission("cdcdcdcd-cdcd-4dcd-8dcd-cdcdcdcdcdcd", now)
	if _, _, err := store.Accept(ctx, submission); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		attemptTime := now.Add(time.Duration(attempt-1) * time.Second)
		delivery, found, err := store.Claim(ctx, domain.SMTP, attemptTime, cfg.Policy)
		if err != nil || !found {
			t.Fatalf("Claim() attempt %d found=%v err=%v", attempt, found, err)
		}
		delivery.Job.State = domain.Pending
		delivery.Job.NextAttemptAt = attemptTime.Add(time.Second)
		delivery.Job.ErrorCode = "smtp_temporary"
		if err := store.Finish(ctx, submission.ID, domain.SMTP, delivery.Job); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.Policy.MaxAttempts = 1
	store, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, found, err := store.Claim(ctx, domain.SMTP, now.Add(1500*time.Millisecond), cfg.Policy); err != nil || found {
		t.Fatalf("Claim() after policy downgrade found=%v err=%v", found, err)
	}
	status, err := store.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, delivery := range status {
		if delivery.Job.Channel == domain.SMTP {
			if delivery.Job.State != domain.Failed || delivery.Job.Attempts != 2 {
				t.Fatalf("lowered policy state = %+v, want failed with both attempts retained", delivery.Job)
			}
			return
		}
	}
	t.Fatal("SMTP delivery missing from status")
}

func TestManualRetryStartsNewPolicyCycleAndKeepsTransientCode(t *testing.T) {
	cfg := testConfig(t)
	ctx := context.Background()
	now := time.Now().In(time.FixedZone("MSK", 3*60*60))
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	submission := testSubmission("66666666-6666-4666-8666-666666666666", now)
	if _, _, err := store.Accept(ctx, submission); err != nil {
		t.Fatal(err)
	}
	delivery, found, err := store.Claim(ctx, domain.SMTP, now, cfg.Policy)
	if err != nil || !found {
		t.Fatalf("Claim() found=%v err=%v", found, err)
	}
	pending := delivery.Job
	pending.State = domain.Pending
	pending.NextAttemptAt = now.Add(time.Minute)
	pending.ErrorCode = "smtp_temporary"
	if err := store.Finish(ctx, submission.ID, domain.SMTP, pending); err != nil {
		t.Fatal(err)
	}
	status, err := store.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status[0].Job.StartedAt.IsZero() || status[0].Job.ErrorCode != "smtp_temporary" {
		t.Fatalf("transient result = %+v, want start time and error code retained", status[0].Job)
	}

	delivery, found, err = store.Claim(ctx, domain.SMTP, now.Add(time.Minute), cfg.Policy)
	if err != nil || !found {
		t.Fatalf("second Claim() found=%v err=%v", found, err)
	}
	failed := delivery.Job
	failed.State = domain.Failed
	failed.ConfirmedAt = now.Add(2 * time.Minute)
	failed.ErrorCode = "smtp_permanent"
	if err := store.Finish(ctx, submission.ID, domain.SMTP, failed); err != nil {
		t.Fatal(err)
	}

	retryAt := now.Add(48 * time.Hour)
	if err := store.Retry(ctx, submission.ID, domain.SMTP, retryAt); err != nil {
		t.Fatal(err)
	}
	delivery, found, err = store.Claim(ctx, domain.SMTP, retryAt, cfg.Policy)
	if err != nil || !found || delivery.Job.Attempts != 1 || !delivery.Job.RetryStartedAt.Equal(retryAt) {
		t.Fatalf("manual retry Claim() = (%+v, %v, %v)", delivery, found, err)
	}
}

func TestCapacityReservesRoomForAcceptedDeliveryStates(t *testing.T) {
	cfg := testConfig(t)
	cfg.MaxBytes = 1024
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	now := time.Now().In(time.FixedZone("MSK", 3*60*60))
	if _, _, err := store.Accept(context.Background(), testSubmission("33333333-3333-4333-8333-333333333333", now)); !errors.Is(err, errCapacity) {
		t.Fatalf("Accept() error = %v, want capacity error", err)
	}
	exported, err := store.Export(context.Background(), time.Time{})
	if err != nil || len(exported) != 0 {
		t.Fatalf("Export() = %d items, %v after rejected acceptance", len(exported), err)
	}
}

func TestDeleteEmailPhysicallyCompactsPersonalData(t *testing.T) {
	cfg := testConfig(t)
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().In(time.FixedZone("MSK", 3*60*60))
	removed := testSubmission("44444444-4444-4444-8444-444444444444", now)
	removed.Message = "private removable message"
	kept := testSubmission("55555555-5555-4555-8555-555555555555", now.Add(time.Second))
	kept.Email = "grace@example.test"
	for _, submission := range []domain.Submission{removed, kept} {
		if _, _, err := store.Accept(ctx, submission); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.DeleteEmail(ctx, removed.Email); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(cfg); !errors.Is(err, application.ErrBusy) {
		t.Fatalf("Open() after compaction error = %v, want ErrBusy", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	journal, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(journal, []byte(removed.Email)) || bytes.Contains(journal, []byte(removed.Message)) {
		t.Fatal("deleted personal data remains in compacted journal")
	}
	store, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, found, err := store.Lookup(ctx, removed.ID); err != nil || found {
		t.Fatalf("deleted submission Lookup() found=%v err=%v", found, err)
	}
	if _, found, err := store.Lookup(ctx, kept.ID); err != nil || !found {
		t.Fatalf("retained submission Lookup() found=%v err=%v", found, err)
	}
}

func TestDeleteEmailPreservesLocalPartCase(t *testing.T) {
	cfg := testConfig(t)
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx := context.Background()
	now := time.Now().In(time.FixedZone("MSK", 3*60*60))
	upper := testSubmission("eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", now)
	upper.Email = "Alice@example.test"
	lower := testSubmission("ffffffff-ffff-4fff-8fff-ffffffffffff", now.Add(time.Second))
	lower.Email = "alice@example.test"
	for _, submission := range []domain.Submission{upper, lower} {
		if _, _, err := store.Accept(ctx, submission); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.DeleteEmail(ctx, "Alice@EXAMPLE.TEST"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.Lookup(ctx, upper.ID); err != nil || found {
		t.Fatalf("case-matched email remains: found=%v err=%v", found, err)
	}
	if _, found, err := store.Lookup(ctx, lower.ID); err != nil || !found {
		t.Fatalf("distinct local-part case was removed: found=%v err=%v", found, err)
	}
}

func TestDeleteEmailCompactsPendingRetryingAndSentJobs(t *testing.T) {
	cfg := testConfig(t)
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx := context.Background()
	now := time.Now().In(time.FixedZone("MSK", 3*60*60))
	pending := testSubmission("12121212-1212-4212-8212-121212121212", now)
	pending.Message = "pending private text"
	retrying := testSubmission("23232323-2323-4232-8232-232323232323", now.Add(time.Second))
	retrying.Message = "retrying private text"
	sent := testSubmission("34343434-3434-4434-8434-343434343434", now.Add(2*time.Second))
	sent.Message = "sent private text"
	for _, submission := range []domain.Submission{pending, retrying, sent} {
		submission.Email = "all-states@example.test"
		if _, _, err := store.Accept(ctx, submission); err != nil {
			t.Fatal(err)
		}
	}
	finishSent := func(channel domain.Channel, claimTime time.Time) domain.Delivery {
		t.Helper()
		delivery, found, err := store.Claim(ctx, channel, claimTime, cfg.Policy)
		if err != nil || !found {
			t.Fatalf("Claim(%s) found=%v err=%v", channel, found, err)
		}
		delivery.Job.State = domain.Sent
		delivery.Job.ConfirmedAt = claimTime.Add(time.Second)
		delivery.Job.ProviderID = "provider-42"
		if err := store.Finish(ctx, delivery.Submission.ID, channel, delivery.Job); err != nil {
			t.Fatal(err)
		}
		return delivery
	}
	finishSent(domain.Telegram, now.Add(3*time.Second))
	retryingDelivery, found, err := store.Claim(ctx, domain.Telegram, now.Add(4*time.Second), cfg.Policy)
	if err != nil || !found || retryingDelivery.Submission.ID != retrying.ID {
		t.Fatalf("retrying Claim() = (%+v, %v, %v)", retryingDelivery, found, err)
	}
	retryingDelivery.Job.State = domain.Pending
	retryingDelivery.Job.NextAttemptAt = now.Add(time.Minute)
	retryingDelivery.Job.ErrorCode = "telegram_temporary"
	if err := store.Finish(ctx, retrying.ID, domain.Telegram, retryingDelivery.Job); err != nil {
		t.Fatal(err)
	}
	finishSent(domain.SMTP, now.Add(5*time.Second))
	finishSent(domain.SMTP, now.Add(7*time.Second))
	finishSent(domain.SMTP, now.Add(9*time.Second))
	if err := store.DeleteEmail(ctx, "all-states@example.test"); err != nil {
		t.Fatal(err)
	}
	if status, err := store.Status(ctx); err != nil || len(status) != 0 {
		t.Fatalf("Status() after deletion = %d deliveries, err=%v", len(status), err)
	}
	journal, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, privateText := range []string{pending.Message, retrying.Message, sent.Message, "all-states@example.test"} {
		if bytes.Contains(journal, []byte(privateText)) {
			t.Errorf("deleted personal data %q remains after compaction", privateText)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}
