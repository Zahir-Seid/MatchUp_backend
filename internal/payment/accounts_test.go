package payment

import (
	"testing"

	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
)

// The resolution the worker, the public instructions and the admin view all
// share: a saved dashboard value wins, an empty one hands the method back to
// the environment bootstrap.
func TestResolveAccountPrefersSettingsOverEnvironment(t *testing.T) {
	env := FallbackAccounts{CBE: "1000000000001", Telebirr: "0911000000"}

	cases := []struct {
		name       string
		row        db.PaymentSetting
		method     string
		want       string
		wantSource AccountSource
	}{
		{"an empty row uses the environment", db.PaymentSetting{}, MethodCBE, env.CBE, SourceEnv},
		{"a saved value wins", db.PaymentSetting{MerchantCbeAccount: "1000999999999"}, MethodCBE, "1000999999999", SourceDatabase},
		{"each method is resolved on its own", db.PaymentSetting{MerchantCbeAccount: "1000999999999"}, MethodTelebirr, env.Telebirr, SourceEnv},
		{"a saved Telebirr value wins", db.PaymentSetting{MerchantTelebirrAccount: "0911999999"}, MethodTelebirr, "0911999999", SourceDatabase},
		{"an unknown method resolves to nothing", db.PaymentSetting{}, "cash", "", SourceEnv},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, source := ResolveAccount(tc.row, tc.method, env)
			if got != tc.want || source != tc.wantSource {
				t.Errorf("ResolveAccount = (%q, %q), want (%q, %q)", got, source, tc.want, tc.wantSource)
			}
		})
	}
}
