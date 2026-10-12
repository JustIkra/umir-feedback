package main

import (
	"context"
	"fmt"
	"os"

	"github.com/JustIkra/umir-feedback/bootstrap"
)

func main() {
	if err := bootstrap.Command(context.Background(), os.Args[1:], os.Getenv, os.Stdout); err != nil {
		if _, writeErr := fmt.Fprintln(os.Stderr, err); writeErr != nil {
			os.Exit(1)
		}
		os.Exit(1)
	}
}
