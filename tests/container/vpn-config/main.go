package main

import (
	"os"
	"strings"

	"github.com/JustIkra/umir-feedback/internal/adapters/vpn"
)

func main() {
	if len(os.Args) != 2 {
		os.Exit(2)
	}
	subscription := strings.Join([]string{
		"vless://11111111-1111-4111-8111-111111111111@198.51.100.10:443?encryption=none&security=tls&type=tcp&sni=one.example&fp=chrome&alpn=h2%2Chttp%2F1.1#tcp-tls",
		"vless://22222222-2222-4222-8222-222222222222@198.51.100.11:443?encryption=none&security=tls&type=xhttp&sni=two.example&host=two.example&path=%2Fconnect&mode=stream-up#xhttp-tls",
		"vless://33333333-3333-4333-8333-333333333333@node-with-sid.example:443?encryption=none&security=reality&sni=node-with-sid.example&fp=chrome&pbk=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA&sid=0123456789abcdef&type=tcp#reality-with-short-id",
		"vless://44444444-4444-4444-8444-444444444444@node.example:443?encryption=none&security=reality&sni=node.example&fp=chrome&pbk=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA&type=tcp#reality-no-short-id",
		"vless://55555555-5555-4555-8555-555555555555@198.51.100.15:443?encryption=none&security=tls&type=ws&sni=five.example&host=five.example&path=%2Fconnect#ws-tls",
		"vless://66666666-6666-4666-8666-666666666666@198.51.100.16:443?encryption=none&security=reality&type=grpc&sni=six.example&fp=chrome&pbk=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA&sid=abcdef0123456789&serviceName=connect#grpc-reality",
	}, "\n")
	config, err := vpn.BuildConfig([]byte(subscription), vpn.ProxyCredentials{Username: "fixture-user", Password: "fixture-password"}, 8080)
	if err != nil {
		fail()
	}
	if err := os.WriteFile(os.Args[1], config, 0o600); err != nil {
		fail()
	}
}

func fail() {
	os.Exit(1)
}
