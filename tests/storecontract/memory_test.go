package storecontract_test

import (
	"testing"

	"github.com/JustIkra/umir-feedback/internal/adapters/memory"
	"github.com/JustIkra/umir-feedback/tests/storecontract"
)

func TestMemoryStoreFollowsTheContract(t *testing.T) {
	config := func(s storecontract.Settings) memory.Config {
		return memory.Config{Policy: storecontract.Policy(), Lease: s.Lease, Retention: s.Retention, Quota: s.Quota, Now: s.Clock.Now}
	}
	storecontract.Run(t, storecontract.Harness{
		Open: func(t *testing.T, s storecontract.Settings) storecontract.Store { return memory.New(config(s)) },
		Reopen: func(t *testing.T, store storecontract.Store, _ storecontract.Settings) storecontract.Store {
			m, ok := store.(*memory.Store)
			if !ok {
				t.Fatal("not a memory store")
			}
			m.Restart()
			return m
		},
	})
}
