// Command sabon-mover runs a single restic operation inside an ephemeral mover
// container. The orchestrator spawns it from the mover image with the job
// encoded in SABON_MOVER_SPEC; see internal/mover for the contract.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/davidborzek/sabon/internal/mover"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println("sabon-mover", version)
		return
	}
	if err := mover.Execute(context.Background(), os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "sabon-mover:", err)
		os.Exit(1)
	}
}
