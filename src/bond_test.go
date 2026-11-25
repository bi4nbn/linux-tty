package main

import "testing"

func TestBondNameOK(t *testing.T) {
	for _, s := range []string{"bond0", "eth0", "bond0.100", "br-eth0"} {
		if !bondNameOK(s) {
			t.Errorf("bondNameOK(%q) = false", s)
		}
	}
	for _, s := range []string{"", "a b", "1234567890123456", "eth/0"} {
		if bondNameOK(s) {
			t.Errorf("bondNameOK(%q) = true", s)
		}
	}
}

func TestBondMACOK(t *testing.T) {
	for _, s := range []string{"", "00:0c:29:bc:db:1d", "02:00:00:00:80:52"} {
		if !bondMACOK(s) {
			t.Errorf("bondMACOK(%q) = false", s)
		}
	}
	for _, s := range []string{"00:11:22:33:44", "00:11:22:33:44:55:66", "zz:11:22:33:44:55"} {
		if bondMACOK(s) {
			t.Errorf("bondMACOK(%q) = true", s)
		}
	}
}

func TestBondModeOptions(t *testing.T) {
	got := bondModeOptions("802.3ad")
	for _, part := range []string{"mode=802.3ad", "miimon=100", "lacp_rate=fast", "xmit_hash_policy=layer3+4"} {
		if !contains(got, part) {
			t.Errorf("bondModeOptions(802.3ad) = %q, missing %q", got, part)
		}
	}
	if got := bondModeOptions("active-backup"); got != "mode=active-backup miimon=100" {
		t.Errorf("bondModeOptions(active-backup) = %q", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
