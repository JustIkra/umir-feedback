package application

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type RegistryTombstone struct {
	RestaurantID     string
	OrgID            string
	LifecycleVersion int64
}

type RegistryPage struct {
	Revision   int64
	Complete   bool
	NextAfter  string
	Tombstones []RegistryTombstone
}

type RegistrySource interface {
	Changes(context.Context, int64, string) (RegistryPage, error)
}

type RegistryState interface {
	RegistryRevision(context.Context) (int64, error)
	ApplyRegistry(context.Context, int64, []ScopeDeleted, time.Time) error
}

type RegistryReconciler struct {
	store  RegistryState
	source RegistrySource
	now    func() time.Time
}

func NewRegistryReconciler(store RegistryState, source RegistrySource, now func() time.Time) *RegistryReconciler {
	return &RegistryReconciler{store: store, source: source, now: now}
}

func (r *RegistryReconciler) RunOnce(ctx context.Context) error {
	if r.store == nil || r.source == nil || r.now == nil {
		return errors.New("registry reconciler dependencies are required")
	}
	last, err := r.store.RegistryRevision(ctx)
	if err != nil {
		return fmt.Errorf("read registry revision: %w", err)
	}
	removed := make(map[string]RegistryTombstone)
	stableRevision := last
	for {
		pageRevision, err := r.readPass(ctx, stableRevision, removed)
		if err != nil {
			return err
		}
		if pageRevision <= stableRevision {
			break
		}
		stableRevision = pageRevision
	}
	events := make([]ScopeDeleted, 0, len(removed))
	for _, tombstone := range removed {
		events = append(events, ScopeDeleted{
			Kind: ScopeRestaurant, ID: tombstone.RestaurantID,
			EventID:  fmt.Sprintf("registry:%s:%d", tombstone.RestaurantID, tombstone.LifecycleVersion),
			Revision: stableRevision, DeletedAt: r.now(),
		})
	}
	if err := r.store.ApplyRegistry(ctx, stableRevision, events, r.now()); err != nil {
		return fmt.Errorf("apply Auth registry: %w", err)
	}
	return nil
}

func (r *RegistryReconciler) readPass(ctx context.Context, since int64, removed map[string]RegistryTombstone) (int64, error) {
	after := ""
	pageRevision := int64(0)
	for {
		page, err := r.source.Changes(ctx, since, after)
		if err != nil {
			return 0, fmt.Errorf("read Auth registry changes: %w", err)
		}
		if page.Revision < since || page.Revision < pageRevision || (!page.Complete && (page.NextAfter == "" || page.NextAfter == after)) || (page.Complete && page.NextAfter != "") {
			return 0, errors.New("Auth registry returned an invalid page sequence")
		}
		if pageRevision == 0 {
			pageRevision = page.Revision
		}
		for _, tombstone := range page.Tombstones {
			if tombstone.RestaurantID == "" || tombstone.OrgID == "" || tombstone.LifecycleVersion < 1 {
				return 0, errors.New("Auth registry returned an invalid tombstone")
			}
			removed[tombstone.RestaurantID] = tombstone
		}
		if page.Complete {
			return pageRevision, nil
		}
		after = page.NextAfter
	}
}
