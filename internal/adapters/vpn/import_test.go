package vpn

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testUUID = "11111111-1111-4111-8111-111111111111"

func TestBuildConfigImportsOnlyVLESSAndRestrictsTelegramConnect(t *testing.T) {
	input := "vless://" + testUUID + "@198.51.100.7:443?encryption=none&security=tls&sni=edge.example&type=tcp#sample"
	configBytes, err := BuildConfig([]byte(input), ProxyCredentials{Username: "proxy-user", Password: "proxy-pass"}, 8080)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(configBytes, &config); err != nil {
		t.Fatal(err)
	}
	if _, ok := config["api"]; ok {
		t.Fatal("control API must be absent")
	}
	logConfig := mapValue(t, config["log"])
	if logConfig["loglevel"] != "none" || logConfig["access"] != "none" || logConfig["dnsLog"] != false {
		t.Fatalf("unexpected logging configuration: %#v", logConfig)
	}
	outbounds := sliceValue(t, config["outbounds"])
	if len(outbounds) != 2 || mapValue(t, outbounds[0])["protocol"] != "blackhole" || mapValue(t, outbounds[1])["protocol"] != "vless" {
		t.Fatalf("outbound order must be blackhole then imported VLESS: %#v", outbounds)
	}
	stream := mapValue(t, mapValue(t, outbounds[1])["streamSettings"])
	if stream["network"] != "tcp" || stream["security"] != "tls" {
		t.Fatalf("unexpected imported stream: %#v", stream)
	}
	routing := mapValue(t, config["routing"])
	if routing["domainStrategy"] != "AsIs" {
		t.Fatalf("domainStrategy = %v, want AsIs", routing["domainStrategy"])
	}
	rules := sliceValue(t, routing["rules"])
	if len(rules) != 2 {
		t.Fatalf("got %d routing rules, want Telegram allow and catch-all", len(rules))
	}
	allow := mapValue(t, rules[0])
	domains := sliceValue(t, allow["domain"])
	if !contains(domains, "full:api.telegram.org") || allow["port"] != "443" || allow["network"] != "tcp" {
		t.Fatalf("unexpected Telegram allow rule: %#v", allow)
	}
	catchAll := mapValue(t, rules[1])
	if catchAll["outboundTag"] != "block" || catchAll["network"] != "tcp,udp" {
		t.Fatalf("catch-all must blackhole: %#v", rules[1])
	}
	inbound := mapValue(t, sliceValue(t, config["inbounds"])[0])
	if inbound["protocol"] != "http" || inbound["port"] != float64(8080) {
		t.Fatalf("unexpected proxy inbound: %#v", inbound)
	}
	users := sliceValue(t, mapValue(t, inbound["settings"])["users"])
	if len(users) != 1 || mapValue(t, users[0])["user"] != "proxy-user" || mapValue(t, users[0])["pass"] != "proxy-pass" {
		t.Fatalf("unexpected proxy credentials: %#v", users)
	}
	if strings.Contains(string(configBytes), "sample") {
		t.Fatal("subscription label must not be imported")
	}
}

func TestBuildConfigRejectsUnsupportedOrUnsafeSubscriptionEntries(t *testing.T) {
	base := "vless://" + testUUID + "@198.51.100.7:443?encryption=none&security=tls&sni=edge.example&type=tcp"
	for name, input := range map[string]string{
		"non-vless":               "vmess://" + testUUID + "@198.51.100.7:443",
		"unknown-option":          base + "&allowInsecure=1",
		"unknown-security":        "vless://" + testUUID + "@198.51.100.7:443?encryption=none&security=none&type=tcp",
		"duplicate-node":          base + "\n" + base,
		"misplaced-xhttp-options": base + "&host=edge.example",
		"misplaced-grpc-options":  strings.Replace(base, "type=tcp", "type=ws", 1) + "&serviceName=unexpected",
		"empty-subscription":      "",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := BuildConfig([]byte(input), ProxyCredentials{Username: "proxy-user", Password: "proxy-pass"}, 8080); err == nil {
				t.Fatal("BuildConfig accepted invalid subscription")
			}
		})
	}
}

func TestBuildConfigImportsXHTTPSettings(t *testing.T) {
	input := "vless://" + testUUID + "@198.51.100.13:443?encryption=none&security=tls&sni=xhttp.example&type=xhttp&host=xhttp.example&path=%2Fstream&mode=stream-up"
	configBytes, err := BuildConfig([]byte(input), ProxyCredentials{Username: "proxy-user", Password: "proxy-pass"}, 8080)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(configBytes, &config); err != nil {
		t.Fatal(err)
	}
	outbound := mapValue(t, sliceValue(t, config["outbounds"])[1])
	stream := mapValue(t, outbound["streamSettings"])
	if stream["network"] != "splithttp" {
		t.Fatalf("XHTTP network = %v, want splithttp", stream["network"])
	}
	settings := mapValue(t, stream["xhttpSettings"])
	if settings["host"] != "xhttp.example" || settings["path"] != "/stream" || settings["mode"] != "stream-up" {
		t.Fatalf("unexpected XHTTP settings: %#v", settings)
	}
}

func TestBuildConfigImportsRealityWithoutOptionalShortID(t *testing.T) {
	input := "vless://" + testUUID + "@node.example:443?encryption=none&security=reality&sni=node.example&fp=chrome&pbk=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA&type=tcp"
	configBytes, err := BuildConfig([]byte(input), ProxyCredentials{Username: "proxy-user", Password: "proxy-pass"}, 8080)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(configBytes, &config); err != nil {
		t.Fatal(err)
	}
	stream := mapValue(t, mapValue(t, sliceValue(t, config["outbounds"])[1])["streamSettings"])
	reality := mapValue(t, stream["realitySettings"])
	if stream["network"] != "tcp" || reality["publicKey"] == "" || reality["serverName"] != "node.example" {
		t.Fatalf("unexpected REALITY settings: %#v", stream)
	}
	if _, ok := reality["shortId"]; ok {
		t.Fatal("missing REALITY short ID must stay omitted")
	}
}

func TestLastKnownGoodExpiresAtConfiguredMaximum(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.FixedZone("MSK", 3*60*60))
	if !LastKnownGoodValid(now.Add(-47*time.Hour), now, 48*time.Hour) {
		t.Fatal("last-known-good config must remain usable before expiry")
	}
	for _, updatedAt := range []time.Time{now.Add(-48 * time.Hour), now.Add(-49 * time.Hour), now.Add(time.Minute)} {
		if LastKnownGoodValid(updatedAt, now, 48*time.Hour) {
			t.Fatalf("last-known-good config unexpectedly valid for timestamp %s", updatedAt)
		}
	}
}

func TestLastKnownGoodRecoveryRejectsExpiredUnsafeAndMissingFiles(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.FixedZone("Europe/Moscow", 3*60*60))
	path := filepath.Join(privateTempDir(t), "config.json")
	if err := os.WriteFile(path, []byte(`{"dummy":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	chownTestFile(t, path)
	if err := os.Chtimes(path, now.Add(-47*time.Hour), now.Add(-47*time.Hour)); err != nil {
		t.Fatal(err)
	}
	usable, err := LastKnownGoodFresh(path, now, 48*time.Hour)
	if err != nil || !usable {
		t.Fatalf("fresh closed config recovery = %v, %v", usable, err)
	}
	for _, age := range []time.Duration{48 * time.Hour, 49 * time.Hour, -time.Minute} {
		if err := os.Chtimes(path, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
		usable, err := LastKnownGoodFresh(path, now, 48*time.Hour)
		if err != nil || usable {
			t.Fatalf("config age %s recovery = %v, %v", age, usable, err)
		}
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LastKnownGoodFresh(path, now, 48*time.Hour); err == nil {
		t.Fatal("recovery accepted a config with group/world permissions")
	}
	if _, err := LastKnownGoodFresh(filepath.Join(privateTempDir(t), "missing"), now, 48*time.Hour); err == nil {
		t.Fatal("recovery accepted a missing config")
	}
	directory := privateTempDir(t)
	ancestorConfig := filepath.Join(directory, "config.json")
	if err := os.WriteFile(ancestorConfig, []byte(`{"dummy":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	chownTestFile(t, ancestorConfig)
	if err := os.Chtimes(ancestorConfig, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(privateTempDir(t), "config-link.json")
	if err := os.Symlink(ancestorConfig, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LastKnownGoodFresh(link, now, 48*time.Hour); err == nil {
		t.Fatal("recovery accepted a symlinked config")
	}
	linkedDirectory := filepath.Join(privateTempDir(t), "linked-dir")
	if err := os.Symlink(directory, linkedDirectory); err != nil {
		t.Fatal(err)
	}
	if _, err := LastKnownGoodFresh(filepath.Join(linkedDirectory, filepath.Base(ancestorConfig)), now, 48*time.Hour); err == nil {
		t.Fatal("recovery accepted a symlinked config ancestor")
	}
}

func contains(values []any, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func mapValue(t *testing.T, value any) map[string]any {
	t.Helper()
	result, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("value has type %T, want object", value)
	}
	return result
}

func sliceValue(t *testing.T, value any) []any {
	t.Helper()
	result, ok := value.([]any)
	if !ok {
		t.Fatalf("value has type %T, want array", value)
	}
	return result
}

func chownTestFile(t *testing.T, path string) {
	t.Helper()
	if err := os.Chown(path, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
}

func privateTempDir(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}
