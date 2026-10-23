package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"codex-feishu/internal/cards"
	"golang.org/x/sys/unix"
)

const maxTaskIconsBytes = 16 << 10

var errTaskIconsUnavailable = errors.New("task icons unavailable")

type taskIconManifest struct {
	Version int    `json:"version"`
	AppID   string `json:"app_id"`
	Icons   struct {
		Toolbox string `json:"toolbox"`
		Command string `json:"command"`
		Read    string `json:"read"`
		Edit    string `json:"edit"`
		Search  string `json:"search"`
		Generic string `json:"generic"`
	} `json:"icons"`
}

// loadTaskIcons reads only pre-confirmed, deployment-local image keys. It never
// creates files, uploads images, or reads the private upload journal.
func loadTaskIcons(database, appID string) (cards.TaskIcons, error) {
	var empty cards.TaskIcons
	if !filepath.IsAbs(database) || appID == "" {
		return empty, errTaskIconsUnavailable
	}
	physical, err := filepath.EvalSymlinks(database)
	if errors.Is(err, os.ErrNotExist) {
		// A fresh database may not exist yet; resolve its existing deployment
		// directory in the same way as other database-adjacent sidecars.
		parent, parentErr := filepath.EvalSymlinks(filepath.Dir(database))
		if parentErr != nil {
			return empty, errTaskIconsUnavailable
		}
		physical, err = filepath.Join(parent, filepath.Base(database)), nil
	}
	if err != nil {
		return empty, errTaskIconsUnavailable
	}
	dirFD, err := unix.Open(filepath.Dir(physical), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return empty, errTaskIconsUnavailable
	}
	defer unix.Close(dirFD)
	// Pin the deployment directory and reject symlinks without following them.
	// NONBLOCK also prevents a malicious FIFO from blocking service startup.
	fd, err := unix.Openat(dirFD, filepath.Base(physical)+".task-icons.json", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, os.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return empty, errTaskIconsUnavailable
	}
	f := os.NewFile(uintptr(fd), "task-icons")
	defer f.Close()
	var directory, file unix.Stat_t
	if unix.Fstat(dirFD, &directory) != nil || directory.Mode&07777 != 0700 || directory.Uid != uint32(os.Geteuid()) ||
		unix.Fstat(fd, &file) != nil || file.Mode&unix.S_IFMT != unix.S_IFREG || file.Mode&07777 != 0600 || file.Uid != uint32(os.Geteuid()) || file.Size > maxTaskIconsBytes {
		return empty, errTaskIconsUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(f, maxTaskIconsBytes+1))
	if err != nil || len(body) > maxTaskIconsBytes {
		return empty, errTaskIconsUnavailable
	}
	var manifest taskIconManifest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var extra any
	if decoder.Decode(&manifest) != nil || !errors.Is(decoder.Decode(&extra), io.EOF) || manifest.Version != 1 || manifest.AppID != appID {
		return empty, errTaskIconsUnavailable
	}
	icons := cards.TaskIcons{
		Toolbox: manifest.Icons.Toolbox, Command: manifest.Icons.Command,
		Read: manifest.Icons.Read, Edit: manifest.Icons.Edit,
		Search: manifest.Icons.Search, Generic: manifest.Icons.Generic,
	}
	for _, key := range []string{icons.Toolbox, icons.Command, icons.Read, icons.Edit, icons.Search, icons.Generic} {
		if !validTaskImageKey(key) {
			return empty, errTaskIconsUnavailable
		}
	}
	return icons, nil
}

func validTaskImageKey(key string) bool {
	if !strings.HasPrefix(key, "img_") || len(key) <= len("img_") || len(key) > 512 {
		return false
	}
	for _, c := range key {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func (s *Service) loadTaskIcons() {
	icons, err := loadTaskIcons(s.Config.Storage.DatabaseFile, s.Config.Feishu.AppID)
	s.taskIcons = icons
	if err != nil {
		s.notice("task_icons.unavailable")
	}
}
