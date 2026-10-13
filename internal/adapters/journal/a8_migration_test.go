package journal

import (
	"bytes"
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

func TestA8LegacyUnscopedMigrationPreservesFingerprintJobsAndTransitions(t *testing.T) {
	cfg := testConfig(t)
	at := time.Date(2026, 10, 13, 9, 0, 0, 0, moscowLocation())
	cfg.Now = func() time.Time { return at }
	sub := testSubmission("123e4567-e89b-42d3-a456-426614174000", at)
	wire := toWireSubmission(sub)
	wire.Namespace = nil
	wire.CommandID = ""
	wire.Legacy = false
	header, e := encodeRecord(record{Version: formatVersion, Type: "header"})
	if e != nil {
		t.Fatal(e)
	}
	accepted, e := encodeRecord(record{Version: formatVersion, Type: "accept", Submission: wire, Jobs: []wireJob{{Channel: "smtp", State: "pending", NextAttemptAt: at}, {Channel: "telegram", State: "pending", NextAttemptAt: at}}})
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(cfg.Path, append(header, accepted...), 0600); e != nil {
		t.Fatal(e)
	}
	s, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		if s != nil {
			if e := s.Close(); e != nil {
				t.Error(e)
			}
		}
	}()
	pairs := []Selector{{Namespace: domain.Anonymous(), ID: sub.ID}}
	before, complete, e := s.Observe(context.Background(), pairs)
	if e != nil || !complete || before[0].Fingerprint != sub.Fingerprint || before[0].Namespace != domain.Anonymous() {
		t.Fatal("legacy record invented scope or fingerprint")
	}
	d, found, e := s.Claim(context.Background(), domain.SMTP, at, cfg.Policy)
	if e != nil || !found {
		t.Fatal("legacy job unavailable")
	}
	d.Job.State = domain.Sent
	d.Job.ConfirmedAt = at
	if e = s.Finish(context.Background(), sub.ID, domain.SMTP, d.Job); e != nil {
		t.Fatal(e)
	}
	if e = s.compactForTest(); e != nil {
		t.Fatal(e)
	}
	snapshot, complete, e := s.Observe(context.Background(), pairs)
	if e != nil || !complete {
		t.Fatal(e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	s, e = Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	after, complete, e := s.Observe(context.Background(), pairs)
	if e != nil || !complete || !reflect.DeepEqual(snapshot, after) {
		t.Fatal("migration/compaction/reopen changed legacy job facts")
	}
	if after[0].Jobs[0].JobID != before[0].Jobs[0].JobID || after[0].Jobs[1].JobID != before[0].Jobs[1].JobID {
		t.Fatal("migration replaced jobs")
	}
	scoped, e := s.ListScoped(context.Background(), "33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444")
	if e != nil || len(scoped) != 0 {
		t.Fatal("legacy record leaked into scoped export")
	}
}
func (s *Store) compactForTest() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.compact(s.entries, s.challenges)
}

func TestA8RecoveryRejectsCrossNamespaceJobAlias(t *testing.T) {
	cfg := testConfig(t)
	at := time.Date(2026, 10, 13, 9, 0, 0, 0, moscowLocation())
	s, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	one := testSubmission("123e4567-e89b-42d3-a456-426614174000", at)
	one.Namespace = domain.Namespace{Kind: "app", UserID: "11111111-1111-4111-8111-111111111111", OrgID: "22222222-2222-4222-8222-222222222222", TenantID: "33333333-3333-4333-8333-333333333333"}
	two := one
	two.Namespace.TenantID = "44444444-4444-4444-8444-444444444444"
	for _, sub := range []domain.Submission{one, two} {
		if _, _, e := s.Accept(context.Background(), sub); e != nil {
			t.Fatal(e)
		}
	}
	rows, _, e := s.Observe(context.Background(), []Selector{{one.Namespace, one.ID}, {two.Namespace, two.ID}})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	data, e := os.ReadFile(cfg.Path)
	if e != nil {
		t.Fatal(e)
	}
	data = bytes.ReplaceAll(data, []byte(rows[1].Jobs[0].JobID), []byte(rows[0].Jobs[0].JobID))
	if e = os.WriteFile(cfg.Path, data, 0600); e != nil {
		t.Fatal(e)
	}
	reopened, e := Open(cfg)
	if e == nil {
		if ce := reopened.Close(); ce != nil {
			t.Fatal(ce)
		}
		t.Fatal("journal recovery accepted an aliased job across namespaces")
	}
}
