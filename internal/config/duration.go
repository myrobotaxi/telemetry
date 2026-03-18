package config

import (
	"encoding/json"
	"fmt"
	"time"
)

// Duration wraps time.Duration with JSON string unmarshaling support.
// JSON values like "5s", "2m", "500ms" are parsed via time.ParseDuration.
type Duration struct {
	d time.Duration
}

// Dur returns the underlying time.Duration.
func (d Duration) Dur() time.Duration {
	return d.d
}

// UnmarshalJSON parses a JSON string (e.g. "5s") into a Duration.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string: %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.d = parsed
	return nil
}

// MarshalJSON serializes the Duration as a JSON string (e.g. "5s").
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.d.String())
}
