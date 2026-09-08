// Package notification owns the in-app feed and the outbound dispatch that
// mirrors it to push and email.
//
// A notification row is created in the same transaction as the state change
// that caused it, so a confirmed booking always has its record. Delivery is a
// separate queue: a dead FCM token or a Resend outage must never roll back a
// payment (SDD 8.5).
package notification

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
)

// Notification types, matching the CHECK constraint on notifications.type.
const (
	TypeBookingConfirmed   = "booking_confirmed"
	TypePaymentUnderReview = "payment_under_review"
	TypePaymentRejected    = "payment_rejected"
	TypeHoldExpiring       = "hold_expiring"
	TypeHoldExpired        = "hold_expired"
	TypeGameReminder       = "game_reminder"
	TypeGameCancelled      = "game_cancelled"
	// TypeBookingCancelled is an admin cancelling a booking, which is a
	// different event from a hold lapsing or a payment being rejected: a person
	// decided it, and the customer may be owed a refund.
	TypeBookingCancelled = "booking_cancelled"
)

// Delivery channels.
const (
	ChannelPush  = "push"
	ChannelEmail = "email"
)

// Outbox templates, matching the CHECK constraint on email_outbox.template.
// These are the mails the identity flow sends directly, outside the feed.
const (
	TemplatePasswordReset     = "password_reset"
	TemplateEmailVerification = "email_verification"
)

// Service owns the feed and the enqueue path.
type Service struct {
	store *db.Store
	log   *slog.Logger
	// emailEnabled gates the email channel. Without a Resend key configured,
	// queueing email deliveries would only manufacture rows that can never be
	// sent.
	emailEnabled bool
}

// NewService builds the notification service.
func NewService(store *db.Store, log *slog.Logger, emailEnabled bool) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{store: store, log: log, emailEnabled: emailEnabled}
}

// Request describes one notification to record and dispatch. Title and body are
// filled in from the template for Type when left empty.
type Request struct {
	UserID uuid.UUID
	Type   string
	Title  string
	Body   string
	// Payload carries the deep-link data the app needs to open the right
	// screen: booking_id, game_id, reference_code.
	Payload map[string]any
	// Channels overrides the template's default channel set.
	Channels []string
}

// Enqueue records a notification and queues its deliveries.
//
// It takes the caller's *db.Queries so it can run inside an existing
// transaction: the notification and the state change that justifies it commit
// together or not at all. Pass store.Queries to run standalone.
func (s *Service) Enqueue(ctx context.Context, q *db.Queries, req Request) (*db.Notification, error) {
	tmpl, ok := templates[req.Type]
	if !ok {
		return nil, fmt.Errorf("notification: unknown type %q", req.Type)
	}

	title, body := req.Title, req.Body
	if title == "" || body == "" {
		rendered, renderedBody := tmpl.render(req.Payload)
		if title == "" {
			title = rendered
		}
		if body == "" {
			body = renderedBody
		}
	}

	payload := []byte("{}")
	if len(req.Payload) > 0 {
		encoded, err := json.Marshal(req.Payload)
		if err != nil {
			return nil, fmt.Errorf("notification: encode payload: %w", err)
		}
		payload = encoded
	}

	notif, err := q.CreateNotification(ctx, db.CreateNotificationParams{
		UserID:  req.UserID,
		Type:    req.Type,
		Title:   title,
		Body:    body,
		Payload: payload,
	})
	if err != nil {
		return nil, fmt.Errorf("create notification: %w", err)
	}

	channels := req.Channels
	if channels == nil {
		channels = tmpl.channels
	}
	channels = s.allowedChannels(channels)
	if len(channels) > 0 {
		if err := q.CreateNotificationDeliveries(ctx, db.CreateNotificationDeliveriesParams{
			NotificationID: notif.ID,
			Channels:       channels,
		}); err != nil {
			return nil, fmt.Errorf("queue notification deliveries: %w", err)
		}
	}
	return &notif, nil
}

// allowedChannels drops channels this deployment cannot actually send on.
func (s *Service) allowedChannels(channels []string) []string {
	kept := make([]string, 0, len(channels))
	for _, c := range channels {
		if c == ChannelEmail && !s.emailEnabled {
			continue
		}
		kept = append(kept, c)
	}
	return kept
}

// View is a feed entry as the app renders it (PRD FR22).
type View struct {
	ID        uuid.UUID       `json:"id"`
	Type      string          `json:"type"`
	Title     string          `json:"title"`
	Body      string          `json:"body"`
	Payload   json.RawMessage `json:"payload"`
	Read      bool            `json:"read"`
	ReadAt    *string         `json:"read_at,omitempty"`
	CreatedAt string          `json:"created_at"`
}

func toView(n db.Notification) View {
	payload := json.RawMessage(n.Payload)
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	view := View{
		ID:        n.ID,
		Type:      n.Type,
		Title:     n.Title,
		Body:      n.Body,
		Payload:   payload,
		Read:      n.ReadAt != nil,
		CreatedAt: n.CreatedAt.Format(timeLayout),
	}
	if n.ReadAt != nil {
		formatted := n.ReadAt.Format(timeLayout)
		view.ReadAt = &formatted
	}
	return view
}

// List returns the caller's feed, newest first.
func (s *Service) List(ctx context.Context, userID uuid.UUID, unreadOnly bool, limit, offset int32) ([]View, int64, error) {
	rows, err := s.store.ListNotificationsForUser(ctx, db.ListNotificationsForUserParams{
		UserID:     userID,
		UnreadOnly: unreadOnly,
		Limit:      limit,
		Offset:     offset,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("list notifications: %w", err)
	}
	unread, err := s.store.CountUnreadNotifications(ctx, userID)
	if err != nil {
		return nil, 0, fmt.Errorf("count unread notifications: %w", err)
	}
	views := make([]View, 0, len(rows))
	for _, row := range rows {
		views = append(views, toView(row))
	}
	return views, unread, nil
}

// MarkRead marks one of the caller's notifications read. Marking an
// already-read notification is not an error; the state is simply unchanged.
func (s *Service) MarkRead(ctx context.Context, userID, notificationID uuid.UUID) error {
	_, err := s.store.MarkNotificationRead(ctx, db.MarkNotificationReadParams{
		ID:     notificationID,
		UserID: userID,
	})
	if err != nil {
		if db.IsNoRows(err) {
			// Either it does not exist, is not theirs, or was already read.
			// None of those warrant a distinct response.
			return nil
		}
		return fmt.Errorf("mark notification read: %w", err)
	}
	return nil
}

// MarkAllRead clears the caller's unread count, returning how many changed.
func (s *Service) MarkAllRead(ctx context.Context, userID uuid.UUID) (int64, error) {
	affected, err := s.store.MarkAllNotificationsRead(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("mark all notifications read: %w", err)
	}
	return affected, nil
}

// RegisterDevice records or refreshes an FCM token for the caller.
//
// The token is unique system-wide: a device that changes hands moves to the new
// account rather than delivering one user's notifications to another.
func (s *Service) RegisterDevice(ctx context.Context, userID uuid.UUID, token, platform string) error {
	if _, err := s.store.UpsertDeviceToken(ctx, db.UpsertDeviceTokenParams{
		UserID:   userID,
		FcmToken: token,
		Platform: platform,
	}); err != nil {
		return fmt.Errorf("upsert device token: %w", err)
	}
	return nil
}

// UnregisterDevice drops a token, for sign-out on that device.
func (s *Service) UnregisterDevice(ctx context.Context, token string) error {
	if _, err := s.store.DeleteDeviceToken(ctx, token); err != nil {
		return fmt.Errorf("delete device token: %w", err)
	}
	return nil
}
