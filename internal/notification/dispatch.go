package notification

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
)

// EmailSender delivers one transactional email.
type EmailSender interface {
	Send(ctx context.Context, msg EmailMessage) error
}

// EmailMessage is a transactional email.
type EmailMessage struct {
	To      string
	Subject string
	HTML    string
	Text    string
}

// Dispatcher drains the delivery queues. It is driven by the worker; nothing on
// the request path calls it.
type Dispatcher struct {
	store *db.Store
	log   *slog.Logger
	email EmailSender
	brand Brand

	batchSize int32
	// staleAfter is how long a claim may sit in 'processing' before it is
	// assumed to belong to a dead worker.
	staleAfter time.Duration
}

// DispatcherOptions configures the dispatcher.
type DispatcherOptions struct {
	Store      *db.Store
	Logger     *slog.Logger
	Email      EmailSender
	Brand      Brand
	BatchSize  int
	StaleAfter time.Duration
}

// NewDispatcher builds the delivery dispatcher. A nil sender disables that
// channel: its deliveries are left pending rather than failed, so enabling the
// channel later drains the backlog instead of discarding it.
func NewDispatcher(opts DispatcherOptions) *Dispatcher {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	batch := int32(opts.BatchSize)
	if batch <= 0 {
		batch = 20
	}
	stale := opts.StaleAfter
	if stale <= 0 {
		stale = 5 * time.Minute
	}
	return &Dispatcher{
		store:      opts.Store,
		log:        log,
		email:      opts.Email,
		brand:      opts.Brand.withDefaults(),
		batchSize:  batch,
		staleAfter: stale,
	}
}

// errChannelDisabled marks a delivery whose channel has no configured sender.
var errChannelDisabled = errors.New("channel has no configured sender")

// DispatchNotifications claims a batch of pending deliveries and sends them,
// returning how many were claimed.
func (d *Dispatcher) DispatchNotifications(ctx context.Context) (int, error) {
	claimed, err := d.store.ClaimNotificationDeliveries(ctx, d.batchSize)
	if err != nil {
		return 0, fmt.Errorf("claim notification deliveries: %w", err)
	}
	if len(claimed) == 0 {
		return 0, nil
	}

	ids := make([]uuid.UUID, 0, len(claimed))
	for _, delivery := range claimed {
		ids = append(ids, delivery.ID)
	}
	contexts, err := d.store.ListDeliveryContexts(ctx, ids)
	if err != nil {
		return len(claimed), fmt.Errorf("list delivery contexts: %w", err)
	}
	byID := make(map[uuid.UUID]db.ListDeliveryContextsRow, len(contexts))
	for _, row := range contexts {
		byID[row.DeliveryID] = row
	}

	for _, delivery := range claimed {
		row, ok := byID[delivery.ID]
		if !ok {
			// The notification was deleted between claim and lookup, which
			// only happens when its user was deleted. Nothing to send.
			d.markSent(ctx, delivery.ID)
			continue
		}
		if err := d.deliver(ctx, row); err != nil {
			d.reschedule(ctx, delivery, err)
			continue
		}
		d.markSent(ctx, delivery.ID)
	}
	return len(claimed), nil
}

// deliver sends one delivery on its channel.
func (d *Dispatcher) deliver(ctx context.Context, row db.ListDeliveryContextsRow) error {
	switch row.Channel {
	case ChannelEmail:
		return d.deliverEmail(ctx, row)
	default:
		// The CHECK constraint makes this unreachable; failing loudly beats
		// marking an unknown channel sent.
		return fmt.Errorf("unknown delivery channel %q", row.Channel)
	}
}

func (d *Dispatcher) deliverEmail(ctx context.Context, row db.ListDeliveryContextsRow) error {
	if d.email == nil {
		return errChannelDisabled
	}
	body := d.brand.notificationEmail(row.UserFullName, row.Notification)
	return d.email.Send(ctx, EmailMessage{
		To:      row.UserEmail,
		Subject: row.Notification.Title,
		HTML:    body.html(),
		Text:    body.text(),
	})
}

func (d *Dispatcher) markSent(ctx context.Context, id uuid.UUID) {
	if err := d.store.MarkDeliverySent(ctx, id); err != nil {
		d.log.Error("could not mark delivery sent",
			slog.String("delivery_id", id.String()), slog.Any("error", err))
	}
}

// reschedule returns a failed delivery to the queue with exponential backoff.
// A channel with no sender configured is held at a flat delay: it is a
// deployment gap, not a failure the backoff curve should punish. A rejection the
// provider will repeat forever is failed outright.
func (d *Dispatcher) reschedule(ctx context.Context, delivery db.NotificationDelivery, cause error) {
	message := cause.Error()
	if errors.Is(cause, ErrEmailRejected) {
		d.log.Warn("delivery permanently rejected",
			slog.String("delivery_id", delivery.ID.String()),
			slog.String("channel", delivery.Channel),
			slog.Any("error", cause))
		if err := d.store.FailDelivery(ctx, db.FailDeliveryParams{
			ID: delivery.ID, LastError: &message,
		}); err != nil {
			d.log.Error("could not fail delivery",
				slog.String("delivery_id", delivery.ID.String()), slog.Any("error", err))
		}
		return
	}

	backoff := Backoff(int(delivery.Attempts))
	if errors.Is(cause, errChannelDisabled) {
		backoff = 15 * time.Minute
	} else {
		d.log.Warn("notification delivery failed",
			slog.String("delivery_id", delivery.ID.String()),
			slog.String("channel", delivery.Channel),
			slog.Int("attempts", int(delivery.Attempts)),
			slog.Any("error", cause))
	}
	if err := d.store.RescheduleDelivery(ctx, db.RescheduleDeliveryParams{
		ID:        delivery.ID,
		Backoff:   db.Interval(backoff),
		LastError: &message,
	}); err != nil {
		d.log.Error("could not reschedule delivery",
			slog.String("delivery_id", delivery.ID.String()), slog.Any("error", err))
	}
}

// ReclaimStuck returns deliveries and emails abandoned by a dead worker to the
// pending state.
func (d *Dispatcher) ReclaimStuck(ctx context.Context) (int64, error) {
	deliveries, err := d.store.ReclaimStuckDeliveries(ctx, db.Interval(d.staleAfter))
	if err != nil {
		return 0, fmt.Errorf("reclaim stuck deliveries: %w", err)
	}
	emails, err := d.store.ReclaimStuckEmails(ctx, db.Interval(d.staleAfter))
	if err != nil {
		return deliveries, fmt.Errorf("reclaim stuck emails: %w", err)
	}
	total := deliveries + emails
	if total > 0 {
		d.log.Warn("reclaimed stalled deliveries",
			slog.Int64("notification_deliveries", deliveries),
			slog.Int64("emails", emails))
	}
	return total, nil
}

// Backoff is the retry delay after n failed attempts: 30s, 1m, 2m, 4m, ...
// capped at 30 minutes. Shared by every queue so retry behaviour is uniform.
func Backoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	const base = 30 * time.Second
	const cap = 30 * time.Minute
	// 2^(attempts-1), clamped before the shift can overflow.
	exponent := min(attempts-1, 10)
	delay := time.Duration(math.Pow(2, float64(exponent))) * base
	if delay > cap || delay <= 0 {
		return cap
	}
	return delay
}
