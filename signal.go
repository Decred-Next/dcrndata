// Copyright (c) 2018, The Decred-Next developers
// Copyright (c) 2013-2014, The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"os"
	"os/signal"
)

// shutdownRequested checks if the Done channel of the given context has been
// closed. This could indicate cancellation, expiration, or deadline expiry. But
// when called for the context provided by withShutdownCancel, it indicates if
// shutdown has been requested (i.e. via requestShutdown).
func shutdownRequested(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

// shutdownRequest is used to initiate shutdown from one of the
// subsystems using the same code paths as when an interrupt signal is received.
var shutdownRequest = make(chan struct{})

// shutdownSignal is closed whenever shutdown is invoked through an interrupt
// signal or from an JSON-RPC stop request.  Any contexts created using
// withShutdownChannel are cancelled when this is closed.
var shutdownSignal = make(chan struct{})

// signals defines the signals that are handled to do a clean shutdown.
// Conditional compilation is used to also include SIGTERM on Unix.
var signals = []os.Signal{os.Interrupt}

// withShutdownCancel creates a copy of a context that is cancelled whenever
// shutdown is invoked through an interrupt signal or from an JSON-RPC stop
// request.
func withShutdownCancel(ctx context.Context) context.Context {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		<-shutdownSignal
		cancel()
	}()
	return ctx
}

// requestShutdown signals for starting the clean shutdown of the process
// through an internal component (such as through the JSON-RPC stop request).
func requestShutdown() {
	shutdownRequest <- struct{}{}
}

// shutdownListener listens for shutdown requests and cancels all contexts
// created from withShutdownCancel.  This function never returns and is intended
// to be spawned in a new goroutine.
func shutdownListener() {
	interruptChannel := make(chan os.Signal, 1)
	signal.Notify(interruptChannel, signals...)

	// Listen for the initial shutdown signal
	select {
	case sig := <-interruptChannel:
		log.Infof("Received signal (%s). Shutting down...", sig)
	case <-shutdownRequest:
		log.Info("Shutdown requested. Shutting down...")
	}

	// Cancel all contexts created from withShutdownCancel.
	close(shutdownSignal)

	// Listen for any more shutdown signals and log that shutdown has already
	// been signaled.
	for {
		select {
		case <-interruptChannel:
		case <-shutdownRequest:
		}
		log.Info("Shutdown signaled. Already shutting down...")
	}
}
