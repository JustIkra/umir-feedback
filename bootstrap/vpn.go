package bootstrap

import "github.com/JustIkra/umir-feedback/internal/adapters/vpn"

func VPNCommand(args []string) int {
	return vpn.Command(args)
}
