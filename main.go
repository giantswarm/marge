package main

import (
	"fmt"
	"os"

	"github.com/giantswarm/marge/cmd"
	"github.com/giantswarm/marge/pkg/project"
)

func main() {
	cmd.SetVersion(project.Version())

	if err := cmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
