package verification

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"regexp"
	"runtime"
)

var binaryHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

func ValidBinaryHash(hash string) bool { return binaryHash.MatchString(hash) }

// RunningBinarySHA256 hashes the executing inode, not the pathname that an
// upgrade may already have replaced. Commit labels alone cannot distinguish
// default "development" builds or dirty builds made from the same HEAD.
func RunningBinarySHA256() (string, error) {
	if runtime.GOOS != "linux" {
		return "", errors.New("running binary identity unavailable")
	}
	f, err := os.Open("/proc/self/exe")
	if err != nil {
		return "", errors.New("running binary identity unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 256<<20 {
		return "", errors.New("running binary identity unavailable")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, (256<<20)+1))
	if err != nil || n != info.Size() {
		return "", errors.New("running binary identity unavailable")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
