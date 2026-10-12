package vpn

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

var errInvalidSubscription = errors.New("invalid VPN subscription")

type ProxyCredentials struct {
	Username string
	Password string
}

type node struct {
	address  string
	port     uint16
	id       string
	flow     string
	method   string
	security string
	stream   map[string]any
}

func BuildConfig(subscription []byte, credentials ProxyCredentials, listenPort uint16) ([]byte, error) {
	if len(subscription) == 0 || len(subscription) > maxSubscriptionBytes || !utf8.Valid(subscription) {
		return nil, errInvalidSubscription
	}
	if listenPort == 0 || !validCredential(credentials.Username) || !validCredential(credentials.Password) {
		return nil, errInvalidSubscription
	}
	lines, err := subscriptionLines(subscription)
	if err != nil || len(lines) == 0 || len(lines) > 256 {
		return nil, errInvalidSubscription
	}
	nodes := make([]node, 0, len(lines))
	seen := make(map[string]struct{}, len(lines))
	for _, line := range lines {
		parsed, err := parseVLESS(line)
		if err != nil {
			return nil, errInvalidSubscription
		}
		key := parsed.address + ":" + strconv.Itoa(int(parsed.port)) + "/" + parsed.id
		if _, ok := seen[key]; ok {
			return nil, errInvalidSubscription
		}
		seen[key] = struct{}{}
		nodes = append(nodes, parsed)
	}
	return marshalConfig(nodes, credentials, listenPort)
}

func LastKnownGoodValid(updatedAt, now time.Time, maxAge time.Duration) bool {
	return maxAge > 0 && !updatedAt.IsZero() && !updatedAt.After(now) && now.Sub(updatedAt) < maxAge
}

func LastKnownGoodFresh(path string, now time.Time, maxAge time.Duration) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 || info.Mode().Perm() != 0o600 || !currentOwner(info) || !noSymlinkAncestors(path) {
		return false, errors.New("VPN config is not a closed regular file")
	}
	return LastKnownGoodValid(info.ModTime(), now, maxAge), nil
}

func currentOwner(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Getuid()) && stat.Gid == uint32(os.Getgid())
}

func noSymlinkAncestors(path string) bool {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	for current := filepath.Dir(absolute); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
		parent := filepath.Dir(current)
		if parent == current {
			return true
		}
	}
}

func subscriptionLines(data []byte) ([]string, error) {
	text := strings.TrimSpace(string(data))
	if strings.HasPrefix(text, "vless://") {
		return splitLines(text), nil
	}
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		decoded, err := encoding.DecodeString(text)
		if err == nil && utf8.Valid(decoded) && strings.HasPrefix(strings.TrimSpace(string(decoded)), "vless://") {
			return splitLines(strings.TrimSpace(string(decoded))), nil
		}
	}
	return nil, errInvalidSubscription
}

func splitLines(text string) []string {
	lines := strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == '\r' })
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			result = append(result, line)
		}
	}
	return result
}

func parseVLESS(raw string) (node, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "vless" || parsed.Opaque != "" || parsed.User == nil || parsed.User.Username() == "" || parsed.User.String() != parsed.User.Username() || parsed.Fragment != "" && strings.ContainsAny(parsed.Fragment, "\r\n") {
		return node{}, errInvalidSubscription
	}
	if !validVLESSID(parsed.User.Username()) {
		return node{}, errInvalidSubscription
	}
	address := parsed.Hostname()
	port, err := strconv.ParseUint(parsed.Port(), 10, 16)
	if err != nil || port == 0 || address == "" || strings.ContainsAny(address, "\r\n\x00") {
		return node{}, errInvalidSubscription
	}
	if ip := net.ParseIP(address); ip == nil {
		if !validDomainName(address) {
			return node{}, errInvalidSubscription
		}
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return node{}, errInvalidSubscription
	}
	for key, values := range query {
		if len(values) != 1 || !supportedOption(key) || strings.ContainsAny(values[0], "\r\n\x00") {
			return node{}, errInvalidSubscription
		}
	}
	get := func(key string) string { return query.Get(key) }
	encryption := get("encryption")
	if encryption == "" {
		encryption = "none"
	}
	if encryption != "none" {
		return node{}, errInvalidSubscription
	}
	security := get("security")
	if security != "tls" && security != "reality" {
		return node{}, errInvalidSubscription
	}
	method := get("type")
	if method == "" || method == "tcp" {
		method = "raw"
	}
	if method != "raw" && method != "ws" && method != "grpc" && method != "xhttp" {
		return node{}, errInvalidSubscription
	}
	if !validOptionPlacement(query, method, security) {
		return node{}, errInvalidSubscription
	}
	flow := get("flow")
	if flow != "" && (flow != "xtls-rprx-vision" || method != "raw" && method != "xhttp") {
		return node{}, errInvalidSubscription
	}
	network := method
	if network == "raw" {
		network = "tcp"
	}
	if network == "xhttp" {
		network = "splithttp"
	}
	stream := map[string]any{"network": network, "security": security}
	if security == "tls" {
		if method == "grpc" && get("serviceName") == "" {
			return node{}, errInvalidSubscription
		}
		tls := map[string]any{}
		if value := get("sni"); value != "" {
			tls["serverName"] = value
		}
		if value := get("fp"); value != "" {
			tls["fingerprint"] = value
		}
		if value := get("alpn"); value != "" {
			protocols := strings.Split(value, ",")
			for _, protocol := range protocols {
				if protocol != "h2" && protocol != "http/1.1" && protocol != "h3" {
					return node{}, errInvalidSubscription
				}
			}
			tls["alpn"] = protocols
		}
		stream["tlsSettings"] = tls
	} else {
		if method == "ws" {
			return node{}, errInvalidSubscription
		}
		publicKey, shortID := get("pbk"), get("sid")
		if publicKey == "" || get("sni") == "" {
			return node{}, errInvalidSubscription
		}
		reality := map[string]any{"serverName": get("sni"), "publicKey": publicKey}
		if shortID != "" {
			reality["shortId"] = shortID
		}
		if value := get("fp"); value != "" {
			reality["fingerprint"] = value
		}
		if value := get("spx"); value != "" {
			reality["spiderX"] = value
		}
		stream["realitySettings"] = reality
	}
	if method == "xhttp" {
		settings := map[string]any{}
		if host := get("host"); host != "" {
			settings["host"] = host
		}
		if path := get("path"); path != "" {
			settings["path"] = path
		}
		if mode := get("mode"); mode != "" {
			settings["mode"] = mode
		}
		stream["xhttpSettings"] = settings
	}
	if method == "ws" {
		settings := map[string]any{}
		if path := get("path"); path != "" {
			settings["path"] = path
		}
		if host := get("host"); host != "" {
			settings["headers"] = map[string]string{"Host": host}
		}
		stream["wsSettings"] = settings
	}
	if method == "grpc" {
		settings := map[string]any{"serviceName": get("serviceName")}
		if mode := get("mode"); mode != "" {
			if mode != "gun" && mode != "multi" {
				return node{}, errInvalidSubscription
			}
			settings["multiMode"] = mode == "multi"
		}
		stream["grpcSettings"] = settings
	}
	return node{address: address, port: uint16(port), id: parsed.User.Username(), flow: flow, method: method, security: security, stream: stream}, nil
}

func supportedOption(key string) bool {
	switch key {
	case "encryption", "security", "type", "sni", "fp", "pbk", "sid", "spx", "flow", "host", "path", "serviceName", "mode", "alpn", "headerType":
		return true
	default:
		return false
	}
}

func validVLESSID(value string) bool {
	if len(value) == 36 {
		for i, char := range value {
			switch i {
			case 8, 13, 18, 23:
				if char != '-' {
					return false
				}
			default:
				if !isASCIIHexDigit(char) {
					return false
				}
			}
		}
		return true
	}
	if len(value) == 0 || len(value) > 29 {
		return false
	}
	for _, char := range value {
		if !isASCIIAlphaNumeric(char) && !strings.ContainsRune("-_.", char) {
			return false
		}
	}
	return true
}

func validDomainName(value string) bool {
	if len(value) == 0 || len(value) > 253 || strings.Contains(value, "..") {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(value, "."), ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if !isASCIIAlphaNumeric(char) && char != '-' {
				return false
			}
		}
	}
	return true
}

func isASCIIAlphaNumeric(value rune) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}

func isASCIIHexDigit(value rune) bool {
	return value >= '0' && value <= '9' || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F'
}

func validOptionPlacement(query url.Values, method, security string) bool {
	if method != "ws" && method != "xhttp" && (query.Has("host") || query.Has("path")) {
		return false
	}
	if method != "grpc" && query.Has("serviceName") || method != "grpc" && method != "xhttp" && query.Has("mode") {
		return false
	}
	if security != "tls" && query.Has("alpn") {
		return false
	}
	if security != "reality" && (query.Has("pbk") || query.Has("sid") || query.Has("spx")) {
		return false
	}
	if query.Has("headerType") && (method != "raw" || query.Get("headerType") != "none") {
		return false
	}
	if method == "xhttp" && query.Has("mode") {
		switch query.Get("mode") {
		case "auto", "packet-up", "stream-up", "stream-one":
		default:
			return false
		}
	}
	return true
}

func validCredential(value string) bool {
	return value != "" && len(value) <= 256 && !strings.ContainsAny(value, "\r\n\x00")
}

func marshalConfig(nodes []node, credentials ProxyCredentials, listenPort uint16) ([]byte, error) {
	outbounds := []any{map[string]any{"tag": "block", "protocol": "blackhole"}}
	for i, n := range nodes {
		user := map[string]any{"id": n.id, "encryption": "none"}
		if n.flow != "" {
			user["flow"] = n.flow
		}
		users := []any{user}
		outbounds = append(outbounds, map[string]any{
			"tag": fmt.Sprintf("vless-%d", i), "protocol": "vless",
			"settings":       map[string]any{"vnext": []any{map[string]any{"address": n.address, "port": n.port, "users": users}}},
			"streamSettings": n.stream,
		})
	}
	config := map[string]any{
		"log": map[string]any{"loglevel": "none", "access": "none", "dnsLog": false},
		"inbounds": []any{map[string]any{
			"tag": "telegram-connect", "listen": "0.0.0.0", "port": listenPort, "protocol": "http",
			"settings": map[string]any{"users": []any{map[string]any{"user": credentials.Username, "pass": credentials.Password}}, "allowTransparent": false},
		}},
		"outbounds": outbounds,
		"routing": map[string]any{
			"domainStrategy": "AsIs",
			"rules": []any{
				map[string]any{"type": "field", "inboundTag": []string{"telegram-connect"}, "domain": []string{"full:api.telegram.org"}, "port": "443", "network": "tcp", "outboundTag": "vless-0"},
				map[string]any{"type": "field", "network": "tcp,udp", "outboundTag": "block"},
			},
		},
	}
	return json.Marshal(config)
}
