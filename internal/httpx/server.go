package httpx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// Serve runs srv until ctx is cancelled, then drains in-flight requests within
// shutdownTimeout.
//
// Both API binaries share this because getting it wrong is silent: a process
// that exits the moment it sees SIGTERM drops whatever requests were mid-flight,
// and during a rolling deploy that is a handful of bookings and payment
// submissions per release that appear to the client as a network error with no
// way to tell whether they landed.
func Serve(ctx context.Context, srv *http.Server, log *slog.Logger, shutdownTimeout time.Duration) error {
	if log == nil {
		log = slog.Default()
	}
	if shutdownTimeout <= 0 {
		shutdownTimeout = 20 * time.Second
	}

	// Buffered: if shutdown wins the race, the goroutine still has somewhere to
	// put ErrServerClosed and exits rather than leaking.
	errCh := make(chan error, 1)
	go func() {
		log.Info("http server listening", slog.String("addr", srv.Addr))
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case err := <-errCh:
		// The listener failed on its own, typically a port already in use.
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case <-ctx.Done():
	}

	log.Info("http server shutting down", slog.Duration("grace", shutdownTimeout))
	// The parent context is already cancelled, so the drain window needs a
	// context that outlives it.
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(drainCtx); err != nil {
		// The grace period ran out with requests still open. Close cuts them
		// off, which is what the timeout was there to bound.
		_ = srv.Close()
		return fmt.Errorf("http server shutdown: %w", err)
	}
	log.Info("http server stopped")
	return <-errCh
}

// Pinger reports whether a dependency is reachable. *db.Store satisfies it.
type Pinger interface {
	Ping(ctx context.Context) error
}

// LivenessHandler answers as long as the process is running.
//
// It deliberately checks nothing. A liveness probe decides whether to restart
// the process, and restarting an otherwise healthy API because Postgres is
// briefly unreachable turns a short database blip into a full outage with a cold
// pool behind it.
func LivenessHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		JSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
	})
}

// ReadinessHandler reports whether the process can actually serve traffic,
// which here means the database answers. This is what a load balancer should
// poll: it takes an instance out of rotation without killing it.
func ReadinessHandler(dep Pinger, timeout time.Duration) http.Handler {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		if err := dep.Ping(ctx); err != nil {
			// The cause is logged, not returned: a probe response is the
			// wrong place to describe the database.
			Fail(w, r, ErrUnavailable.WithCause(err))
			return
		}
		JSON(w, r, http.StatusOK, map[string]string{"status": "ready"})
	})
}
