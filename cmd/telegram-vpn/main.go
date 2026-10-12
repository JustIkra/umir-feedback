package main

import (
	"os"

	"github.com/JustIkra/umir-feedback/bootstrap"
)

func main() {
	os.Exit(bootstrap.VPNCommand(os.Args[1:]))
}
