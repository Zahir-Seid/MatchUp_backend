package auth

import (
	"strings"
	"testing"
)

func TestPasswordPolicyByRole(t *testing.T) {
	tests := []struct {
		name     string
		role     string
		password string
		ok       bool
		want     string
	}{
		{"player eight accepted", RolePlayer, "abcde123", true, ""},
		{"player seven rejected", RolePlayer, "abc1234", false, "at least 8"},
		{"admin nine rejected", RoleAdmin, "abcdef123", false, "at least 10"},
		{"admin ten accepted", RoleAdmin, "abcdef1234", true, ""},
		{"unknown role uses player bound", "future_role", "abcde123", true, ""},
		{"needs a non-letter", RolePlayer, "abcdefgh", false, "letters"},
		{"byte cap still applies", RoleAdmin, strings.Repeat("a", 72) + "1", false, "at most 72"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hash, err := hashPasswordForRole(tt.password, tt.role)
			if tt.ok {
				if err != nil {
					t.Fatalf("hashPasswordForRole(%q, %q): %v", tt.password, tt.role, err)
				}
				if !verifyPassword(hash, tt.password) {
					t.Error("hash does not verify the original password")
				}
				return
			}
			if err == nil {
				t.Fatalf("hashPasswordForRole(%q, %q) accepted a weak password", tt.password, tt.role)
			}
			weak, ok := AsWeakPassword(err)
			if !ok {
				t.Fatalf("error is not ErrWeakPassword: %v", err)
			}
			if !strings.Contains(weak.Reason, tt.want) {
				t.Errorf("reason = %q, want it to contain %q", weak.Reason, tt.want)
			}
		})
	}
}

func TestPasswordPolicyCountsRunesNotBytes(t *testing.T) {
	// Eight runes, more than eight bytes: the bound is a length a person can
	// count, while bcrypt's separate byte cap protects the hash.
	if _, err := hashPasswordForRole("አበሐመሠረሰ1", RolePlayer); err != nil {
		t.Errorf("eight-rune player password rejected: %v", err)
	}
}
