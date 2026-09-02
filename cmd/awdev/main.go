package main

import (
	"fmt"
	"os"

	"github.com/agenticworkflowdev/cli/internal/app"
)

func main() {
	command := app.NewCommand()
	command.SetOut(os.Stdout)
	command.SetErr(os.Stderr)

	if err := command.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
