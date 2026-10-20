package project

import "testing"

func TestParseSourceRepo(t *testing.T) {
	cases := []struct {
		in, owner, name string
	}{
		{"bklebasa/infra", "bklebasa", "infra"},
		{"https://github.com/bklebasa/infra", "bklebasa", "infra"},
		{"https://github.com/bklebasa/infra.git", "bklebasa", "infra"},
		{"http://github.com/bklebasa/infra/", "bklebasa", "infra"},
	}
	for _, tc := range cases {
		got, err := ParseSourceRepo(tc.in)
		if err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if got.Owner != tc.owner || got.Name != tc.name {
			t.Fatalf("%s: got %#v", tc.in, got)
		}
		if got.String() != tc.owner+"/"+tc.name {
			t.Fatalf("string: %s", got.String())
		}
		if got.CloneURL() != "https://github.com/"+tc.owner+"/"+tc.name+".git" {
			t.Fatalf("clone url: %s", got.CloneURL())
		}
	}
	if _, err := ParseSourceRepo("not a repo"); err == nil {
		t.Fatal("expected invalid repo")
	}
}
