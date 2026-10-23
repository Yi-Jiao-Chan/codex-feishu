package compat

import "testing"

func TestVerifiedWorktreeCodexVersionIsExact(t *testing.T) {
	for _, version := range []string{"", "0.162.0", "0.162.1-dev", "0.163.0", "26.1016.62342"} {
		if VerifiedWorktreeCodexVersion(version) {
			t.Fatalf("unverified App Server version accepted: %q", version)
		}
	}
	if !VerifiedWorktreeCodexVersion("0.162.1") {
		t.Fatal("verified App Server version was rejected")
	}
}
