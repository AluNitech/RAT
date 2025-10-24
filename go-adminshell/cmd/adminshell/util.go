package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

func parseArguments(line string) ([]string, error) {
	var (
		args     []string
		current  strings.Builder
		inSingle bool
		inDouble bool
		escape   bool
	)

	flush := func() {
		if current.Len() > 0 {
			args = append(args, current.String())
			current.Reset()
		}
	}

	for _, r := range line {
		switch {
		case escape:
			current.WriteRune(r)
			escape = false
		case r == '\\' && !inSingle:
			escape = true
		case r == '"' && !inSingle:
			inDouble = !inDouble
		case r == '\'' && !inDouble:
			inSingle = !inSingle
		case unicode.IsSpace(r) && !inSingle && !inDouble:
			flush()
		default:
			current.WriteRune(r)
		}
	}

	if escape {
		return nil, errors.New("バックスラッシュで終わっています")
	}
	if inSingle || inDouble {
		return nil, errors.New("引用符が閉じられていません")
	}
	flush()
	return args, nil
}

func expandLocalPath(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	if strings.HasPrefix(path, "~") {
		if len(path) == 1 {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			return home, nil
		}
		if len(path) >= 2 && (path[1] == '/' || path[1] == os.PathSeparator) {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			if len(path) == 2 {
				return home, nil
			}
			return filepath.Join(home, path[2:]), nil
		}
	}
	return path, nil
}
