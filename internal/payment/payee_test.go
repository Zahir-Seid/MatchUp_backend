package payment

import "testing"

func TestComparePayee(t *testing.T) {
	const ours = "1000123456789"

	cases := []struct {
		name     string
		reported string
		ours     string
		want     PayeeVerdict
	}{
		// CBE's own masking: prefix plus four trailing digits.
		{"masked match", "1000****6789", ours, PayeeMatch},
		{"masked wrong tail", "1000****1111", ours, PayeeMismatch},
		{"masked wrong prefix", "2000****6789", ours, PayeeMismatch},
		{"masked with separators", "1000 **** 6789", ours, PayeeMatch},
		{"bullet mask", "1000••••6789", ours, PayeeMatch},

		// Too little shown to mean anything.
		{"exactly four visible digits", "***6789", "1000123456789", PayeeMatch},
		{"two visible digits", "**89", ours, PayeeInconclusive},
		{"prefix only", "1000****", ours, PayeeInconclusive},
		{"fully masked", "****", ours, PayeeInconclusive},

		// Telebirr returns the credited party unmasked.
		{"exact match", ours, ours, PayeeMatch},
		{"tail match with different padding", "0001000123456789", ours, PayeeMatch},
		{"different account", "1000999999999", ours, PayeeMismatch},

		// Absent or unconfigured.
		{"no reported account", "", ours, PayeeUnknown},
		{"only punctuation reported", "n/a", ours, PayeeUnknown},
		{"no merchant account configured", "1000****6789", "", PayeeInconclusive},

		// A masked value that cannot be a redaction of our account.
		{"longer than ours", "10001234567890123****4567", ours, PayeeMismatch},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ComparePayee(tc.reported, tc.ours); got != tc.want {
				t.Errorf("ComparePayee(%q, %q) = %q, want %q", tc.reported, tc.ours, got, tc.want)
			}
		})
	}
}

// The point of the verdict split: only an explicit match may confirm a booking.
func TestOnlyMatchIsAcceptable(t *testing.T) {
	for _, verdict := range []PayeeVerdict{PayeeMismatch, PayeeInconclusive, PayeeUnknown} {
		if verdict == PayeeMatch {
			t.Fatalf("%q must not equal PayeeMatch", verdict)
		}
	}
}

func TestNormalizeAccount(t *testing.T) {
	cases := []struct{ in, want string }{
		{"1000-1234-5678", "100012345678"},
		{"1000****6789", "1000****6789"},
		{"acct 1000 6789", "10006789"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := normalizeAccount(tc.in); got != tc.want {
			t.Errorf("normalizeAccount(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
