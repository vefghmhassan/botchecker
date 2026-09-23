package hetzner

import (
	"errors"
	"fmt"
)

// APIError is a refusal from Hetzner, with the code it gave.
//
// The status alone is not enough to act on: a full project and a bad token both
// answer 403, and telling them apart by reading the message text breaks the
// moment Hetzner rewords it. The code is the stable part, so it is what the
// callers match on.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("hetzner returned http %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("hetzner: %s (%s)", e.Message, e.Code)
}

// IsLimitReached reports whether the project cannot hold another server.
// This is a reason to try the next project, not a reason to give up.
func IsLimitReached(err error) bool {
	return hasCode(err, "resource_limit_exceeded")
}

// IsResourceUnavailable reports whether this location cannot serve the request
// — usually the server type is not offered there, or capacity ran out. The next
// location in the same project is worth trying.
func IsResourceUnavailable(err error) bool {
	return hasCode(err, "resource_unavailable") || hasCode(err, "invalid_input")
}

// IsAuthFailure reports a token the API would not accept.
func IsAuthFailure(err error) bool {
	return hasCode(err, "unauthorized") || hasCode(err, "forbidden")
}

func hasCode(err error, code string) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Code == code
}
