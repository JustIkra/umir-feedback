package protocol

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

func TestA8ReviewObserveRejectsClockChangeDuringWait(t *testing.T) {
	l := newLive(t)
	body, err := json.Marshal(map[string]any{"submissions": []map[string]any{{"namespace": domain.Anonymous(), "submissionID": submissionID}}, "waitMs": 200})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan Response, 1)
	go func() {
		done <- l.control.execute(context.Background(), Request{Version: 1, Project: project, RequestID: fixtureID, Operation: "feedback.observe", Data: body})
	}()
	time.Sleep(50 * time.Millisecond)
	before := l.clock.Read()
	if code := l.clock.Set(before.InstanceID, before.Generation, "2026-10-13T09:00:01+03:00"); code != "" {
		t.Fatal(code)
	}
	response := <-done
	if response.Error == nil {
		t.Fatalf("feedback.observe succeeded across clock generation %s -> %s: %+v", before.Generation, l.clock.Read().Generation, response.Data)
	}
}

func TestA8ReviewObserveHonorsMinimumPollInterval(t *testing.T) {
	l := newLive(t)
	body, err := json.Marshal(map[string]any{"submissions": []map[string]any{{"namespace": domain.Anonymous(), "submissionID": submissionID}}, "waitMs": 500})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan Response, 1)
	started := time.Now()
	go func() {
		done <- l.control.execute(context.Background(), Request{Version: 1, Project: project, RequestID: fixtureID, Operation: "feedback.observe", Data: body})
	}()
	time.Sleep(30 * time.Millisecond)
	if response := l.submit(t, domain.Anonymous(), payload(domain.Anonymous(), "Synthetic")); response.Code != 201 {
		t.Fatal("intake failed", response.Code)
	}
	response := <-done
	elapsed := time.Since(started)
	if response.Error != nil {
		t.Fatal(response.Error)
	}
	observation, ok := response.Data.(Observation)
	if !ok || !observation.Complete {
		t.Fatal("observation unavailable", response.Data)
	}
	if elapsed < 95*time.Millisecond {
		t.Fatalf("missing pair was observed after %s; approved minimum poll interval is 100ms", elapsed)
	}
}
