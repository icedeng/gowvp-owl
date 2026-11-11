package core

import "fmt"

type Error struct {
	Status  int
	Reason  string
	Message string
	Details map[string]any
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Reason, e.Message) }
func Fail(status int, reason, message string) *Error {
	return &Error{Status: status, Reason: reason, Message: message, Details: map[string]any{}}
}
func Conflict(message string) *Error { return Fail(409, "REVISION_CONFLICT", message) }
