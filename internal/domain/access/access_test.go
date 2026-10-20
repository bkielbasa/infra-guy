package access

import "testing"

func TestDecide(t *testing.T) {
	cases := []struct {
		name string
		fact Fact
		role Role
		ok   bool
	}{
		{name: "personal owner", fact: Fact{Personal: true, AccountID: 7, UserID: 7}, role: RoleAdmin, ok: true},
		{name: "personal mismatch", fact: Fact{Personal: true, AccountID: 7, UserID: 8}},
		{name: "org admin", fact: Fact{State: "active", OrgRole: "admin"}, role: RoleAdmin, ok: true},
		{name: "org member", fact: Fact{State: "active", OrgRole: "member"}, role: RoleMember, ok: true},
		{name: "pending", fact: Fact{State: "pending", OrgRole: "member"}},
		{name: "outside", fact: Fact{State: "", OrgRole: ""}},
		{name: "api error", fact: Fact{Failed: true, State: "active", OrgRole: "admin"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			role, ok := Decide(tc.fact)
			if role != tc.role || ok != tc.ok {
				t.Fatalf("got %q %v", role, ok)
			}
		})
	}
}
