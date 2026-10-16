package application

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

type registrySourceFunc func(context.Context, int64, string) (RegistryPage, error)

func (f registrySourceFunc) Changes(ctx context.Context, since int64, after string) (RegistryPage, error) {
	return f(ctx, since, after)
}

type registryStoreFake struct {
	revision int64
	applied  int
	seen     []ScopeDeleted
}

func (s *registryStoreFake) RegistryRevision(context.Context) (int64, error) { return s.revision, nil }
func (s *registryStoreFake) ApplyRegistry(_ context.Context, revision int64, events []ScopeDeleted, _ time.Time) error {
	s.applied++
	s.revision = revision
	s.seen = append(s.seen, events...)
	return nil
}

func TestRegistryReconcilerCatchesUpAcrossPagesAndRevisions(t *testing.T) {
	store := &registryStoreFake{revision: 3}
	calls := 0
	source := registrySourceFunc(func(_ context.Context, since int64, after string) (RegistryPage, error) {
		calls++
		switch {
		case since == 3 && after == "":
			return RegistryPage{Revision: 4, Complete: false, NextAfter: "next", Tombstones: []RegistryTombstone{{RestaurantID: "rest-1", OrgID: "org-1", LifecycleVersion: 7}}}, nil
		case since == 3 && after == "next":
			return RegistryPage{Revision: 4, Complete: true}, nil
		case since == 4 && after == "":
			return RegistryPage{Revision: 5, Complete: true, Tombstones: []RegistryTombstone{{RestaurantID: "rest-2", OrgID: "org-2", LifecycleVersion: 8}}}, nil
		case since == 5 && after == "":
			return RegistryPage{Revision: 5, Complete: true}, nil
		default:
			t.Fatalf("unexpected cursor since=%d after=%q", since, after)
			return RegistryPage{}, nil
		}
	})
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.FixedZone("MSK", 3*60*60))
	reconciler := NewRegistryReconciler(store, source, func() time.Time { return now })
	if err := reconciler.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.applied != 1 || store.revision != 5 || calls != 4 || len(store.seen) != 2 {
		t.Fatalf("unexpected reconciliation: store=%#v calls=%d", store, calls)
	}
	seen := make(map[string]ScopeDeleted, len(store.seen))
	for _, event := range store.seen {
		seen[event.ID] = event
	}
	for id, revision := range map[string]int64{"rest-1": 7, "rest-2": 8} {
		event := seen[id]
		if event.Kind != ScopeRestaurant || event.Revision != revision || !event.DeletedAt.Equal(now) {
			t.Fatalf("unexpected tombstone for %s: %#v", id, event)
		}
	}
}

func TestRegistryReconcilerRejectsCursorCycleAcrossPagePass(t *testing.T) {
	store := &registryStoreFake{revision: 3}
	calls := 0
	source := registrySourceFunc(func(_ context.Context, _ int64, after string) (RegistryPage, error) {
		calls++
		if calls > 3 {
			return RegistryPage{}, errors.New("request continued after cursor cycle")
		}
		next := map[string]string{"": "cursor-a", "cursor-a": "cursor-b", "cursor-b": "cursor-a"}
		if cursor, ok := next[after]; ok {
			return RegistryPage{Revision: 3, Complete: false, NextAfter: cursor}, nil
		}
		return RegistryPage{}, errors.New("request continued after cursor cycle")
	})
	reconciler := NewRegistryReconciler(store, source, func() time.Time { return time.Now().In(domain.MoscowLocation()) })
	err := reconciler.RunOnce(context.Background())
	if err == nil || calls != 3 || !strings.Contains(err.Error(), "invalid page sequence") {
		t.Fatalf("cursor cycle was not rejected safely: calls=%d err=%v", calls, err)
	}
}
