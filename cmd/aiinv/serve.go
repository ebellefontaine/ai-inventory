package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/ebellefontaine/ai-inventory/internal/config"
)

// The HTTP server's timeouts. A server with none of these is held open
// indefinitely by any client that opens a connection and then goes quiet, which
// costs nothing to do and needs no credential.
//
// ReadHeaderTimeout is the important one: it is the window between a connection
// being accepted and the request line and headers being complete, which is the
// whole of a slowloris attack. Ten seconds is far beyond any real client on a
// bad network and far below what an attacker needs.
//
// ReadTimeout and WriteTimeout cover the body and the response. Bodies here are
// capped at 64 KiB by the httpapi middleware and every response is a small JSON
// document, so thirty seconds is generous by two orders of magnitude and only
// ever fires on a connection that has stopped making progress.
//
// IdleTimeout keeps keep-alive connections from accumulating: an agent fleet
// beats on a timer and would otherwise hold one idle connection per replica
// between beats.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 120 * time.Second
)

// shutdownTimeout bounds the drain.
//
// Every request this server serves is one short database transaction — a
// registration is the longest, and it is a handful of statements — so a
// well-behaved drain finishes in milliseconds. Fifteen seconds is therefore not
// a guess at how long requests take but a bound on how long a stuck one may
// hold up the exit, chosen to sit comfortably inside the thirty seconds
// Kubernetes allows by default before SIGKILL. Exceeding it is reported as a
// failed shutdown rather than waited out, because a process that will not leave
// is something an operator needs to hear about.
const shutdownTimeout = 15 * time.Second

// minSweepInterval keeps a small configured heartbeat interval from turning the
// lifecycle worker into a busy loop against the database.
const minSweepInterval = 5 * time.Second

// sweepInterval is how often the lifecycle worker looks for agents that have
// gone quiet.
//
// The threshold it is measuring against is HeartbeatIntervalSeconds ×
// IdleAfterMissedBeats — three minutes at the defaults. The sweep interval is
// the lag between an agent crossing that line and the inventory saying so, so
// it wants to be a small fraction of it: half a heartbeat interval bounds the
// lag at thirty seconds by default, under a sixth of the threshold itself.
//
// It can afford to be that short because a sweep that finds nothing new writes
// nothing at all — the statement's final predicate excludes every agent whose
// state and replica count are already right — so the steady-state cost is one
// indexed query per interval, not one write per silent agent.
func sweepInterval(cfg config.Config) time.Duration {
	interval := time.Duration(cfg.HeartbeatIntervalSeconds) * time.Second / 2
	return max(interval, minSweepInterval)
}

// serveCommand is the thin shell around the composition root: build it, get a
// listener, run until told to stop.
func serveCommand(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	app, err := newApp(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer app.close()

	// The listener is opened here rather than by http.Server so that a port
	// already in use is a start-up failure with the address in it, rather than
	// an error returned from the goroutine that was supposed to be serving.
	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.ListenAddr, err)
	}

	return app.serve(ctx, listener)
}

// serve accepts requests on listener and runs the lifecycle worker until ctx is
// cancelled, then shuts down in the order that loses the least work: stop
// accepting, drain what is in flight, then stop the worker.
//
// The worker is stopped last because it is the only thing here that can be
// interrupted safely at any moment — its sweep is one transaction, and a
// cancelled sweep rolls back and changes nothing — whereas a request cut off
// mid-registration is an agent that believes it holds a credential the server
// never committed.
//
// The worker runs on a context of its own rather than on ctx, so that
// cancelling ctx does not kill a sweep that is in flight while requests are
// still draining.
func (a *app) serve(ctx context.Context, listener net.Listener) error {
	workerCtx, stopWorker := context.WithCancel(context.WithoutCancel(ctx))
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		a.runWorker(workerCtx)
	}()

	server := &http.Server{
		Handler:           a.handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		// net/http's own errors — a malformed request line, a TLS handshake
		// failure — otherwise go to the standard logger and out of the
		// structured stream everything else in this process writes to.
		ErrorLog: slog.NewLogLogger(a.logger.Handler(), slog.LevelWarn),
	}

	serveFailed := make(chan error, 1)
	go func() {
		a.logger.Info("serving", "addr", listener.Addr().String())
		err := server.Serve(listener)
		// Shutdown is how this server is meant to stop, so the error it
		// produces is not one.
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveFailed <- err
	}()

	select {
	case err := <-serveFailed:
		// The listener died on its own. There is nothing to drain, but the
		// worker still has to be stopped or this call would return with a
		// goroutine still sweeping.
		stopWorker()
		<-workerDone
		if err != nil {
			return fmt.Errorf("serve: %w", err)
		}
		return nil
	case <-ctx.Done():
	}

	a.logger.Info("shutdown requested: closing listener and draining in-flight requests",
		"timeout", shutdownTimeout.String())

	// A fresh context: ctx is already cancelled, and passing it would give the
	// drain no time at all.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	err := server.Shutdown(shutdownCtx)
	if err != nil {
		a.logger.Error("drain did not complete", "error", err)
	} else {
		a.logger.Info("in-flight requests drained")
	}

	stopWorker()
	select {
	case <-workerDone:
		a.logger.Info("lifecycle worker stopped")
	case <-shutdownCtx.Done():
		// The sweep is one transaction and cancelling it rolls back, so this is
		// a worker that is stuck rather than one doing something worth waiting
		// for. It is reported for the same reason a slow drain is: the process
		// is leaving without having stopped everything it started.
		a.logger.Error("lifecycle worker did not stop within the shutdown budget")
		if err == nil {
			err = errors.New("lifecycle worker did not stop in time")
		}
	}

	if err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	a.logger.Info("shutdown complete")
	return nil
}

// runWorker keeps the lifecycle sweep running until ctx is cancelled.
//
// A failed sweep restarts rather than ending the loop. LifecycleWorker.Run
// returns an error rather than swallowing it precisely so that its supervisor
// can decide, and the decision here is that a database blip must not leave a
// server that is still registering agents but has silently stopped noticing the
// ones that went quiet — an inventory that is confidently wrong.
//
// It does not take the process down either. Registration and heartbeats are the
// server's job; the sweep is what it infers in the background, and refusing a
// fleet's registrations because a background query failed would trade a real
// capability for a nicety.
func (a *app) runWorker(ctx context.Context) {
	interval := sweepInterval(a.cfg)
	a.logger.Info("lifecycle worker started", "sweep_interval", interval.String())

	for {
		err := a.worker.Run(ctx, interval)
		if err == nil {
			return // cancelled
		}
		a.logger.Error("lifecycle sweep failed", "error", err, "retry_in", interval.String())

		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
