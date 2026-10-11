package rules

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
)

func TestParseDirectoryCommandsCanonicalizesPaths(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "project with spaces")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(project, alias); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		text string
		want DirectoryCommand
	}{
		{"/mute-dir " + alias, DirectoryCommand{Action: DirectoryMute, Rule: DirectoryRule{Path: project}}},
		{"/mute-dir " + alias + " --recursive", DirectoryCommand{Action: DirectoryMute, Rule: DirectoryRule{Path: project, Recursive: true}}},
		{"/mute-dir --recursive " + alias, DirectoryCommand{Action: DirectoryMute, Rule: DirectoryRule{Path: project, Recursive: true}}},
		{"/unmute-dir " + alias, DirectoryCommand{Action: DirectoryUnmute, Rule: DirectoryRule{Path: project}}},
		{"/unmute-dir " + alias + " --recursive", DirectoryCommand{Action: DirectoryUnmute, Rule: DirectoryRule{Path: project, Recursive: true}}},
		{"/mute-dir " + strconv.Quote(project), DirectoryCommand{Action: DirectoryMute, Rule: DirectoryRule{Path: project}}},
		{"/mute-dir '" + project + "'", DirectoryCommand{Action: DirectoryMute, Rule: DirectoryRule{Path: project}}},
		{"/mute-dir list", DirectoryCommand{Action: DirectoryList}},
	} {
		t.Run(test.text, func(t *testing.T) {
			got, err := ParseDirectoryCommand(test.text)
			if err != nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("got %+v, want %+v, error %v", got, test.want, err)
			}
		})
	}
	if got, err := ParseDirectoryCommand("/mute-dir " + alias + "/../alias/."); err != nil || got.Rule.Path != project {
		t.Fatalf("unclean path not canonicalized: %+v %v", got, err)
	}
}

func TestDirectoryCommandsTreatPathsAsData(t *testing.T) {
	// A valid name containing shell syntax is literal filesystem data.
	path := filepath.Join(t.TempDir(), "$(touch marker);$HOME")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	got, err := ParseDirectoryCommand("/mute-dir '" + path + "'")
	if err != nil || got.Rule.Path != path {
		t.Fatalf("path expanded or rejected: %+v %v", got, err)
	}
}

func TestParseDirectoryCommandsRejectsInvalidInput(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{
		"", "/mute-dir", "/unmute-dir", "/mute " + base, "/unknown " + base,
		"/mute-dir status", "/unmute-dir list", "/mute-dir list extra", "/mute-dir list --recursive",
		"/mute-dir relative", "/unmute-dir relative", "/mute-dir " + filepath.Join(base, "missing"), "/unmute-dir " + file,
		"/mute-dir " + base + " --unknown", "/mute-dir " + base + " extra", "/mute-dir " + base + " --recursive extra",
		"/mute-dir --recursive --recursive", "/mute-dir --recursive " + base + " --recursive",
		"/mute-dir ''", "/mute-dir '" + base, "/mute-dir \"" + base, "/mute-dir " + base + "\\",
	} {
		t.Run(text, func(t *testing.T) {
			if got, err := ParseDirectoryCommand(text); err == nil {
				t.Fatalf("invalid command accepted: %+v", got)
			}
		})
	}
}
