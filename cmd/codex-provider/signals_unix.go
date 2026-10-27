//go:build !windows

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

func launcherSignals() (context.Context, func()) {
	// The CLI shares the foreground process group and receives terminal Ctrl-C
	// itself. Do not stop its proxy when Ctrl-C only cancels a turn in the TUI.
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGHUP)
	return ctx, func() { cancel(); signal.Stop(interrupts) }
}
