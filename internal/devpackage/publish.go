package devpackage

import (
	"bytes"
	"errors"
	"io"
	"os"
)

func openExclusive(path string, mode uint32) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(mode))
}

func publishNoClobber(output, archivePath, checksumPath, archiveCandidate, checksumCandidate string, archive, checksum []byte) (bool, error) {
	archiveExists, archiveSame, err := inspectExistingPrivate(archivePath, archive)
	if err != nil {
		return false, err
	}
	checksumExists, checksumSame, err := inspectExistingPrivate(checksumPath, checksum)
	if err != nil {
		return false, err
	}
	if checksumExists && !archiveExists {
		return false, errors.New("checksum file exists without archive")
	}
	if archiveExists && !archiveSame {
		return false, errors.New("archive already exists with different bytes")
	}
	if checksumExists && !checksumSame {
		return false, errors.New("checksum already exists with different bytes")
	}
	if archiveExists && checksumExists {
		return true, nil
	}

	if !archiveExists {
		if err := os.Link(archiveCandidate, archivePath); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return false, errors.New("cannot reserve archive output")
			}
			present, same, inspectErr := inspectExistingPrivate(archivePath, archive)
			if inspectErr != nil {
				return false, inspectErr
			}
			if !present || !same {
				return false, errors.New("archive appeared with different bytes")
			}
			archiveExists, archiveSame = true, true
		} else {
			archiveExists, archiveSame = true, true
		}
	}
	if !archiveExists || !archiveSame {
		return false, errors.New("archive reservation failed")
	}
	if !checksumExists {
		if err := os.Link(checksumCandidate, checksumPath); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return false, errors.New("cannot reserve checksum output")
			}
			present, same, inspectErr := inspectExistingPrivate(checksumPath, checksum)
			if inspectErr != nil {
				return false, inspectErr
			}
			if !present || !same {
				return false, errors.New("checksum appeared with different bytes")
			}
			checksumExists, checksumSame = true, true
		} else {
			checksumExists, checksumSame = true, true
		}
	}
	if !checksumExists || !checksumSame {
		return false, errors.New("checksum reservation failed")
	}
	return false, syncOutputDirectory(output)
}

func inspectExistingPrivate(path string, expected []byte) (present, same bool, err error) {
	_, err = os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, errors.New("cannot inspect existing output")
	}
	if err := verifyPrivateRegular(path); err != nil {
		return false, false, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, false, errors.New("cannot read existing output")
	}
	return true, bytes.Equal(data, expected), nil
}

func syncOutputDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return errors.New("cannot open output directory")
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		// Some filesystems do not support directory fsync; the names were
		// inserted atomically already, so preserve successful publication.
		if errors.Is(err, os.ErrInvalid) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		return errors.New("cannot sync output directory")
	}
	return nil
}
