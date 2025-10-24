package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

const disableFocusSeq = "\x1b[?1004l"

func readSecret(prompt string) (string, error) {
	if prompt != "" {
		fmt.Print(prompt)
	}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		secret, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return "", io.EOF
			}
			return "", err
		}
		return strings.TrimSpace(string(secret)), nil
	}
	return readLine("")
}

func readLine(prompt string) (string, error) {
	if prompt != "" {
		fmt.Print(prompt)
	}
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) {
			if len(line) == 0 {
				return "", io.EOF
			}
			return sanitizeLine(line), nil
		}
		return "", err
	}
	return sanitizeLine(line), nil
}

func loadTrimmed(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func sanitizeLine(line string) string {
	trimmed := strings.TrimSpace(stripFocusReports(line))
	return trimmed
}

func stripFocusReports(s string) string {
	if !strings.ContainsRune(s, '') {
		return s
	}
	bytes := []byte(s)
	result := make([]byte, 0, len(bytes))

	for i := 0; i < len(bytes); i++ {
		b := bytes[i]
		if b != 0x1b {
			result = append(result, b)
			continue
		}
		if i+1 >= len(bytes) {
			continue
		}
		next := bytes[i+1]
		if next != '[' {
			// not a CSI sequence; keep ESC and continue
			result = append(result, b)
			continue
		}

		j := i + 2
		for j < len(bytes) {
			c := bytes[j]
			if (c >= '0' && c <= '9') || c == ';' || c == '?' {
				j++
				continue
			}
			final := c
			j++
			if final == 'I' || final == 'O' {
				i = j - 1
				goto nextLoop
			}
			break
		}
		// not focus report; keep ESC and continue normally
		result = append(result, b)
		continue

	nextLoop:
		continue
	}

	return string(result)
}

func disableTerminalFocusReporting() {
	if tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0); err == nil {
		_, _ = tty.Write([]byte(disableFocusSeq))
		_ = tty.Close()
		return
	}
	fmt.Print(disableFocusSeq)
}
