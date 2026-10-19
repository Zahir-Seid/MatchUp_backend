package notification

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
)

func TestBackoffGrowsAndCaps(t *testing.T) {
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{0, 30 * time.Second}, // a claim always increments first, so 0 is defensive
		{1, 30 * time.Second},
		{2, time.Minute},
		{3, 2 * time.Minute},
		{4, 4 * time.Minute},
		{5, 8 * time.Minute},
		{6, 16 * time.Minute},
		{7, 30 * time.Minute}, // 32m would exceed the cap
		{50, 30 * time.Minute},
	}
	for _, tc := range cases {
		if got := Backoff(tc.attempts); got != tc.want {
			t.Errorf("Backoff(%d) = %s, want %s", tc.attempts, got, tc.want)
		}
	}
}

func TestTemplatesRenderWithoutPayload(t *testing.T) {
	// Every template must survive an empty payload: a caller that forgets a
	// deep-link field should get vaguer copy, never a panic in the worker.
	for name, tmpl := range templates {
		title, body := tmpl.render(nil)
		if title == "" {
			t.Errorf("%s: empty title", name)
		}
		if body == "" {
			t.Errorf("%s: empty body", name)
		}
		if strings.Contains(body, "%!") {
			t.Errorf("%s: malformed format verb in body: %s", name, body)
		}
		if len(tmpl.channels) == 0 {
			t.Errorf("%s: no channels", name)
		}
	}
}

func TestTemplateCoversEveryType(t *testing.T) {
	for _, typ := range []string{
		TypeBookingConfirmed, TypePaymentUnderReview, TypePaymentRejected,
		TypeHoldExpiring, TypeHoldExpired, TypeGameReminder, TypeGameCancelled,
		TypeBookingCancelled, TypePaymentAccountsUpdated, TypeGameSpotAvailable,
	} {
		if _, ok := templates[typ]; !ok {
			t.Errorf("no template for type %q", typ)
		}
	}
}

func TestTemplateUsesPayloadWhenPresent(t *testing.T) {
	starts := time.Date(2026, 9, 12, 15, 30, 0, 0, time.UTC)
	_, body := templates[TypeGameReminder].render(map[string]any{
		"field_name": "Kotebe Turf",
		"starts_at":  starts.Format(time.RFC3339),
	})
	if !strings.Contains(body, "Kotebe Turf") {
		t.Errorf("body dropped the field name: %s", body)
	}
	if !strings.Contains(body, "Sat 12 Sep at 3:30 PM") {
		t.Errorf("body dropped the start time: %s", body)
	}
}

func TestPayloadWhenIgnoresUnparseableTime(t *testing.T) {
	if got := (payload{"starts_at": "next tuesday"}).when(); got != "" {
		t.Errorf("when() = %q, want empty for an unparseable timestamp", got)
	}
	if got := (payload{}).when(); got != "" {
		t.Errorf("when() = %q, want empty when absent", got)
	}
}

func TestPushDataFlattensPayload(t *testing.T) {
	id := uuid.New()
	raw, err := json.Marshal(map[string]any{
		"booking_id": "abc",
		"week_index": 3,
		"is_monthly": true,
		"nested":     map[string]any{"a": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	data := pushData(db.Notification{ID: id, Type: TypeBookingConfirmed, Payload: raw})

	want := map[string]string{
		"type":            TypeBookingConfirmed,
		"notification_id": id.String(),
		"booking_id":      "abc",
		"week_index":      "3",
		"is_monthly":      "true",
		"nested":          `{"a":1}`,
	}
	for key, value := range want {
		if data[key] != value {
			t.Errorf("data[%q] = %q, want %q", key, data[key], value)
		}
	}
}

func TestPushDataToleratesBrokenPayload(t *testing.T) {
	data := pushData(db.Notification{Type: TypeHoldExpired, Payload: []byte("{not json")})
	if data["type"] != TypeHoldExpired {
		t.Errorf("type lost: %#v", data)
	}
}

func TestTokenLink(t *testing.T) {
	cases := []struct{ base, token, want string }{
		{"matchup://reset", "abc", "matchup://reset?token=abc"},
		{"https://app.matchup.et/reset?src=email", "abc", "https://app.matchup.et/reset?src=email&token=abc"},
		{"", "abc", ""},
		{"matchup://reset", "", ""},
	}
	for _, tc := range cases {
		if got := tokenLink(tc.base, tc.token); got != tc.want {
			t.Errorf("tokenLink(%q, %q) = %q, want %q", tc.base, tc.token, got, tc.want)
		}
	}
}

func TestExpiryPhrase(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	cases := []struct{ in, want string }{
		{now.Add(30 * time.Minute).Format(time.RFC3339), "This link expires in 30 minutes."},
		{now.Add(time.Hour).Format(time.RFC3339), "This link expires in an hour."},
		{now.Add(3 * time.Hour).Format(time.RFC3339), "This link expires in 3 hours."},
		{now.Add(-time.Minute).Format(time.RFC3339), ""},
		{"", ""},
		{"whenever", ""},
	}
	for _, tc := range cases {
		if got := expiryPhrase(tc.in, now); got != tc.want {
			t.Errorf("expiryPhrase(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestGreeting(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Abebe Bikila", "Hello Abebe,"},
		{"Abebe", "Hello Abebe,"},
		{"  ", "Hello,"},
	}
	for _, tc := range cases {
		if got := greeting(tc.in); got != tc.want {
			t.Errorf("greeting(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func brandFixture() Brand {
	return Brand{
		AppName:          "MatchUp",
		SupportEmail:     "help@matchup.et",
		PasswordResetURL: "https://app.matchup.et/reset",
	}
}

func outboxFixture(t *testing.T, template string, p map[string]any) db.EmailOutbox {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return db.EmailOutbox{
		ID:        uuid.New(),
		ToAddress: "player@example.com",
		Template:  template,
		Payload:   raw,
	}
}

func TestRenderOutboxPasswordReset(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	row := outboxFixture(t, TemplatePasswordReset, map[string]any{
		"full_name":  "Abebe Bikila",
		"token":      "reset-token-123",
		"expires_at": now.Add(30 * time.Minute).Format(time.RFC3339),
	})

	msg, err := brandFixture().renderOutbox(row, now)
	if err != nil {
		t.Fatalf("renderOutbox: %v", err)
	}
	if msg.To != row.ToAddress {
		t.Errorf("To = %q, want %q", msg.To, row.ToAddress)
	}
	if !strings.Contains(msg.Subject, "password") {
		t.Errorf("subject does not mention the password: %q", msg.Subject)
	}
	for _, want := range []string{
		"https://app.matchup.et/reset?token=reset-token-123",
		"reset-token-123",
		"Hello Abebe,",
		"expires in 30 minutes",
		"If this was not you",
	} {
		if !strings.Contains(msg.HTML, want) {
			t.Errorf("HTML missing %q", want)
		}
	}
	if !strings.Contains(msg.Text, "reset-token-123") {
		t.Error("text part missing the token")
	}
}

func TestRenderOutboxEscapesRecipientName(t *testing.T) {
	// The name is user-supplied, so it must never reach the HTML unescaped.
	row := outboxFixture(t, TemplatePasswordReset, map[string]any{
		"full_name": `<script>alert(1)</script>`,
		"token":     "t",
	})
	msg, err := brandFixture().renderOutbox(row, time.Now())
	if err != nil {
		t.Fatalf("renderOutbox: %v", err)
	}
	if strings.Contains(msg.HTML, "<script>") {
		t.Errorf("unescaped markup in HTML: %s", msg.HTML)
	}
}

func TestRenderOutboxFallsBackToCodeWithoutLink(t *testing.T) {
	// No configured deep link: the mail still has to be sendable, carrying the
	// code on its own.
	row := outboxFixture(t, TemplatePasswordReset, map[string]any{"token": "code-only"})
	msg, err := Brand{}.renderOutbox(row, time.Now())
	if err != nil {
		t.Fatalf("renderOutbox: %v", err)
	}
	if !strings.Contains(msg.HTML, "code-only") {
		t.Error("HTML missing the code")
	}
	if strings.Contains(msg.HTML, "href=\"\"") {
		t.Error("rendered an empty link")
	}
}

func TestRenderOutboxRejectsUnusableMail(t *testing.T) {
	cases := map[string]db.EmailOutbox{
		"no token":         outboxFixture(t, TemplatePasswordReset, map[string]any{"full_name": "A"}),
		"unknown template": {ID: uuid.New(), ToAddress: "a@b.c", Template: "welcome_hamper"},
	}
	for name, row := range cases {
		if _, err := brandFixture().renderOutbox(row, time.Now()); err == nil {
			t.Errorf("%s: expected an error", name)
		} else if !strings.Contains(err.Error(), "rejected") {
			t.Errorf("%s: error should be permanent, got %v", name, err)
		}
	}
}

func TestRenderNotificationEmailReusesPushCopy(t *testing.T) {
	n := db.Notification{
		ID:    uuid.New(),
		Type:  TypeBookingConfirmed,
		Title: "Booking confirmed",
		Body:  "Your payment cleared and booking MU-ABC12345 is confirmed.",
	}
	body := brandFixture().notificationEmail("Abebe Bikila", n)
	if body.Heading != n.Title {
		t.Errorf("heading = %q, want the notification title", body.Heading)
	}
	html := body.html()
	if !strings.Contains(html, n.Body) {
		t.Error("HTML dropped the notification body")
	}
	if !strings.Contains(body.text(), n.Body) {
		t.Error("text part dropped the notification body")
	}
	if !strings.Contains(html, "help@matchup.et") {
		t.Error("HTML dropped the support address")
	}
}
