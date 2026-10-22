package config

import "testing"

func TestValidPublicBaseURL(t *testing.T) {
	cases := map[string]bool{
		"http://localhost:8080":        true,
		"https://api.matchup.et":       true,
		"https://api.matchup.et/":      true,
		"http://192.168.1.20:8080":     true,
		"":                             false,
		"localhost:8080":               false,
		"ftp://api.matchup.et":         false,
		"https://user:pass@matchup.et": false,
		"https://api.matchup.et/v1":    false,
		"https://api.matchup.et?x=1":   false,
		"https://api.matchup.et#frag":  false,
	}

	for raw, want := range cases {
		if got := validPublicBaseURL(raw); got != want {
			t.Errorf("validPublicBaseURL(%q) = %v, want %v", raw, got, want)
		}
	}
}
