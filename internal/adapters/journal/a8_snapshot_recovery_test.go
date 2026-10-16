package journal

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

func TestA8LegacySnapshotCompactionDeliveryAndMultipleReopens(t *testing.T) {
	cfg, sub := a8LegacySnapshotBaseline(t)
	ctx := context.Background()
	now := cfg.Now()
	cfg.Now = func() time.Time { return now }
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s != nil {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		}
	}()
	selectors := []Selector{{Namespace: domain.Anonymous(), ID: sub.ID}}
	original, complete, err := s.Observe(ctx, selectors)
	if err != nil || !complete || original[0].Jobs[0].JobID == "" || original[0].Jobs[1].JobID == "" || original[0].Jobs[0].JobID == original[0].Jobs[1].JobID {
		t.Fatal("legacy snapshot has no independent jobs", err)
	}
	evidence := [][]RecordView{original}
	reopen := func(compact bool) {
		t.Helper()
		if compact {
			if err := s.compactForTest(); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s = nil
		s, err = Open(cfg)
		if err != nil {
			t.Fatal(err)
		}
		rows, complete, err := s.Observe(ctx, selectors)
		if err != nil || !complete {
			t.Fatal(err)
		}
		row, initial := rows[0], original[0]
		if row.Namespace != domain.Anonymous() || row.SubmissionID != sub.ID || row.CommandID != string(sub.ID) || row.Fingerprint != sub.Fingerprint || !row.ReceivedAt.Equal(sub.ReceivedAt) {
			t.Fatal("migration changed original submission facts")
		}
		for i, job := range row.Jobs {
			if job.JobID != initial.Jobs[i].JobID || job.Channel != initial.Jobs[i].Channel || !job.AcceptedAt.Equal(initial.Jobs[i].AcceptedAt) {
				t.Fatal("compaction or reopen reassigned a job edge")
			}
		}
		evidence = append(evidence, rows)
	}
	reopen(true)
	for _, channel := range []domain.Channel{domain.SMTP, domain.Telegram} {
		claimed, found, err := s.Claim(ctx, channel, now, cfg.Policy)
		if err != nil || !found || claimed.Job.Attempts != 1 {
			t.Fatal("first delivery claim", found, err)
		}
		reopen(false)
		status, err := s.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, delivery := range status {
			if delivery.Job.ID == claimed.Job.ID && (delivery.Job.State != domain.Pending || delivery.Job.Attempts != 1 || !delivery.Job.NextAttemptAt.Equal(now.Add(time.Second))) {
				t.Fatal("recovery lost retained delivery timeline")
			}
		}
		now = now.Add(time.Second)
		continued, found, err := s.Claim(ctx, channel, now, cfg.Policy)
		if err != nil || !found || continued.Job.ID != claimed.Job.ID || continued.Job.Attempts != 2 {
			t.Fatal("continued delivery changed original job", found, err)
		}
		continued.Job.State = domain.Sent
		continued.Job.ConfirmedAt = now
		if err := s.FinishScoped(ctx, domain.Anonymous(), sub.ID, channel, continued.Job); err != nil {
			t.Fatal(err)
		}
		reopen(true)
	}
	final, _, err := s.Observe(ctx, selectors)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range final[0].Jobs {
		if job.State != domain.Sent || job.Attempts != 2 {
			t.Fatal("confirmed delivery history lost")
		}
	}
	reopen(true)
	if !reflect.DeepEqual(final, evidence[len(evidence)-1]) {
		t.Fatal("final compaction changed confirmed facts")
	}
	if dir := os.Getenv("A8_EVIDENCE_DIR"); dir != "" {
		data, err := json.MarshalIndent(evidence, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "legacy-snapshot-reopens.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestA8MixedSnapshotAcceptRejectsAliasesInBothOrders(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		for _, crossChannel := range []bool{false, true} {
			t.Run(stringBool(reverse)+"-"+stringBool(crossChannel), func(t *testing.T) {
				cfg := testConfig(t)
				at := time.Date(2026, 10, 13, 9, 0, 0, 0, domain.MoscowLocation())
				cfg.Now = func() time.Time { return at }
				s, err := Open(cfg)
				if err != nil {
					t.Fatal(err)
				}
				one := testSubmission("123e4567-e89b-42d3-a456-426614174000", at)
				one.Namespace = domain.Namespace{Kind: "app", UserID: "11111111-1111-4111-8111-111111111111", OrgID: "22222222-2222-4222-8222-222222222222", TenantID: "33333333-3333-4333-8333-333333333333"}
				two := one
				two.Namespace.TenantID = "44444444-4444-4444-8444-444444444444"
				if _, _, err := s.Accept(context.Background(), one); err != nil {
					t.Fatal(err)
				}
				if err := s.compactForTest(); err != nil {
					t.Fatal(err)
				}
				if _, _, err := s.Accept(context.Background(), two); err != nil {
					t.Fatal(err)
				}
				rows, _, err := s.Observe(context.Background(), []Selector{{one.Namespace, one.ID}, {two.Namespace, two.ID}})
				if err != nil {
					t.Fatal(err)
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(cfg.Path)
				if err != nil {
					t.Fatal(err)
				}
				channel := 0
				if crossChannel {
					channel = 1
				}
				data = bytes.ReplaceAll(data, []byte(rows[1].Jobs[channel].JobID), []byte(rows[0].Jobs[0].JobID))
				if reverse {
					lines := bytes.Split(data, []byte("\n"))
					lines[1], lines[2] = lines[2], lines[1]
					data = bytes.Join(lines, []byte("\n"))
				}
				if err := os.WriteFile(cfg.Path, data, 0600); err != nil {
					t.Fatal(err)
				}
				reopened, err := Open(cfg)
				if err == nil {
					if err := reopened.Close(); err != nil {
						t.Fatal(err)
					}
					t.Fatal("mixed snapshot/accept admitted a global job alias")
				}
			})
		}
	}
}

func stringBool(v bool) string {
	if v {
		return "true"
	}
	return "false"
}
