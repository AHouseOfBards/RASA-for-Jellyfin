// Package rasaerr defines RASA's typed errors.
//
// SPEC.md §15 requires two artifacts per failure: a plain-language message for
// the user and a full technical record for the log, and they must never be the
// same string. This package enforces that by construction rather than by code
// review — Error.Error() returns technical detail and is what the logger sees,
// while Error.User() returns a separate type carrying only safe fields. The UI
// is given a UserFacing and has no access to Detail.
package rasaerr

import (
	"errors"
	"fmt"
	"strings"
)

// Code is a stable identifier for a failure. It appears in logs and is what
// tests and support threads refer to; the wording of Message may change freely
// without breaking either.
type Code string

// ActionKind tells the UI how to render a recovery action.
type ActionKind int

const (
	// ActionRetry re-attempts the same operation.
	ActionRetry ActionKind = iota
	// ActionAlternate takes a different path — the 8443 fallback, a different
	// hostname.
	ActionAlternate
	// ActionExternal needs the user to do something outside RASA, such as
	// updating Jellyfin or editing a router page.
	ActionExternal
	// ActionCancel abandons setup.
	ActionCancel
)

// Action is a recovery the user can take. Where an action is possible it
// should be a button, not a sentence telling them to go and do something.
type Action struct {
	ID    string
	Label string
	Kind  ActionKind
}

// Error is a RASA failure.
//
// Construct these with the catalogue functions rather than by hand, so that
// every failure the product can produce has reviewed user-facing copy.
type Error struct {
	Code Code

	// Message is what happened, in the user's terms. Never contains error
	// codes, stack traces, API names, jargon, or blame.
	Message string
	// Why explains the cause when it is knowable and useful. Optional.
	Why string
	// Actions are the recoveries offered. May be empty.
	Actions []Action

	// Detail is technical context for the log. Never shown to the user.
	Detail string
	// Partial describes what was left half-configured, if anything. SPEC.md
	// §15: an error must say whether it is safe to run setup again.
	Partial string
	// Phase is the setup phase that failed.
	Phase string

	wrapped error
}

// UserFacing is the safe projection of an Error. It is what the wizard
// renders, and it structurally cannot carry technical detail.
type UserFacing struct {
	Code    Code
	Message string
	Why     string
	Partial string
	Actions []Action
}

// Error returns the technical representation, for logs only.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(string(e.Code))
	if e.Phase != "" {
		b.WriteString(" [" + e.Phase + "]")
	}
	if e.Detail != "" {
		b.WriteString(": " + e.Detail)
	}
	if e.wrapped != nil {
		b.WriteString(": " + e.wrapped.Error())
	}
	return b.String()
}

// Unwrap exposes the underlying cause to errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.wrapped }

// User returns the safe projection shown to the user.
func (e *Error) User() UserFacing {
	return UserFacing{
		Code:    e.Code,
		Message: e.Message,
		Why:     e.Why,
		Partial: e.Partial,
		Actions: e.Actions,
	}
}

// Retryable reports whether any offered action re-attempts the operation.
func (e *Error) Retryable() bool {
	for _, a := range e.Actions {
		if a.Kind == ActionRetry {
			return true
		}
	}
	return false
}

// WithPhase tags the failure with the setup phase it occurred in.
func (e *Error) WithPhase(p string) *Error { e.Phase = p; return e }

// WithPartial records what was left half-configured.
func (e *Error) WithPartial(what string) *Error { e.Partial = what; return e }

// WithDetail appends technical context for the log.
func (e *Error) WithDetail(format string, args ...any) *Error {
	d := fmt.Sprintf(format, args...)
	if e.Detail == "" {
		e.Detail = d
	} else {
		e.Detail += "; " + d
	}
	return e
}

// As extracts a *Error from an error chain.
func As(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// UserMessage returns a user-safe message for any error.
//
// This is the only sanctioned way to turn an arbitrary error into something
// shown to a user. An unrecognised error yields deliberately vague copy rather
// than leaking a Go error string into the wizard — SPEC.md §15 forbids both
// raw error text and "an unknown error occurred", so the fallback names the
// phase and points at the diagnostic bundle instead.
func UserMessage(err error) UserFacing {
	if err == nil {
		return UserFacing{}
	}
	if e, ok := As(err); ok {
		return e.User()
	}
	return UserFacing{
		Code:    CodeUnexpected,
		Message: "Setup couldn't finish this step.",
		Why:     "RASA hit a problem it didn't recognise. The details are saved in the log.",
		Actions: []Action{
			{ID: "retry", Label: "Try again", Kind: ActionRetry},
			{ID: "bundle", Label: "Save diagnostics", Kind: ActionExternal},
		},
	}
}
