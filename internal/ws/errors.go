package ws

import "errors"

var (
	// ErrInvalidToken is returned when a token is empty or fails validation.
	ErrInvalidToken = errors.New("invalid token")

	// ErrAuthTimeout is returned when a client does not send an auth
	// message within the allowed window.
	ErrAuthTimeout = errors.New("auth timeout")

	// ErrHubClosed is returned when an operation is attempted on a
	// stopped hub.
	ErrHubClosed = errors.New("hub closed")
)
