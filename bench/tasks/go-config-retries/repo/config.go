// Package config reads the agent's `key = value` configuration file.
package config

// Config is the resolved configuration. Every field has a default in
// defaults.go and a parser case in parse.go; the three must stay in step.
type Config struct {
	// Host is the address the agent connects to.
	Host string
	// Port is the TCP port on Host.
	Port int
	// Verbose turns on per-request logging.
	Verbose bool
}
