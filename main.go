package main

import (
	"os"

	"CodeAgent/internal/cli"
)

func main() {
	// cli.Main returns after deferred cancellation and resource cleanup.
	os.Exit(cli.Main(os.Args[1:]))
}
