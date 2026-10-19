// Package audit records who did what to which record, for the actions an admin
// takes on somebody else's money or reservation (PRD FR26, FR27).
//
// The log is append-only and deliberately outside the request path's error
// budget: an admin action that already committed must not be undone because its
// audit row failed to write. Recording therefore reports the failure loudly and
// returns it, but callers that have already changed state log it rather than
// rolling back — the alternative is a state change nobody can explain, which is
// worse than a state change with a gap in the paper trail.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
)

// Actions recorded by the admin API. The value is stored verbatim, so these are
// the vocabulary a dashboard filters on.
const (
	ActionPaymentApproved     = "payment.approved"
	ActionPaymentRejected     = "payment.rejected"
	ActionBookingCancelled    = "booking.cancelled"
	ActionGameCreated         = "game.created"
	ActionGameUpdated         = "game.updated"
	ActionGameCancelled       = "game.cancelled"
	ActionGameCapacityChanged = "game.capacity_changed"
	ActionFieldCreated        = "field.created"
	ActionFieldUpdated        = "field.updated"
	// ActionSeriesCreated records one call that generated a whole recurring
	// run of games. The individual games each get their own game.created entry
	// too, so the trail reads both ways: what the admin asked for, and what it
	// produced.
	ActionSeriesCreated     = "series.created"
	ActionUserRoleChanged   = "user.role_changed"
	ActionUserStatusChanged = "user.status_changed"
	ActionAdminLogin        = "admin.login"
	// ActionBookingSettingsUpdated records a change to the platform pricing
	// policy: a value edit with money behind it, so it belongs in the log even
	// though no booking row changed.
	ActionBookingSettingsUpdated = "settings.booking_updated"
	// ActionPaymentSettingsUpdated records a change to the receiving accounts
	// payments are verified against. It is the one settings edit with money
	// behind it, so every other admin is alerted about it as well as audited.
	ActionPaymentSettingsUpdated = "settings.payments_updated"
)

// Target types, naming the table the target_id belongs to.
const (
	TargetPayment = "payment"
	TargetBooking = "booking"
	TargetGame    = "game"
	TargetField   = "field"
	TargetUser    = "user"
	// TargetSeries is not a table. A recurring slot has no row of its own:
	// it is the set of games sharing a games.series_id, so the target id is
	// that uuid.
	TargetSeries = "game_series"
	// TargetSettings names the dashboard-editable platform settings. There is
	// no uuid to point at — a singleton row — so the target id is the setting
	// group's name, "booking" or "payments".
	TargetSettings = "settings"
)

// Recorder writes audit entries.
type Recorder struct {
	store *db.Store
	log   *slog.Logger
}

// NewRecorder builds a recorder.
func NewRecorder(store *db.Store, log *slog.Logger) *Recorder {
	if log == nil {
		log = slog.Default()
	}
	return &Recorder{store: store, log: log}
}

// Entry is one audit record.
type Entry struct {
	// ActorID is the admin who acted. Nil records a system action, such as the
	// worker parking a payment for review.
	ActorID *uuid.UUID
	Action  string
	// TargetType and TargetID identify the affected row. TargetID is text
	// rather than a UUID so a non-UUID key can be recorded without a second
	// column.
	TargetType string
	TargetID   string
	// Reason is the admin's own explanation. Mandatory for the overrides that
	// require one; the query stores nil for the rest.
	Reason string
	// Metadata carries the before/after detail that makes an entry reviewable:
	// which fields changed, what the bank said, which booking was affected.
	Metadata map[string]any
}

// Record appends one entry.
//
// It takes the caller's *db.Queries so the entry can commit with the change it
// describes; pass store.Queries to write standalone. Recording inside the
// transaction is the default because an approval whose audit row is missing
// looks, later, exactly like an approval nobody made.
func (r *Recorder) Record(ctx context.Context, q *db.Queries, e Entry) error {
	metadata, err := encodeMetadata(e.Metadata)
	if err != nil {
		return err
	}
	var reason *string
	if e.Reason != "" {
		reason = &e.Reason
	}

	if _, err := q.CreateAuditLog(ctx, db.CreateAuditLogParams{
		ActorUserID: e.ActorID,
		Action:      e.Action,
		TargetType:  e.TargetType,
		TargetID:    e.TargetID,
		Reason:      reason,
		Metadata:    metadata,
	}); err != nil {
		return fmt.Errorf("record audit entry %s: %w", e.Action, err)
	}
	return nil
}

// RecordDetached writes an entry for a change that has already committed. The
// error is logged rather than returned, because there is nothing left for the
// caller to undo, and the context is detached so a client that hung up does not
// erase the trail of what was done on its behalf.
func (r *Recorder) RecordDetached(ctx context.Context, e Entry) {
	ctx = context.WithoutCancel(ctx)
	if err := r.Record(ctx, r.store.Queries, e); err != nil {
		r.log.Error("audit entry lost",
			slog.String("action", e.Action),
			slog.String("target_type", e.TargetType),
			slog.String("target_id", e.TargetID),
			slog.Any("error", err))
	}
}

// encodeMetadata turns the metadata map into the jsonb column's bytes. A value
// that will not encode is dropped in favour of a note naming the failure: a
// readable entry missing one field beats no entry at all.
func encodeMetadata(metadata map[string]any) ([]byte, error) {
	if len(metadata) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		fallback, ferr := json.Marshal(map[string]string{
			"encode_error": err.Error(),
		})
		if ferr != nil {
			return nil, fmt.Errorf("encode audit metadata: %w", err)
		}
		return fallback, nil
	}
	return encoded, nil
}

// View is an audit entry as the dashboard renders it.
type View struct {
	ID         int64           `json:"id"`
	Action     string          `json:"action"`
	TargetType string          `json:"target_type"`
	TargetID   string          `json:"target_id"`
	Reason     *string         `json:"reason,omitempty"`
	Metadata   json.RawMessage `json:"metadata" swaggertype:"object"`
	CreatedAt  string          `json:"created_at"`
	// Actor is absent for system actions.
	Actor *Actor `json:"actor,omitempty"`
} //	@name	AuditEntry

// Actor identifies the admin behind an entry.
type Actor struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
} //	@name	AuditActor

const timeLayout = "2006-01-02T15:04:05Z07:00"

// ToView renders one listed row.
func ToView(row db.ListAuditLogsRow) View {
	metadata := json.RawMessage(row.AuditLog.Metadata)
	if len(metadata) == 0 {
		metadata = json.RawMessage("{}")
	}
	view := View{
		ID:         row.AuditLog.ID,
		Action:     row.AuditLog.Action,
		TargetType: row.AuditLog.TargetType,
		TargetID:   row.AuditLog.TargetID,
		Reason:     row.AuditLog.Reason,
		Metadata:   metadata,
		CreatedAt:  row.AuditLog.CreatedAt.Format(timeLayout),
	}
	if row.AuditLog.ActorUserID != nil {
		actor := Actor{ID: row.AuditLog.ActorUserID.String()}
		if row.ActorName != nil {
			actor.Name = *row.ActorName
		}
		if row.ActorEmail != nil {
			actor.Email = *row.ActorEmail
		}
		view.Actor = &actor
	}
	return view
}

// List returns the log newest first, optionally filtered.
func (r *Recorder) List(ctx context.Context, filter Filter, limit, offset int32) ([]View, error) {
	rows, err := r.store.ListAuditLogs(ctx, db.ListAuditLogsParams{
		Limit:       limit,
		Offset:      offset,
		ActorUserID: filter.ActorID,
		Action:      filter.Action,
		TargetType:  filter.TargetType,
	})
	if err != nil {
		return nil, fmt.Errorf("list audit logs: %w", err)
	}
	views := make([]View, 0, len(rows))
	for _, row := range rows {
		views = append(views, ToView(row))
	}
	return views, nil
}

// Filter narrows a listing. A nil field means "any".
type Filter struct {
	ActorID    *uuid.UUID
	Action     *string
	TargetType *string
}
