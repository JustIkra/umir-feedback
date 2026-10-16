//go:build integration

package pgintegration

import (
	"testing"

	"github.com/JustIkra/umir-feedback/tests/storecontract"
)

func TestPostgresStoreFollowsTheContract(t *testing.T) {
	var h harness
	storecontract.Run(t, h.contract())
}
