// Package errors defines the engine's deliberate failures and their exit codes.
//
// Exit codes are part of the CLI's contract: CI steps and scripts branch on them, so they
// live in one place rather than being invented at each call site. Codes are never reused
// and never renumbered.
package errors

import (
	"errors"
	"fmt"
)

// Kind identifies a class of failure. Its value is the process exit code.
type Kind int

const (
	// KindGeneral is an unclassified failure. Exit 1.
	KindGeneral Kind = 1
	// KindConfig is a missing, malformed or inconsistent configuration file. Exit 2.
	KindConfig Kind = 2
	// KindAdapterNotFound is an association naming an adapter nobody registered. Exit 3.
	KindAdapterNotFound Kind = 3
	// KindAdapter is an adapter failing on a source it was expected to handle. Exit 4.
	KindAdapter Kind = 4
	// KindUnsafeURL is a URL refused before any request was made. Exit 5.
	KindUnsafeURL Kind = 5
	// KindBlocked is a source refusing automated access — robots, a login wall, a CAPTCHA
	// or a hard refusal. Reported, never worked around. Exit 6.
	KindBlocked Kind = 6
	// KindMigration is a database schema that could not be brought up to date. Exit 7.
	KindMigration Kind = 7
	// KindProvider is an optional external provider failing, such as an LLM. Callers are
	// expected to continue without it rather than surfacing this. Exit 8.
	KindProvider Kind = 8
	// KindUsage is a bad invocation. Exit 64, following sysexits convention.
	KindUsage Kind = 64
)

// Error carries a kind alongside the message, so the CLI can map it to an exit code.
type Error struct {
	Kind    Kind
	Message string
	Cause   error
}

func (e *Error) Error() string {
	if e.Cause == nil {
		return e.Message
	}
	return fmt.Sprintf("%s: %v", e.Message, e.Cause)
}

// Unwrap exposes the cause to errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.Cause }

// ExitCode is the process exit code for this failure.
func (e *Error) ExitCode() int {
	if e.Kind == 0 {
		return int(KindGeneral)
	}
	return int(e.Kind)
}

// New builds an Error with no underlying cause.
func New(kind Kind, format string, args ...any) *Error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

// Wrap builds an Error around an existing one. It returns nil when cause is nil, so it can
// be used directly in a return without a preceding check.
func Wrap(kind Kind, cause error, format string, args ...any) *Error {
	if cause == nil {
		return nil
	}
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...), Cause: cause}
}

// NewBlocked reports that a source refuses automated access.
//
// It lives here, rather than in the package that produces it, so that an adapter can
// recognise a refusal without importing the fetcher — pkg/fetch has to import pkg/directory
// to implement its Fetcher interface, so the dependency cannot go the other way.
//
// A refusal is a *result*, not a failure to retry: a caller turns it into a
// directory.Blocked entry or a model.CrawlBlocked status and moves on. See
// docs/knowledge-concepts.md §6.
func NewBlocked(format string, args ...any) *Error {
	return New(KindBlocked, format, args...)
}

// IsBlocked reports whether err is a source refusing automated access.
func IsBlocked(err error) bool {
	_, blocked := BlockedReason(err)
	return blocked
}

// BlockedReason returns the reason a source gave for refusing access, and whether err was
// such a refusal at all. The reason is meant to be recorded verbatim: "robots.txt disallows
// /members" is what makes a blocked source actionable rather than mysterious.
func BlockedReason(err error) (string, bool) {
	var typed *Error
	for errors.As(err, &typed) {
		if typed.Kind == KindBlocked {
			return typed.Error(), true
		}
		if typed.Cause == nil {
			return "", false
		}
		err = typed.Cause
	}
	return "", false
}
