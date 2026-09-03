package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/agenticworkflowdev/cli/internal/app"
	"github.com/agenticworkflowdev/cli/internal/diagnostic"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	command := app.NewCommand()
	command.SetOut(os.Stdout)
	command.SetErr(os.Stderr)

	if err := command.ExecuteContext(ctx); err != nil {
		workingDirectory, _ := os.Getwd()
		_, _ = diagnostic.ReportError(os.Stderr, workingDirectory, os.Args, err)
		os.Exit(1)
	}
}
