// Command worker runs the background loops.
//
// Everything that must happen without a request behind it lives here: bank
// verification of submitted payments, the sweep that releases spots whose hold
// ran out, notification and email delivery, and the reconciliation pass that
// hands back work abandoned by a process that died holding it.
//
// It serves no HTTP. That is deliberate: a bank lookup takes tens of seconds and
// a delivery batch can stall on a provider, and neither belongs on a request
// the app is waiting on. The API's job is to record what happened and queue the
// work; this binary is what spends the time.
//
// Several copies can run at once. Every queue is claimed with a single
// claim-and-mark statement using SKIP LOCKED, so two workers take disjoint
// batches rather than doing the same job twice (SDD 9.2).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Zahir-Seid/telebirr-verifier/pkg/telebirr"

	"github.com/Zahir-Seid/MatchUp_backend/internal/booking"
	"github.com/Zahir-Seid/MatchUp_backend/internal/config"
	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/notification"
	"github.com/Zahir-Seid/MatchUp_backend/internal/payment"
	"github.com/Zahir-Seid/MatchUp_backend/internal/payment/cbeadapter"
	"github.com/Zahir-Seid/MatchUp_backend/internal/payment/telebirradapter"
	"github.com/Zahir-Seid/MatchUp_backend/internal/worker"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "worker: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// A job mid-pass keeps its claim until it returns, so shutdown waits for the
	// current pass rather than killing it: a payment abandoned mid-verification
	// would sit in 'processing' until the reconciliation pass timed it out.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(config.NeedWorker)
	if err != nil {
		return err
	}

	log := cfg.NewLogger("worker")
	slog.SetDefault(log)

	store, err := db.Open(ctx, db.PoolConfig{
		URL:             cfg.DatabaseURL,
		MaxConns:        cfg.DBMaxConns,
		MinConns:        cfg.DBMinConns,
		MaxConnLifetime: cfg.DBMaxConnLife,
		MaxConnIdleTime: cfg.DBMaxConnIdle,
	})
	if err != nil {
		return err
	}
	defer store.Close()

	// The queueing side of notifications: the verify service and the sweeper use
	// it to record an outcome for the customer inside the transaction that
	// caused it. Delivery is the dispatcher's job, further down.
	notifySvc := notification.NewService(store, log, cfg.ResendAPIKey != "")

	email := emailSender(cfg, log)

	dispatcher := notification.NewDispatcher(notification.DispatcherOptions{
		Store:  store,
		Logger: log,
		Email:  email,
		Brand: notification.Brand{
			AppName:          cfg.AppName,
			SupportEmail:     cfg.SupportEmail,
			PasswordResetURL: cfg.PasswordResetURL,
			EmailVerifyURL:   cfg.EmailVerifyURL,
		},
		BatchSize:  cfg.NotifyBatchSize,
		StaleAfter: cfg.StuckJobThreshold,
	})

	verifiers, err := buildVerifiers(cfg, log)
	if err != nil {
		return err
	}

	verifySvc := payment.NewVerifyService(payment.VerifyOptions{
		Store:      store,
		Logger:     log,
		Notifier:   notifySvc,
		Verifiers:  verifiers,
		BatchSize:  cfg.VerifyBatchSize,
		StaleAfter: cfg.StuckJobThreshold,
	})

	sweeper := booking.NewSweeper(booking.SweepOptions{
		Store:      store,
		Logger:     log,
		Notifier:   notifySvc,
		WarnBefore: cfg.ExpiryWarningWindow,
		// Every warning becomes a delivery row, so the warning pass is bounded
		// by the same batch size that drains them. A backlog then moves at the
		// rate deliveries actually leave rather than piling up ahead of them.
		WarnMaxRows: cfg.NotifyBatchSize,
	})

	jobs := worker.Standard(worker.Deps{
		Log:        log,
		Verify:     verifySvc,
		Sweeper:    sweeper,
		Dispatcher: dispatcher,
		Cadence: worker.Cadence{
			Verify:      cfg.VerifyPollInterval,
			ExpirySweep: cfg.ExpirySweepInterval,
			Notify:      cfg.NotifyPollInterval,
			Reconcile:   cfg.ReconcileInterval,
			// Every job's timeout is derived from this, so a pass cannot
			// outlive the point at which its own claims are considered
			// abandoned and handed to another worker.
			StuckThreshold: cfg.StuckJobThreshold,
		},
	})

	return worker.New(log, jobs...).Run(ctx)
}

// buildVerifiers builds the payment channels. Both are always constructed: a
// verifier that cannot reach its bank returns a transport error, which is
// retried and then parked for review, whereas a missing verifier would leave
// its payments claimed and dropped with no outcome at all.
func buildVerifiers(cfg *config.Config, log *slog.Logger) ([]payment.Verifier, error) {
	cbe := cbeadapter.New(cbeadapter.Options{
		MerchantAccount: cfg.MerchantCBEAccount,
		Timeout:         cfg.CBETimeout,
		// Config refuses this outside APP_ENV=local. Logged loudly even there,
		// because with it on a receipt is forgeable by anyone on the network
		// path and nothing else in the system would show that.
		InsecureSkipTLS: cfg.AllowInsecureVerifierTLS,
	})
	if cfg.AllowInsecureVerifierTLS {
		log.Warn("CBE TLS verification is disabled; payment receipts are forgeable on this host")
	}

	// NewFromEnv reads the library's own proxy and route settings, then applies
	// these on top -- the appended options win, so the timeout an operator sets
	// through this project's config is the one that governs and there is a
	// single place to look for it.
	client, err := telebirr.NewFromEnv(nil,
		telebirr.WithLogger(log.With(slog.String("component", "telebirr"))),
		telebirr.WithTotalTimeout(cfg.TelebirrTimeout),
	)
	if err != nil {
		return nil, fmt.Errorf("telebirr client: %w", err)
	}

	return []payment.Verifier{
		cbe,
		telebirradapter.New(client, cfg.MerchantTelebirrAccount),
	}, nil
}

// emailSender builds the Resend sender, or returns nil when email is not
// configured. A nil sender leaves email deliveries pending rather than failing
// them, so adding a key later drains the backlog instead of finding it
// discarded.
func emailSender(cfg *config.Config, log *slog.Logger) notification.EmailSender {
	if cfg.ResendAPIKey == "" {
		log.Warn("email delivery disabled: RESEND_API_KEY is not set")
		return nil
	}
	sender, err := notification.NewResendSender(cfg.ResendAPIKey, cfg.ResendFromAddress)
	if err != nil {
		log.Error("email delivery disabled: Resend sender could not be built", slog.Any("error", err))
		return nil
	}
	return sender
}
