package payment

import (
	"context"
	"log/slog"

	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
)

// PaymentInstructions is what the app shows a payer: where to send money for
// each payment method, read from the same values the worker checks receipts
// against, so the app can never display an account the backend would not
// verify.
type PaymentInstructions struct {
	Methods []PaymentMethod `json:"methods"`
} //	@name	PaymentInstructions

// PaymentMethod is one way to pay, with the account to send the money to.
type PaymentMethod struct {
	Method       string   `json:"method"`
	Label        string   `json:"label"`
	Account      string   `json:"account"`
	AccountName  string   `json:"account_name"`
	Instructions []string `json:"instructions"`
} //	@name	PaymentMethod

// instructionCopy is the static display copy per method. The accounts change
// from the dashboard; the wording does not, so it lives here rather than in
// the settings row.
var instructionCopy = []struct {
	method       string
	label        string
	instructions []string
}{
	{
		method: MethodCBE,
		label:  "CBE",
		instructions: []string{
			"Open your CBE mobile app or visit a branch.",
			"Transfer the exact amount to the account above.",
			"Submit the transaction reference here once it succeeds.",
		},
	},
	{
		method: MethodTelebirr,
		label:  "Telebirr",
		instructions: []string{
			"Open Telebirr.",
			"Send the exact amount to the number above.",
			"Submit the transaction reference here once it succeeds.",
		},
	},
}

// Instructions returns the receiving accounts the app shows a payer.
//
// This never fails on the settings read: an unreadable row falls back to the
// environment bootstrap, which is what the worker itself would be verifying
// against while the row is unreadable. A method with no account at all is left
// out rather than shown blank — config refuses that state outside local, so
// reaching it is a misconfiguration worth a log line.
func (s *Service) Instructions(ctx context.Context) PaymentInstructions {
	row, err := s.store.GetPaymentSettings(ctx)
	if err != nil {
		level := slog.LevelWarn
		if !db.IsNoRows(err) {
			level = slog.LevelError
		}
		s.log.Log(ctx, level, "payment settings unavailable; using the environment accounts",
			slog.Any("error", err))
	}

	methods := make([]PaymentMethod, 0, len(instructionCopy))
	for _, copy := range instructionCopy {
		account, _ := ResolveAccount(row, copy.method, s.fallbacks)
		if account == "" {
			s.log.Warn("payment method has no receiving account; leaving it out of the instructions",
				slog.String("method", copy.method))
			continue
		}
		methods = append(methods, PaymentMethod{
			Method:       copy.method,
			Label:        copy.label,
			Account:      account,
			AccountName:  s.merchantName,
			Instructions: copy.instructions,
		})
	}
	return PaymentInstructions{Methods: methods}
}
