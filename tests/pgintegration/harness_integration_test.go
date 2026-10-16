//go:build integration

// Package pgintegration runs the PostgreSQL store against a real server with
// real owner and runtime roles (R8). Every test creates its own database,
// roles and, where needed, virtual host; nothing is shared and nothing is skipped.
package pgintegration

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/JustIkra/umir-feedback/internal/adapters/pgstore"
	"github.com/JustIkra/umir-feedback/internal/adapters/postgres"
	mechanisms "github.com/JustIkra/umir-feedback/internal/adapters/runtime"
	"github.com/JustIkra/umir-feedback/internal/application"
	"github.com/JustIkra/umir-feedback/internal/domain"
	"github.com/JustIkra/umir-feedback/tests/storecontract"
	"github.com/JustIkra/umir-feedback/tests/testsupport/itest"
)

// env is one migrated database with its roles.
type env struct {
	roles *itest.Roles
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{roles: itest.NewRoles(t)}
	owner := e.openDB(t, e.roles.OwnerURLGranting(), 2)
	if err := postgres.Migrate(t.Context(), owner, pgstore.Spec()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return e
}

func (e *env) openDB(t testing.TB, url string, conns int32) *postgres.DB {
	t.Helper()
	db, err := postgres.Open(context.Background(), postgres.Config{URL: url, ApplicationName: "t-feedback", MaxConns: conns, ConnectTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

func (e *env) config(t testing.TB, db *postgres.DB, st storecontract.Settings) pgstore.Config {
	cfg := pgstore.Config{
		DB: db, Policy: storecontract.Policy(), Lease: st.Lease, Retention: st.Retention, Quota: st.Quota,
		NewID: mechanisms.NewUUIDv4,
	}
	if st.Clock != nil {
		cfg.Now = st.Clock.Now
	} else {
		clock, err := mechanisms.NewClock()
		if err != nil {
			t.Fatal(err)
		}
		cfg.Now = clock.Now
	}
	if cfg.Retention == (application.RetentionConfig{}) {
		cfg.Retention = application.RetentionConfig{AnonymousDays: 30, RestaurantDays: 180, DeletedScopeDays: 7, TombstoneDays: 180}
	}
	if cfg.Quota == (application.QuotaConfig{}) {
		cfg.Quota = application.QuotaConfig{Anonymous: 150000, Restaurant: 5000}
	}
	return cfg
}

func (e *env) store(t testing.TB, st storecontract.Settings) *pgstore.Store {
	t.Helper()
	db := e.openDB(t, e.roles.RuntimeURL, 8)
	store, err := pgstore.Open(context.Background(), e.config(t, db, st))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return store
}

func moscow(t time.Time) time.Time { return t.In(domain.MoscowLocation()) }

type harness struct {
	mu   sync.Mutex
	envs map[*pgstore.Store]*env
}

func (h *harness) contract() storecontract.Harness {
	h.envs = map[*pgstore.Store]*env{}
	return storecontract.Harness{
		Open: func(t *testing.T, st storecontract.Settings) storecontract.Store {
			e := newEnv(t)
			s := e.store(t, st)
			h.mu.Lock()
			h.envs[s] = e
			h.mu.Unlock()
			return s
		},
		Reopen: func(t *testing.T, old storecontract.Store, st storecontract.Settings) storecontract.Store {
			previous, ok := old.(*pgstore.Store)
			if !ok {
				t.Fatal("not a PostgreSQL store")
			}
			h.mu.Lock()
			e := h.envs[previous]
			h.mu.Unlock()
			if err := previous.Close(); err != nil {
				t.Fatal(err)
			}
			s := e.store(t, st)
			h.mu.Lock()
			h.envs[s] = e
			h.mu.Unlock()
			return s
		},
	}
}
