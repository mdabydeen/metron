package bench

// Config is the service configuration.
type Config struct {
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Verbose bool   `json:"verbose"`
}

// Default returns the built-in configuration.
func Default() Config {
	return Config{
		Host:    "localhost",
		Port:    8080,
		Verbose: false,
	}
}
