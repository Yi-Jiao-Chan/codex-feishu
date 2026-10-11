package rules

import (
	"errors"
	"strings"
	"unicode"
)

type DirectoryAction string

const (
	DirectoryMute   DirectoryAction = "mute"
	DirectoryUnmute DirectoryAction = "unmute"
	DirectoryList   DirectoryAction = "list"
)

type DirectoryCommand struct {
	Action DirectoryAction
	Rule   DirectoryRule
}

// ParseDirectoryCommand accepts /mute-dir PATH [--recursive], /unmute-dir
// PATH [--recursive], and /mute-dir list. The flag may also precede PATH.
// Quotes and backslash escapes permit paths with spaces; nothing is expanded
// or executed. The result contains a canonical, existing physical directory.
func ParseDirectoryCommand(text string) (DirectoryCommand, error) {
	words, err := directoryCommandWords(text)
	if err != nil {
		return DirectoryCommand{}, err
	}
	if len(words) < 2 || len(words) > 3 {
		return DirectoryCommand{}, errors.New("expected directory command with one path and optional --recursive")
	}
	var c DirectoryCommand
	switch words[0] {
	case "/mute-dir":
		c.Action = DirectoryMute
		if len(words) == 2 && words[1] == "list" {
			c.Action = DirectoryList
			return c, nil
		}
	case "/unmute-dir":
		c.Action = DirectoryUnmute
	default:
		return DirectoryCommand{}, errors.New("unknown directory command")
	}
	path := words[1]
	if len(words) == 3 {
		if words[1] == "--recursive" {
			path = words[2]
		} else if words[2] != "--recursive" {
			return DirectoryCommand{}, errors.New("unknown directory command arguments")
		}
		c.Rule.Recursive = true
	}
	c.Rule.Path, err = CanonicalDirectory(path)
	if err != nil {
		return DirectoryCommand{}, err
	}
	return c, nil
}

func directoryCommandWords(text string) ([]string, error) {
	var words []string
	var word strings.Builder
	var quote rune
	started, escaped := false, false
	for _, char := range text {
		if escaped {
			word.WriteRune(char)
			escaped = false
			continue
		}
		if char == '\\' && quote != '\'' {
			escaped, started = true, true
			continue
		}
		if quote != 0 {
			if char == quote {
				quote = 0
			} else {
				word.WriteRune(char)
			}
			continue
		}
		if char == '\'' || char == '"' {
			quote, started = char, true
		} else if unicode.IsSpace(char) {
			if started {
				words = append(words, word.String())
				word.Reset()
				started = false
			}
		} else {
			word.WriteRune(char)
			started = true
		}
	}
	if quote != 0 || escaped {
		return nil, errors.New("unfinished directory command quote or escape")
	}
	if started {
		words = append(words, word.String())
	}
	return words, nil
}
