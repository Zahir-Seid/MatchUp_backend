package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/Zahir-Seid/MatchUp_backend/internal/booking"
	"github.com/Zahir-Seid/MatchUp_backend/internal/notification"
	"github.com/Zahir-Seid/MatchUp_backend/internal/payment"
)

// Cadence is how often each loop runs. The defaults come from the PRD's
// operational targets: verification within a few seconds of submission (FR18),
// holds released within the sweep window (FR13).
type Cadence struct {
	Verify      time.Duration
	ExpirySweep time.Duration
	Notify      time.Duration
	Reconcile   time.Duration
	// StuckThreshold is how long a claim may sit before the reconciliation pass
	// assumes the process holding it died. It is the ceiling every other job's
	// timeout is derived from, because a run that outlives it would have its own
	// work reclaimed underneath it and done twice.
	StuckThreshold time.Duration
}

// Deps are the services the background loops drive. A nil service drops its
// jobs rather than failing: a worker started without notification credentials
// should still verify payments and release expired holds.
type Deps struct {
	Log        *slog.Logger
	Verify     *payment.VerifyService
	Sweeper    *booking.Sweeper
	Dispatcher *notification.Dispatcher
	Cadence    Cadence
}

// Standard assembles the production job set.
func Standard(d Deps) []Job {
	c := d.Cadence
	if c.StuckThreshold <= 0 {
		c.StuckThreshold = 5 * time.Minute
	}

	// A run must finish, or be cancelled, before its claims could be considered
	// stale. Four fifths of the threshold leaves room for the final write to
	// land after the last bank call returns.
	claimBudget := c.StuckThreshold * 4 / 5

	var jobs []Job

	if d.Verify != nil {
		jobs = append(jobs, Job{
			Name:     "payment-verification",
			Interval: nonZero(c.Verify, 2*time.Second),
			Timeout:  claimBudget,
			Run:      d.Verify.RunOnce,
		})
	}

	if d.Sweeper != nil {
		sweep := nonZero(c.ExpirySweep, 15*time.Second)
		jobs = append(jobs, Job{
			Name:     "booking-hold-sweep",
			Interval: sweep,
			// Two short transactions. The floor keeps a fast cadence from
			// cutting the second one off.
			Timeout: maxDuration(30*time.Second, 2*sweep),
			Run: func(ctx context.Context) (int, error) {
				warned, expired, err := d.Sweeper.RunOnce(ctx)
				return warned + expired, err
			},
		})
	}

	if d.Dispatcher != nil {
		notify := nonZero(c.Notify, 5*time.Second)
		// Feed deliveries and the email outbox are separate loops so a slow
		// provider on one does not hold up the other.
		jobs = append(jobs,
			Job{
				Name:     "notification-delivery",
				Interval: notify,
				Timeout:  claimBudget,
				Run:      d.Dispatcher.DispatchNotifications,
			},
			Job{
				Name:     "email-outbox",
				Interval: notify,
				Timeout:  claimBudget,
				Run:      d.Dispatcher.DispatchEmails,
			},
		)
	}

	if d.Verify != nil || d.Dispatcher != nil {
		jobs = append(jobs, Job{
			Name:     "reconcile-stuck-work",
			Interval: nonZero(c.Reconcile, 5*time.Minute),
			Timeout:  time.Minute,
			Run:      reconcile(d),
		})
	}

	return jobs
}

// reconcile hands back every claim abandoned by a process that died holding it.
//
// This is what keeps a crash from stranding work: a claimed row is invisible to
// every other worker, so without this pass a payment claimed one second before
// a deploy would sit in 'processing' forever. Both queues are swept in one job
// because they fail for the same reason at the same moment.
func reconcile(d Deps) func(context.Context) (int, error) {
	return func(ctx context.Context) (int, error) {
		var total int
		var firstErr error

		if d.Verify != nil {
			jobs, err := d.Verify.ReclaimStuck(ctx)
			total += jobs
			if err != nil {
				firstErr = err
			}
		}
		if d.Dispatcher != nil {
			deliveries, err := d.Dispatcher.ReclaimStuck(ctx)
			total += int(deliveries)
			// The verification queue's error must not hide this one, and vice
			// versa: whichever failed, the other still needs reporting.
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return total, firstErr
	}
}

func nonZero(d, fallback time.Duration) time.Duration {
	if d <= 0 {
		return fallback
	}
	return d
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
