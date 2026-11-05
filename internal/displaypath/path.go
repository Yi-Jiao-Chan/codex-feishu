// Package displaypath formats paths for display without changing their meaning.
package displaypath

import (
	"os"
	"path/filepath"
	"strings"
)

// Formatter shortens paths beneath a user's home directory for display.
// Its zero value leaves paths unchanged.
type Formatter struct {
	homes []string
}

// UserHome constructs a Formatter for the current user's home directory.
func UserHome() Formatter {
	home, err := os.UserHomeDir()
	if err != nil {
		return Formatter{}
	}
	return ForHome(home)
}

// ForHome constructs a Formatter for home and its resolved symlink alias.
func ForHome(home string) Formatter {
	if !validHome(home) {
		return Formatter{}
	}

	cleanHome := filepath.Clean(home)
	if !validHome(cleanHome) {
		return Formatter{}
	}
	homes := []string{cleanHome}

	resolved, err := filepath.EvalSymlinks(home)
	if err == nil && validHome(resolved) {
		resolved = filepath.Clean(resolved)
		if validHome(resolved) && resolved != cleanHome {
			if len(resolved) > len(cleanHome) {
				homes = []string{resolved, cleanHome}
			} else {
				homes = append(homes, resolved)
			}
		}
	}
	return Formatter{homes: homes}
}

func validHome(home string) bool {
	return home != "" && !strings.ContainsRune(home, '\x00') && filepath.IsAbs(home) && filepath.Dir(home) != home
}

// Path replaces a matching home directory prefix with the literal "$HOME".
func (f Formatter) Path(path string) string {
	for _, home := range f.homes {
		if path == home {
			return "$HOME"
		}
		if strings.HasPrefix(path, home) && len(path) > len(home) && os.IsPathSeparator(path[len(home)]) {
			return "$HOME" + path[len(home):]
		}
	}
	return path
}

// Shorten formats path using the current user's home directory.
func Shorten(path string) string {
	return UserHome().Path(path)
}
