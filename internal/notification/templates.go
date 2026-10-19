package notification

import (
	"fmt"
	"strings"
	"time"
)

// timeLayout is the timestamp format in API responses.
const timeLayout = time.RFC3339

// template is the copy for one notification type plus the channels it goes out
// on. Copy lives here rather than at each call site so the same event always
// reads the same way, whichever module raised it.
//
// Only English copy exists at this phase; the app is English-only per the
// design documents. Adding Amharic later means a locale on the user row and a
// second string per template, not a change to the dispatch path.
type copyTemplate struct {
	title    string
	body     func(p payload) string
	channels []string
}

// payload is convenience access to a notification's deep-link data, tolerating
// missing keys so a template can never panic on an incomplete payload.
type payload map[string]any

func (p payload) str(key string) string {
	if p == nil {
		return ""
	}
	if v, ok := p[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// reference returns a phrase naming the booking, or a neutral fallback when the
// reference code is missing.
func (p payload) reference() string {
	if code := p.str("reference_code"); code != "" {
		return "booking " + code
	}
	return "your booking"
}

// when returns a human phrase for the game time, empty when absent or
// unparseable.
func (p payload) when() string {
	raw := p.str("starts_at")
	if raw == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return ""
	}
	return t.Format("Mon 2 Jan at 3:04 PM")
}

var templates = map[string]copyTemplate{
	TypeBookingConfirmed: {
		title: "Booking confirmed",
		body: func(p payload) string {
			// Email doubles as the receipt the customer can keep, which PRD
			// FR23 allows as a secondary channel for confirmations.
			if when := p.when(); when != "" {
				return fmt.Sprintf("Your payment cleared and %s is confirmed for %s. See you on the pitch.", p.reference(), when)
			}
			return fmt.Sprintf("Your payment cleared and %s is confirmed. See you on the pitch.", p.reference())
		},
		channels: []string{ChannelPush, ChannelEmail},
	},

	TypePaymentUnderReview: {
		title: "We are checking your payment",
		body: func(p payload) string {
			return fmt.Sprintf("We could not automatically match the payment for %s against the bank record, so someone is checking it by hand. Your spots are still held while we do. We will let you know as soon as it is settled.", p.reference())
		},
		channels: []string{ChannelPush},
	},

	TypePaymentRejected: {
		title: "Payment could not be confirmed",
		body: func(p payload) string {
			reason := p.str("reason")
			if reason == "" {
				reason = "the payment could not be matched to a bank record"
			}
			return fmt.Sprintf("We could not confirm the payment for %s: %s. The spots have been released. Please get in touch if you believe this is a mistake.", p.reference(), reason)
		},
		channels: []string{ChannelPush, ChannelEmail},
	},

	TypeHoldExpiring: {
		title: "Finish your payment",
		body: func(p payload) string {
			return fmt.Sprintf("The spots for %s are held for a few more minutes. Send the payment and submit your transaction reference to keep them.", p.reference())
		},
		channels: []string{ChannelPush},
	},

	TypeHoldExpired: {
		title: "Reservation expired",
		body: func(p payload) string {
			return fmt.Sprintf("The hold on %s ran out, so the spots went back up for anyone to take. You can book again if they are still free.", p.reference())
		},
		channels: []string{ChannelPush},
	},

	TypeGameReminder: {
		title: "Your game is coming up",
		body: func(p payload) string {
			field := p.str("field_name")
			when := p.when()
			switch {
			case field != "" && when != "":
				return fmt.Sprintf("You are playing at %s on %s. Arrive a few minutes early.", field, when)
			case when != "":
				return fmt.Sprintf("You are playing on %s. Arrive a few minutes early.", when)
			default:
				return "Your game is coming up soon. Arrive a few minutes early."
			}
		},
		channels: []string{ChannelPush},
	},

	TypeBookingCancelled: {
		title: "Booking cancelled",
		body: func(p payload) string {
			reason := p.str("reason")
			sentence := fmt.Sprintf("We have cancelled %s", p.reference())
			if reason != "" {
				sentence += ": " + reason
			}
			// Refunds are handled off-platform, so the copy must not promise
			// one automatically; it says who to talk to instead.
			if p.str("was_paid") == "true" {
				return sentence + ". The spots have been released. Please get in touch about the payment you already made."
			}
			return sentence + ". The spots have been released."
		},
		channels: []string{ChannelPush, ChannelEmail},
	},

	TypeGameCancelled: {
		title: "Game cancelled",
		body: func(p payload) string {
			reason := p.str("reason")
			when := p.when()
			sentence := "A game you booked has been cancelled"
			if when != "" {
				sentence = fmt.Sprintf("The game on %s has been cancelled", when)
			}
			if reason != "" {
				sentence += ": " + reason
			}
			return sentence + ". Someone will be in touch about your booking."
		},
		channels: []string{ChannelPush, ChannelEmail},
	},
	TypePaymentAccountsUpdated: {
		title: "Payment accounts changed",
		body: func(p payload) string {
			var accounts []string
			if cbe := p.str("cbe_account"); cbe != "" {
				accounts = append(accounts, "CBE: "+cbe)
			}
			if telebirr := p.str("telebirr_account"); telebirr != "" {
				accounts = append(accounts, "Telebirr: "+telebirr)
			}
			if len(accounts) == 0 {
				return "The receiving accounts payments are verified against were reset to the environment configuration. If this was not you or your team, check the audit log now."
			}
			return fmt.Sprintf("The receiving accounts payments are verified against changed to %s. If this was not you or your team, check the audit log and the account numbers now.", strings.Join(accounts, "; "))
		},
		channels: []string{ChannelEmail},
	},
}

// render produces the title and body for a payload.
func (t copyTemplate) render(p map[string]any) (string, string) {
	return t.title, t.body(payload(p))
}
