package money

import (
	"errors"
	"testing"
)

func TestParseETBText(t *testing.T) {
	valid := []struct {
		in   string
		want Santim
	}{
		{"1,500.00 Birr", 150000},
		{"1500.00", 150000},
		{"ETB 1,500.00", 150000},
		{"  250.50 birr ", 25050},
		{"0.05", 5},
		{"1,234,567.89 Birr", 123456789},
		{"14.99", 1499},
		{"75 Birr", 7500},
		{"1500.00 ብር", 150000},
	}
	for _, tc := range valid {
		got, err := ParseETBText(tc.in)
		if err != nil {
			t.Errorf("ParseETBText(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseETBText(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}

	// Anything ambiguous must fail rather than resolve to a plausible number:
	// a wrong reading here would confirm a payment that never arrived.
	invalid := []struct {
		in      string
		wantErr error
	}{
		{"", ErrEmptyAmount},
		{"Birr", ErrEmptyAmount},
		{"-", ErrEmptyAmount},
		{"n/a", ErrEmptyAmount},
		{"1500.00 / 1520.00 Birr", ErrUnparseableAmount},
		{"1.2.3", ErrUnparseableAmount},
		{"-100.00", ErrNegativeAmount},
	}
	for _, tc := range invalid {
		if got, err := ParseETBText(tc.in); err == nil {
			t.Errorf("ParseETBText(%q) = %d, want error %v", tc.in, got, tc.wantErr)
		} else if !errors.Is(err, tc.wantErr) {
			t.Errorf("ParseETBText(%q) error = %v, want %v", tc.in, err, tc.wantErr)
		}
	}
}

func TestRoundTripETB(t *testing.T) {
	// Every santim value must survive the float boundary the verifier
	// libraries impose, including amounts whose ETB form is not exactly
	// representable in binary.
	for _, s := range []Santim{0, 1, 5, 99, 1499, 150000, 123456789} {
		back, err := FromETB(s.ETB())
		if err != nil {
			t.Fatalf("FromETB(%v): %v", s.ETB(), err)
		}
		if back != s {
			t.Errorf("round trip %d -> %v -> %d", s, s.ETB(), back)
		}
	}
}

func TestFormat(t *testing.T) {
	cases := map[Santim]string{
		0:         "0.00 ETB",
		5:         "0.05 ETB",
		150000:    "1,500.00 ETB",
		123456789: "1,234,567.89 ETB",
		-150000:   "-1,500.00 ETB",
	}
	for in, want := range cases {
		if got := in.Format(); got != want {
			t.Errorf("Santim(%d).Format() = %q, want %q", in, got, want)
		}
	}
}

func TestMonthlyDiscountArithmetic(t *testing.T) {
	// PRD monthly pricing: four weeks charged, half a session discounted.
	// 4 * price * spots - 0.5 * price * spots, all in santim.
	price, spots, weeks := Santim(15000), 3, 4
	subtotal := price.Mul(spots).Mul(weeks)
	discount := price.Mul(spots).Half()
	if subtotal != 180000 {
		t.Fatalf("subtotal = %d, want 180000", subtotal)
	}
	if discount != 22500 {
		t.Fatalf("discount = %d, want 22500", discount)
	}
	if total := subtotal - discount; total != 157500 {
		t.Fatalf("total = %d, want 157500", total)
	}
	// An odd santim price must not let the discount round up past its share.
	odd := Santim(15001)
	if got := odd.Mul(1).Half(); got != 7500 {
		t.Fatalf("Half() rounded up: got %d, want 7500", got)
	}
}
