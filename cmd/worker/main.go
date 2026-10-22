// Command worker runs the background loops.
//
// Everything that must happen without a request behind it lives here: bank
// verification of submitted payments, the sweep that releases spots whose hold
// ran out, push and email delivery, and the reconciliation pass that hands back
// work abandoned by a process that died holding it.
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

	// Local runs read .env; production sets the environment directly.
	config.LoadDotEnv()
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

	push := pushSender(ctx, cfg, log)
	email := emailSender(cfg, log)

	dispatcher := notification.NewDispatcher(notification.DispatcherOptions{
		Store:  store,
		Logger: log,
		Push:   push,
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

	// resolveAccount reads the receiving account a method must be verified
	// against: the dashboard value once an admin has saved one, the
	// environment bootstrap otherwise. It is read per job, so an edit applies
	// to the next verification without a restart, and an override is logged so
	// a stale environment value cannot silently win.
	fallbacks := payment.FallbackAccounts{
		CBE:      cfg.MerchantCBEAccount,
		Telebirr: cfg.MerchantTelebirrAccount,
	}
	resolveAccount := func(ctx context.Context, method string) (string, error) {
		row, err := store.GetPaymentSettings(ctx)
		if err != nil {
			if !db.IsNoRows(err) {
				return "", fmt.Errorf("read payment settings: %w", err)
			}
			// An unseeded row (a restored or truncated database, most
			// likely): the environment accounts are still the full answer.
			log.Warn("payment settings row missing; using the environment accounts",
				slog.Any("error", err))
		}
		account, source := payment.ResolveAccount(row, method, fallbacks)
		if source == payment.SourceDatabase {
			if env := fallbacks.ForMethod(method); account != env {
				log.Warn("receiving account comes from the dashboard settings, not the environment",
					slog.String("method", method),
					slog.String("account", account),
					slog.String("environment_account", env))
			}
		}
		return account, nil
	}

	verifySvc := payment.NewVerifyService(payment.VerifyOptions{
		Store:      store,
		Logger:     log,
		Notifier:   notifySvc,
		Verifiers:  verifiers,
		Accounts:   resolveAccount,
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
		Timeout: cfg.CBETimeout,
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
		telebirradapter.New(client),
	}, nil
}

// pushSender builds the FCM sender, or returns nil when push is not configured.
//
// Nil disables the channel rather than failing it: the dispatcher leaves those
// deliveries pending, so credentials added later drain the backlog instead of
// finding it discarded.
func pushSender(ctx context.Context, cfg *config.Config, log *slog.Logger) notification.PushSender {
	if cfg.FCMServiceAccountJSON == "" {
		log.Warn("push delivery disabled: FCM_SERVICE_ACCOUNT_JSON is not set")
		return nil
	}
	sender, err := notification.NewFCMSender(ctx, cfg.FCMServiceAccountJSON)
	if err != nil {
		// Config validation already requires the credential outside local, so
		// reaching here means it is present but unusable. That is worth a loud
		// line, but not worth refusing to verify payments over.
		log.Error("push delivery disabled: FCM sender could not be built", slog.Any("error", err))
		return nil
	}
	return sender
}

// emailSender builds the Resend sender, or returns nil when email is not
// configured. Same reasoning as pushSender: pending beats failed.
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
