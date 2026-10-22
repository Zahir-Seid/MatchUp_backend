package httpx

import "testing"

func TestPhoneNormalizesEthiopianMobileNumbers(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"local nine", "0912345678", "+251912345678"},
		{"local seven", "0712345678", "+251712345678"},
		{"bare nine", "912345678", "+251912345678"},
		{"bare seven", "712345678", "+251712345678"},
		{"international nine", "+251912345678", "+251912345678"},
		{"international seven", "+251712345678", "+251712345678"},
		{"country code nine", "251912345678", "+251912345678"},
		{"country code seven", "251712345678", "+251712345678"},
		{"spaces", "09 12 34 56 78", "+251912345678"},
		{"hyphens", "071-234-5678", "+251712345678"},
		{"parens", "(0912) 345678", "+251912345678"},
		{"trimmed", "  0912345678  ", "+251912345678"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := NewValidator()
			got := v.Phone("phone", tt.in)
			if err := v.Err(); err != nil {
				t.Fatalf("unexpected validation error for %q: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("Phone(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestPhoneRejectsInvalidNumbers(t *testing.T) {
	tests := []string{
		"0812345678",
		"091234567",
		"09123456789",
		"+25198096034",
		"not-a-phone",
		"0912 345 67a",
	}
	for _, in := range tests {
		t.Run(in, func(t *testing.T) {
			v := NewValidator()
			v.Phone("phone", in)
			if err := v.Err(); err == nil {
				t.Fatalf("Phone(%q) was accepted", in)
			}
		})
	}
}

func TestPhoneAllowsEmpty(t *testing.T) {
	v := NewValidator()
	if got := v.Phone("phone", "   "); got != "" {
		t.Errorf("Phone(blank) = %q, want empty", got)
	}
	if err := v.Err(); err != nil {
		t.Errorf("blank phone is optional, got error: %v", err)
	}
}
