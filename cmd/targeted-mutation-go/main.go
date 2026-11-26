package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/t-k/targeted-mutation-go/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	status := cli.Execute(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(status)
}
