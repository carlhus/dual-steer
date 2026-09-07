package main

import (
	agent "example.com/dual-steer/dualsteer-agent"
	"fmt"
	"os"
)

func main() {
	if err := agent.Run(os.Args[1:], os.Stdout, agent.ResolveInterface); err != nil {
		fmt.Fprintln(os.Stderr, "dualsteer-agent:", err)
		os.Exit(1)
	}
}
