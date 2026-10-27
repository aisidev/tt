package main

import (
	"github.com/aisidev/tt/internal/agent"
	"os"
)

func main() { os.Exit(agent.Run(os.Args[1:], os.Stdout, os.Stderr)) }
