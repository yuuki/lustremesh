// SPDX-License-Identifier: Apache-2.0

// Command lustremesh probes the targets of a Lustre file system from a client.
package main

import (
	"os"

	"github.com/yuuki/lustremesh/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
