//go:build linux

package verification

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
)

func TestRunningBinaryIdentity(t *testing.T) {
	got, err := RunningBinarySHA256()
	if err != nil || !ValidBinaryHash(got) {
		t.Fatal(got, err)
	}
	bytes, err := os.ReadFile("/proc/self/exe")
	if err != nil {
		t.Fatal(err)
	}
	expected := sha256.Sum256(bytes)
	if got != hex.EncodeToString(expected[:]) {
		t.Fatal("hash did not identify running executable")
	}
	for _, bad := range []string{"", "development", got[:63], "G" + got[1:]} {
		if ValidBinaryHash(bad) {
			t.Fatal("invalid artifact hash accepted", bad)
		}
	}
}
