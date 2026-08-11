package config

// Default returns the configuration used when the file is absent, and the
// starting point Parse fills in from the file.
func Default() Config {
	return Config{
		Host:    "localhost",
		Port:    8080,
		Verbose: false,
	}
}
