package compat

import "testing"

func TestVerifiedCodexVersionUsesExactAllowlist(t *testing.T) {
	for _, test := range []struct {
		version string
		want    bool
	}{
		{version: "0.162.0", want: true},
		{version: "0.162.1", want: true},
		{version: "", want: false},
		{version: "0.162.2", want: false},
		{version: "0.163.0", want: false},
		{version: "0.162", want: false},
		{version: "0.162.1+local", want: false},
	} {
		if got := VerifiedCodexVersion(test.version); got != test.want {
			t.Fatalf("VerifiedCodexVersion(%q) = %v, want %v", test.version, got, test.want)
		}
	}
}

func TestVerifiedCodexVersionsReturnsIndependentFreshSlices(t *testing.T) {
	first := VerifiedCodexVersions()
	want := []string{"0.162.0", "0.162.1"}
	if len(first) != len(want) || first[0] != want[0] || first[1] != want[1] {
		t.Fatalf("VerifiedCodexVersions() = %#v, want %#v", first, want)
	}
	first[0] = "mutated"
	second := VerifiedCodexVersions()
	if len(second) != len(want) || second[0] != want[0] || second[1] != want[1] {
		t.Fatalf("VerifiedCodexVersions() reused mutable storage: %#v", second)
	}
}
