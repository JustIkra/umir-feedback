//go:build integration

package itest_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/JustIkra/umir-feedback/tests/testsupport/itest"
)

func TestRolesAreRealAndSeparate(t *testing.T) {
	r := itest.NewRoles(t)
	for name, url := range map[string]string{"owner": r.OwnerURL, "runtime": r.RuntimeURL} {
		conn, err := pgx.Connect(context.Background(), url)
		if err != nil {
			t.Fatalf("%s role cannot connect: %v", name, err)
		}
		var user string
		if err := conn.QueryRow(context.Background(), "SELECT current_user").Scan(&user); err != nil {
			t.Fatal(err)
		}
		if err := conn.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if (name == "owner") != (user == r.OwnerRole) {
			t.Fatalf("%s connected as %s", name, user)
		}
	}
}

func TestVHostIsUsable(t *testing.T) {
	if itest.NewVHost(t) == "" {
		t.Fatal("empty URL")
	}
}
