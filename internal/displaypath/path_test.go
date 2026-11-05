package displaypath

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestFormatterPath(t *testing.T) {
	home := filepath.Join(t.TempDir(), "synthetic home", "用户")
	sep := string(filepath.Separator)
	f := ForHome(home + sep + sep)

	tests := []struct {
		name string
		path string
		want string
	}{
		{"exact", home, "$HOME"},
		{"trailing separator", home + sep, "$HOME" + sep},
		{"nested unicode and spaces", home + sep + "项目 文档" + sep + "文件", "$HOME" + sep + "项目 文档" + sep + "文件"},
		{"sibling prefix", home + "2" + sep + "file", home + "2" + sep + "file"},
		{"other user", filepath.Join(filepath.Dir(home), "other", "file"), filepath.Join(filepath.Dir(home), "other", "file")},
		{"relative", "relative" + sep + "file", "relative" + sep + "file"},
		{"literal variable", "$HOME" + sep + "file", "$HOME" + sep + "file"},
		{"braced variable", "${HOME}" + sep + "file", "${HOME}" + sep + "file"},
		{"tilde", "~" + sep + "file", "~" + sep + "file"},
		{"URL", "file://" + filepath.ToSlash(home) + "/file", "file://" + filepath.ToSlash(home) + "/file"},
		{"dot segments", home + sep + ".." + sep + "other", "$HOME" + sep + ".." + sep + "other"},
		{"repeated separators", home + sep + sep + "file", "$HOME" + sep + sep + "file"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := f.Path(tt.path); got != tt.want {
				t.Errorf("Path(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestInvalidHomeAndZeroFormatter(t *testing.T) {
	home := filepath.Join(t.TempDir(), "synthetic-user")
	sep := string(filepath.Separator)
	root := filepath.VolumeName(home) + sep
	path := home + sep + "project"
	for _, invalid := range []string{"", ".", "relative" + sep + "home", root, root + sep, home + "\x00invalid"} {
		t.Run(invalid, func(t *testing.T) {
			f := ForHome(invalid)
			if got := f.Path(path); got != path {
				t.Errorf("ForHome(%q).Path(%q) = %q, want unchanged", invalid, path, got)
			}
			if got := f.Path(invalid + sep + "project"); got != invalid+sep+"project" {
				t.Errorf("ForHome(%q) rewrote a path with an invalid home prefix: %q", invalid, got)
			}
		})
	}
	if got := (Formatter{}).Path(path); got != path {
		t.Errorf("zero Formatter.Path(%q) = %q, want unchanged", path, got)
	}
}

func TestUserHome(t *testing.T) {
	name := "HOME"
	switch runtime.GOOS {
	case "windows":
		name = "USERPROFILE"
	case "plan9":
		name = "home"
	}
	home := filepath.Join(t.TempDir(), "synthetic-user")
	path := home + string(filepath.Separator) + "project"
	t.Setenv(name, home)
	if got, want := UserHome().Path(path), "$HOME"+string(filepath.Separator)+"project"; got != want {
		t.Errorf("UserHome().Path(%q) = %q, want %q", path, got, want)
	}
	if got, want := Shorten(path), "$HOME"+string(filepath.Separator)+"project"; got != want {
		t.Errorf("Shorten(%q) = %q, want %q", path, got, want)
	}

	t.Setenv(name, "")
	if _, err := os.UserHomeDir(); err == nil {
		t.Skip("platform supplies a home directory when the environment variable is empty")
	}
	if got := UserHome().Path(path); got != path {
		t.Errorf("UserHome() after lookup failure = %q, want unchanged", got)
	}
}

func TestResolvedHomeAliasAndLongestMatch(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	physical := filepath.Join(dir, "physical home")
	if err := os.Mkdir(physical, 0o700); err != nil {
		t.Fatal(err)
	}
	logical := filepath.Join(physical, "home-link")
	if err := os.Symlink(physical, logical); err != nil {
		t.Skipf("cannot create symbolic link: %v", err)
	}
	f := ForHome(logical)
	if err := os.Remove(logical); err != nil {
		t.Fatal(err)
	}

	sep := string(filepath.Separator)
	for _, path := range []string{logical, physical} {
		if got := f.Path(path); got != "$HOME" {
			t.Errorf("Path(%q) = %q, want $HOME", path, got)
		}
		if got, want := f.Path(path+sep+"missing file"), "$HOME"+sep+"missing file"; got != want {
			t.Errorf("Path(%q) = %q, want %q", path+sep+"missing file", got, want)
		}
	}
}
