package config

import "errors"

var (
	// ErrMissingRequired is returned when a required configuration field
	// (typically an environment variable) is not set.
	ErrMissingRequired = errors.New("missing required configuration")

	// ErrInvalidValue is returned when a configuration value fails
	// validation (e.g., negative duration, out-of-range port).
	ErrInvalidValue = errors.New("invalid configuration value")

	// ErrConfigLoad is returned when the JSON configuration file cannot
	// be opened or decoded.
	ErrConfigLoad = errors.New("failed to load configuration file")
)
