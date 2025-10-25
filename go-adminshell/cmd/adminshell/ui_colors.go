package main

import (
	"os"
	"strings"

	"golang.org/x/term"
)

const (
	ansiReset         = "\033[0m"
	ansiBold          = "\033[1m"
	ansiFgGreen       = "\033[32m"
	ansiFgRed         = "\033[31m"
	ansiFgLightCyan   = "\033[38;5;51m"
	ansiFgLightYellow = "\033[38;5;221m"
	ansiFgLightBlue   = "\033[38;5;75m"
	ansiFgLightGreen  = "\033[38;5;78m"
	ansiFgOrange      = "\033[38;5;214m"
	ansiFgPink        = "\033[38;5;205m"
)

type colorConfig struct {
	enabled       bool
	prompt        string
	promptUser    string
	promptSymbol  string
	accent        string
	header        string
	success       string
	warn          string
	error         string
	statusOnline  string
	statusOffline string
}

var uiColors = detectColorConfig()

func detectColorConfig() colorConfig {
	cfg := colorConfig{}
	if os.Getenv("NO_COLOR") != "" {
		return cfg
	}
	termName := strings.ToLower(strings.TrimSpace(os.Getenv("TERM")))
	if termName == "" || termName == "dumb" {
		return cfg
	}
	stdoutTTY := term.IsTerminal(int(os.Stdout.Fd()))
	stderrTTY := term.IsTerminal(int(os.Stderr.Fd()))
	if !stdoutTTY && !stderrTTY {
		return cfg
	}

	cfg.enabled = true
	cfg.prompt = ansiFgLightCyan
	cfg.promptUser = ansiFgLightYellow
	cfg.promptSymbol = ansiFgLightBlue
	cfg.accent = ansiFgLightBlue
	cfg.header = ansiBold + ansiFgLightBlue
	cfg.success = ansiFgLightGreen
	cfg.warn = ansiFgOrange
	cfg.error = ansiFgPink
	cfg.statusOnline = ansiBold + ansiFgGreen
	cfg.statusOffline = ansiBold + ansiFgRed
	return cfg
}

func (c colorConfig) wrap(code, text string) string {
	if !c.enabled || code == "" || text == "" {
		return text
	}
	return code + text + ansiReset
}

func (c colorConfig) surround(code string) (string, string) {
	if !c.enabled || code == "" {
		return "", ""
	}
	return code, ansiReset
}
