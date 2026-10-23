package creation

import (
	"os"

	"golang.org/x/sys/unix"
)

func renameRestoreNoReplace(from *os.Root, source string, to *os.Root, destination string) error {
	a, err := from.Open(".")
	if err != nil {
		return err
	}
	defer a.Close()
	b, err := to.Open(".")
	if err != nil {
		return err
	}
	defer b.Close()
	return unix.RenameatxNp(int(a.Fd()), source, int(b.Fd()), destination, unix.RENAME_EXCL)
}
