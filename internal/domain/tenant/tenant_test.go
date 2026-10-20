package tenant

import "testing"

func TestParseAccountType(t *testing.T) {
	got, err := ParseAccountType("User")
	if err != nil || got != "user" {
		t.Fatalf("user: %q %v", got, err)
	}
	got, err = ParseAccountType("Organization")
	if err != nil || got != "organization" {
		t.Fatalf("org: %q %v", got, err)
	}
	if _, err := ParseAccountType("Bot"); err == nil {
		t.Fatal("accepted Bot")
	}
}
