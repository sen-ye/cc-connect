//go:build windows

package main

import (
	"context"
	"os"
	"os/signal"
)

func launcherSignals() (context.Context, func()) {
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	return context.Background(), func() { signal.Stop(interrupts) }
}
