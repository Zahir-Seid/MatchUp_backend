package payment

import "github.com/Zahir-Seid/MatchUp_backend/internal/db"

// AccountSource says where an effective receiving account came from.
type AccountSource string

const (
	// SourceDatabase means an admin saved the account in the dashboard.
	SourceDatabase AccountSource = "db"
	// SourceEnv means the account still comes from the environment bootstrap.
	SourceEnv AccountSource = "env"
)

// FallbackAccounts are the environment-configured receiving accounts: the
// bootstrap values that apply wherever the dashboard has not overridden them.
// They stay required configuration even once the dashboard owns an account, so
// a fresh or restored database can never leave verification without a payee to
// check against.
type FallbackAccounts struct {
	CBE      string
	Telebirr string
}

// ForMethod returns the environment account for a payment method.
func (f FallbackAccounts) ForMethod(method string) string {
	switch method {
	case MethodCBE:
		return f.CBE
	case MethodTelebirr:
		return f.Telebirr
	default:
		return ""
	}
}

// ResolveAccount returns the receiving account a method's receipts must pay
// into, and which source it came from: the dashboard settings once an admin has
// saved a value, the environment bootstrap otherwise. It is read per use rather
// than captured at boot, so an edit applies without a restart.
func ResolveAccount(row db.PaymentSetting, method string, fallback FallbackAccounts) (string, AccountSource) {
	switch method {
	case MethodCBE:
		if row.MerchantCbeAccount != "" {
			return row.MerchantCbeAccount, SourceDatabase
		}
	case MethodTelebirr:
		if row.MerchantTelebirrAccount != "" {
			return row.MerchantTelebirrAccount, SourceDatabase
		}
	}
	return fallback.ForMethod(method), SourceEnv
}
