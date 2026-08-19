package main

import (
	"fmt"
	"os"

	"github.com/agenticworkflowdev/cli/internal/cli"
)

func main() {
	command := cli.NewRootCommand()
	command.SetOut(os.Stdout)
	command.SetErr(os.Stderr)

	if err := command.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
