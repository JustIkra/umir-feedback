package journal

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestA8DurableIndependentJobIdentity(t *testing.T) {
	cfg := testConfig(t)
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	}()
	_, _, err = s.Accept(context.Background(), testSubmission("123e4567-e89b-42d3-a456-426614174000", time.Date(2026, 10, 13, 9, 0, 0, 0, moscowLocation())))
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := s.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, d := range jobs {
		v := reflect.ValueOf(d.Job).FieldByName("ID")
		if !v.IsValid() || v.String() == "" {
			t.Fatal("accepted job has no durable independent job ID")
		}
		if ids[v.String()] {
			t.Fatal("job IDs alias")
		}
		ids[v.String()] = true
	}
}
