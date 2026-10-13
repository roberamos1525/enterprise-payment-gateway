// Command gatewayctl is the operator CLI for the simulated payment gateway.
package main

import (
	"os"

	"github.com/robertramos/enterprise-payment-gateway/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], cli.Env{Stdout: os.Stdout, Stderr: os.Stderr}))
}
