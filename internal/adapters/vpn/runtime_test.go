package vpn

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestRefreshTunnelPausesBeforeUpdateAndDoesNotRestartExpiredCache(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.FixedZone("Europe/Moscow", 3*60*60))
	configPath := filepath.Join(privateTempDir(t), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"dummy":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	chownTestFile(t, configPath)
	if err := os.Chtimes(configPath, now.Add(-47*time.Hour), now.Add(-47*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var steps []string
	updateErr := errors.New("synthetic refresh failure")
	updated, err := refreshTunnel(configPath, 48*time.Hour, func() time.Time { return now }, func() error {
		steps = append(steps, "stop")
		return nil
	}, func() error {
		steps = append(steps, "update")
		if err := os.Chtimes(configPath, now.Add(-49*time.Hour), now.Add(-49*time.Hour)); err != nil {
			return err
		}
		return updateErr
	}, func() { steps = append(steps, "start") })
	if err != nil || updated {
		t.Fatalf("failed update reported updated=%v, error %v", updated, err)
	}
	if !reflect.DeepEqual(steps, []string{"stop", "update"}) {
		t.Fatalf("refresh sequence = %v, want stop/update with expired cache kept stopped", steps)
	}
}

func TestRefreshTunnelRestartsFreshLastKnownGoodAfterUpdateFailure(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.FixedZone("Europe/Moscow", 3*60*60))
	configPath := filepath.Join(privateTempDir(t), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"dummy":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	chownTestFile(t, configPath)
	if err := os.Chtimes(configPath, now.Add(-47*time.Hour), now.Add(-47*time.Hour)); err != nil {
		t.Fatal(err)
	}
	started := false
	_, err := refreshTunnel(configPath, 48*time.Hour, func() time.Time { return now }, func() error { return nil }, func() error {
		return errors.New("synthetic refresh failure")
	}, func() { started = true })
	if err != nil || !started {
		t.Fatalf("fresh cache restart = started %v, error %v", started, err)
	}
}

func TestRefreshTunnelReportsSuccessfulUpdate(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.FixedZone("Europe/Moscow", 3*60*60))
	configPath := filepath.Join(privateTempDir(t), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"dummy":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	chownTestFile(t, configPath)
	if err := os.Chtimes(configPath, now, now); err != nil {
		t.Fatal(err)
	}
	started := false
	updated, err := refreshTunnel(configPath, 48*time.Hour, func() time.Time { return now }, func() error { return nil }, func() error { return nil }, func() { started = true })
	if err != nil || !updated || !started {
		t.Fatalf("successful update = updated %v, started %v, error %v", updated, started, err)
	}
}

func TestSecureStateDirRejectsUnsafePermissionsWithoutRepair(t *testing.T) {
	directory := privateTempDir(t)
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	chownTestFile(t, directory)
	if err := secureStateDir(directory); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := secureStateDir(directory); err == nil {
		t.Fatal("state directory with group/world permissions was accepted")
	}
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("state directory permissions were repaired to %04o", info.Mode().Perm())
	}
}

func TestProxyCredentialsPreserveSpacesAndRemoveOnlyFileNewline(t *testing.T) {
	credentials, err := parseProxyCredentials([]byte(" proxy-user \r\n"), []byte(" proxy-password \n"))
	if err != nil || credentials.Username != " proxy-user " || credentials.Password != " proxy-password " {
		t.Fatalf("credentials changed: %v", err)
	}
}
