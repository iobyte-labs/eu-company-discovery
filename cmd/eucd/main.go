// Command eucd is the command-line interface to the company discovery engine.
//
// It discovers companies through configured association directories, resolves their
// official websites, extracts contacts those companies published, and exports the result
// under a versioned schema.
//
// It writes no applications and sends no mail.
package main

import (
	"os"

	"github.com/iobyte-labs/eu-company-discovery/internal/cli"
)

func main() {
	os.Exit(cli.Execute(os.Args[1:], os.Stdout, os.Stderr))
}
