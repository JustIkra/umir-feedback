package main

import (
	"fmt"
	"os"

	"github.com/JustIkra/umir-feedback/tests/fixtures/protocol"
)

func main() {
	if len(os.Args) != 2 || os.Args[1] != "request" {
		fail()
	}
	if e := protocol.RequestFromStdin(); e != nil {
		fail()
	}
}
func fail() {
	if _, e := fmt.Fprintln(os.Stderr, "Feedback fixture control unavailable"); e != nil {
		os.Exit(1)
	}
	os.Exit(1)
}
