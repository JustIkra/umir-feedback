package application

import (
	"context"
	"testing"
	"time"
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
	if store.seen[0].Kind != ScopeRestaurant || store.seen[0].ID != "rest-1" || store.seen[0].Revision != 5 || !store.seen[0].DeletedAt.Equal(now) {
		t.Fatalf("unexpected first tombstone: %#v", store.seen[0])
	}
}
