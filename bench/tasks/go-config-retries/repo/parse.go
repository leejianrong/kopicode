package config

import (
	"fmt"
	"strconv"
	"strings"
)

// Parse reads `key = value` lines over the defaults. Blank lines and lines
// starting with # are ignored. An unknown key is an error, so a typo in the
// file is reported rather than silently doing nothing.
func Parse(text string) (Config, error) {
	cfg := Default()

	for n, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return Config{}, fmt.Errorf("line %d: expected key = value, got %q", n+1, line)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		switch key {
		case "host":
			cfg.Host = value
		case "port":
			port, err := strconv.Atoi(value)
			if err != nil {
				return Config{}, fmt.Errorf("line %d: port: %w", n+1, err)
			}
			cfg.Port = port
		case "verbose":
			verbose, err := strconv.ParseBool(value)
			if err != nil {
				return Config{}, fmt.Errorf("line %d: verbose: %w", n+1, err)
			}
			cfg.Verbose = verbose
		default:
			return Config{}, fmt.Errorf("line %d: unknown key %q", n+1, key)
		}
	}

	return cfg, nil
}
