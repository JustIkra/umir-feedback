package integration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/JustIkra/umir-feedback/internal/adapters/journal"
	mechanisms "github.com/JustIkra/umir-feedback/internal/adapters/runtime"
	"github.com/JustIkra/umir-feedback/internal/application"
	"github.com/JustIkra/umir-feedback/internal/domain"
)

type testClock struct{ at time.Time }

func (c testClock) Now() time.Time { return c.at }

type stableJitter struct{}

func (stableJitter) Delay(d time.Duration) time.Duration { return d }

type provider struct {
	result domain.DeliveryResult
	calls  int
}

func (p *provider) Send(context.Context, domain.Delivery) domain.DeliveryResult {
	p.calls++
	return p.result
}

func TestConcurrentIntakeAndIndependentDeliveryAcrossRestart(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.FixedZone("MSK", 10800))
	clock := testClock{now}
	policy := domain.DefaultRetryPolicy()
	config := journal.Config{Path: filepath.Join(dir, "journal.jsonl"), MaxBytes: 1 << 20, UID: os.Getuid(), GID: os.Getgid(), Policy: policy, Now: clock.Now}
	store, err := journal.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	intake := application.NewIntake(store, clock, mechanisms.Normalizer{}, "v1")
	input := domain.Input{ID: "123e4567-e89b-42d3-a456-426614174000", Name: " e\u0301 ", Email: "User@EXAMPLE.ORG", Message: "hello", Accepted: true, ConsentVersion: "v1"}
	var wg sync.WaitGroup
	results := make(chan bool, 16)
	failures := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, created, err := intake.Submit(context.Background(), input)
			results <- created
			failures <- err
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	created := 0
	for result := range results {
		if result {
			created++
		}
	}
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if created != 1 {
		t.Fatalf("created %d", created)
	}
	records, err := store.Export(context.Background(), time.Time{})
	if err != nil || len(records) != 1 || records[0].Name != "é" || records[0].Email != "User@example.org" {
		t.Fatalf("records %#v %v", records, err)
	}
	changed := input
	changed.Message = "other"
	_, _, err = intake.Submit(context.Background(), changed)
	if !errors.Is(err, application.ErrIdentityConflict) {
		t.Fatalf("conflict %v", err)
	}
	smtp := &provider{result: domain.DeliveryResult{Permanent: true, Code: "smtp_permanent_rejection"}}
	telegram := &provider{result: domain.DeliveryResult{Success: true, ProviderID: "42"}}
	smtpWorker := application.NewWorker(store, clock, stableJitter{}, smtp, domain.SMTP, policy)
	tgWorker := application.NewWorker(store, clock, stableJitter{}, telegram, domain.Telegram, policy)
	if worked, err := smtpWorker.ProcessOne(context.Background()); err != nil || !worked {
		t.Fatalf("smtp %v %v", worked, err)
	}
	if worked, err := tgWorker.ProcessOne(context.Background()); err != nil || !worked {
		t.Fatalf("telegram %v %v", worked, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = journal.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	intake = application.NewIntake(store, clock, mechanisms.Normalizer{}, "v2")
	_, again, err := intake.Submit(context.Background(), input)
	if err != nil || again {
		t.Fatalf("consent rotation replay %v %v", again, err)
	}
	for _, entry := range []struct {
		sender  *provider
		channel domain.Channel
	}{{smtp, domain.SMTP}, {telegram, domain.Telegram}} {
		w := application.NewWorker(store, clock, stableJitter{}, entry.sender, entry.channel, policy)
		if worked, err := w.ProcessOne(context.Background()); err != nil || worked {
			t.Fatalf("terminal resent %v %v", worked, err)
		}
		if entry.sender.calls != 1 {
			t.Fatal("provider duplicate")
		}
	}
	if err := store.Retry(context.Background(), input.ID, domain.SMTP, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	smtp.result = domain.DeliveryResult{Success: true}
	w := application.NewWorker(store, testClock{now.Add(time.Second)}, stableJitter{}, smtp, domain.SMTP, policy)
	if worked, err := w.ProcessOne(context.Background()); err != nil || !worked {
		t.Fatalf("retry %v %v", worked, err)
	}
	jobs, err := store.Status(context.Background())
	if err != nil || len(jobs) != 2 {
		t.Fatalf("jobs %v", err)
	}
	for _, d := range jobs {
		if d.Job.State != domain.Sent {
			t.Fatalf("state %#v", d.Job)
		}
	}
}
