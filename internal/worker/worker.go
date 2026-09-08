// Package worker runs the background loops: payment verification, the booking
// hold sweep, notification and email delivery, and the reconciliation pass that
// recovers work abandoned by a dead process.
//
// Every loop is a poll against a Postgres queue that claims with FOR UPDATE SKIP
// LOCKED, so running more than one worker is safe and needs no coordination
// beyond the database (SDD 9.2). There is no in-memory scheduling state: a
// worker that dies mid-job leaves the row claimed, and the reconciliation pass
// hands it back.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"runtime/debug"
	"sync"
	"time"
)

// Job is one background loop.
type Job struct {
	// Name appears in every log line the job produces.
	Name string
	// Interval is the gap between the end of one run and the start of the next.
	// Measuring from the end rather than on a fixed tick is deliberate: a run
	// that overruns its interval must not have the next one start on top of it.
	Interval time.Duration
	// Timeout bounds a single run. Zero means the interval is used, which keeps
	// a hung run from blocking its loop indefinitely.
	Timeout time.Duration
	// Run does the work and reports how many items it handled, for the log.
	Run func(ctx context.Context) (int, error)
}

// Worker runs a set of jobs until its context is cancelled.
type Worker struct {
	log  *slog.Logger
	jobs []Job
}

// New builds a worker over the given jobs.
func New(log *slog.Logger, jobs ...Job) *Worker {
	if log == nil {
		log = slog.Default()
	}
	kept := make([]Job, 0, len(jobs))
	for _, j := range jobs {
		if j.Run != nil && j.Interval > 0 {
			kept = append(kept, j)
		}
	}
	return &Worker{log: log, jobs: kept}
}

// Run starts every job and blocks until ctx is cancelled and all of them have
// finished their current pass. A job's error never stops the worker: a database
// hiccup should cost one poll, not the process.
func (w *Worker) Run(ctx context.Context) error {
	if len(w.jobs) == 0 {
		return errors.New("worker: no jobs configured")
	}

	w.log.Info("worker starting", slog.Int("jobs", len(w.jobs)))

	var wg sync.WaitGroup
	for _, job := range w.jobs {
		wg.Add(1)
		go func(job Job) {
			defer wg.Done()
			w.loop(ctx, job)
		}(job)
	}
	wg.Wait()

	w.log.Info("worker stopped")
	return nil
}

// loop runs one job on its interval until ctx is cancelled.
func (w *Worker) loop(ctx context.Context, job Job) {
	log := w.log.With(slog.String("job", job.Name))

	// A short random delay before the first run staggers the loops so five
	// queues are not polled in the same instant at every boot, and so two
	// workers restarted together do not march in lockstep.
	if !sleep(ctx, jitter(job.Interval)) {
		return
	}

	for {
		start := time.Now()
		handled, err := w.runOnce(ctx, job)
		elapsed := time.Since(start)

		switch {
		case err != nil && ctx.Err() != nil:
			// Cancelled mid-run during shutdown. Not a fault.
			log.Debug("job interrupted by shutdown", slog.Duration("elapsed", elapsed))
		case err != nil:
			log.Error("job failed",
				slog.Duration("elapsed", elapsed),
				slog.Any("error", err))
		case handled > 0:
			log.Info("job ran",
				slog.Int("handled", handled),
				slog.Duration("elapsed", elapsed))
		default:
			// An idle poll is the common case and would drown out everything
			// else at info level.
			log.Debug("job idle", slog.Duration("elapsed", elapsed))
		}

		if !sleep(ctx, job.Interval) {
			return
		}
	}
}

// runOnce executes one pass under its own timeout, converting a panic into an
// error so a bug in one job cannot take the process down and stop the others.
func (w *Worker) runOnce(ctx context.Context, job Job) (handled int, err error) {
	timeout := job.Timeout
	if timeout <= 0 {
		timeout = job.Interval
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	defer func() {
		if rec := recover(); rec != nil {
			w.log.Error("job panicked",
				slog.String("job", job.Name),
				slog.Any("panic", rec),
				slog.String("stack", string(debug.Stack())))
			err = fmt.Errorf("job %s panicked: %v", job.Name, rec)
		}
	}()

	return job.Run(runCtx)
}

// sleep waits for d or until ctx is cancelled, reporting whether the wait
// completed.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// jitter returns a random fraction of d, up to a tenth of a second short of it.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(d)))
}
