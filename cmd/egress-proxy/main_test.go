package main

import "testing"

func TestPermits(t *testing.T) {
	deny := rule{def: false, allow: []string{"api.anthropic.com", ".ubuntu.com", "*.npmjs.org"}}
	allow := rule{def: true, deny: []string{"github.com", ".githubusercontent.com"}}

	cases := []struct {
		r    rule
		host string
		want bool
	}{
		{deny, "api.anthropic.com", true},           // exact allow
		{deny, "archive.ubuntu.com", true},          // suffix allow
		{deny, "ubuntu.com", true},                  // bare suffix root
		{deny, "registry.npmjs.org", true},          // *.suffix
		{deny, "github.com", false},                 // not in allowlist
		{deny, "evil.com", false},                   // default deny
		{allow, "example.com", true},                // default allow
		{allow, "github.com", false},                // exact deny
		{allow, "raw.githubusercontent.com", false}, // suffix deny
	}
	for _, c := range cases {
		if got := c.r.permits(c.host); got != c.want {
			t.Errorf("permits(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}
