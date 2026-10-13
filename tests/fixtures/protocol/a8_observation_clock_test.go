package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

func TestA8CachedObservationRetainsOriginalBytesAfterClockAdvance(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(stringComplete(complete), func(t *testing.T) {
			l := newLive(t)
			if complete {
				if reply := l.submit(t, domain.Anonymous(), payload(domain.Anonymous(), "Synthetic")); reply.Code != 201 {
					t.Fatal("intake refused")
				}
			}
			id, err := UUID()
			if err != nil {
				t.Fatal(err)
			}
			original := l.observe(t, id, []domain.Namespace{domain.Anonymous()})
			before := l.clock.Read()
			clockID, err := UUID()
			if err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(map[string]any{"version": 1, "project": project, "requestID": clockID, "operation": "clock.set", "data": map[string]string{"expectedInstanceID": before.InstanceID, "expectedGeneration": before.Generation, "instant": before.Instant}})
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest("POST", ControlPath, bytes.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+controlKey)
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			l.control.ServeHTTP(w, r)
			if w.Code != 200 || l.clock.Read().Generation == before.Generation {
				t.Fatal("real clock control did not advance generation")
			}
			if !bytes.Equal(original, l.observe(t, id, []domain.Namespace{domain.Anonymous()})) {
				t.Fatal("exact cached control replay was replaced by a fresh observation")
			}
			fresh, err := UUID()
			if err != nil {
				t.Fatal(err)
			}
			next := decodeObservation(t, l.observe(t, fresh, []domain.Namespace{domain.Anonymous()}))
			prior := decodeObservation(t, original)
			if next.SnapshotID == prior.SnapshotID || next.SnapshotSHA256 != prior.SnapshotSHA256 || next.Complete != complete {
				t.Fatal("fresh stable-clock read changed the committed prefix")
			}
		})
	}
}

func TestA8ObserveShortBudgetAndCanceledContext(t *testing.T) {
	l := newLive(t)
	body, err := json.Marshal(map[string]any{"submissions": []map[string]any{{"namespace": domain.Anonymous(), "submissionID": submissionID}}, "waitMs": 25})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	observation, code := l.control.observe(context.Background(), body)
	elapsed := time.Since(started)
	if code != "" || observation.Complete || len(observation.Records) != 0 || elapsed < 25*time.Millisecond || elapsed >= 100*time.Millisecond {
		t.Fatalf("short budget was extended or invented completion: %s complete=%v elapsed=%s", code, observation.Complete, elapsed)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, code := l.control.observe(ctx, body); code != "WaitExpired" {
		t.Fatal("canceled observation returned successful data", code)
	}
}

func stringComplete(v bool) string {
	if v {
		return "complete"
	}
	return "incomplete"
}
