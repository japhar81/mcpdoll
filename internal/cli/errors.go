// Copyright 2026 The MCPDoll Authors.

package cli

import (
	"errors"
	"fmt"
)

// Typed errors so [Execute] can pick an exit code without string-matching, and
// so a command author picks the code by choosing a wrapper rather than by
// remembering a number.

type usageErr struct{ err error }

func (e *usageErr) Error() string { return e.err.Error() }
func (e *usageErr) Unwrap() error { return e.err }

// usageError marks a malformed invocation.
func usageError(err error) error { return &usageErr{err: err} }

type configErr struct{ err error }

func (e *configErr) Error() string { return e.err.Error() }
func (e *configErr) Unwrap() error { return e.err }

// configError marks a bad configuration or registry document: retrying will not
// help, something has to be edited.
func configError(err error) error { return &configErr{err: err} }

type notFoundErr struct{ err error }

func (e *notFoundErr) Error() string { return e.err.Error() }
func (e *notFoundErr) Unwrap() error { return e.err }

// notFoundError marks a resource that does not exist.
func notFoundError(err error) error { return &notFoundErr{err: err} }

type unavailableErr struct{ err error }

func (e *unavailableErr) Error() string { return e.err.Error() }
func (e *unavailableErr) Unwrap() error { return e.err }

// unavailableError marks a target that could not be reached; a retry may help.
func unavailableError(err error) error { return &unavailableErr{err: err} }

type validationErr struct{ err error }

func (e *validationErr) Error() string { return e.err.Error() }
func (e *validationErr) Unwrap() error { return e.err }

// validationError marks input that parsed but broke a rule.
func validationError(err error) error { return &validationErr{err: err} }

// codeFor maps an error to its documented exit code.
func codeFor(err error) int {
	var (
		usage       *usageErr
		config      *configErr
		notFound    *notFoundErr
		unavailable *unavailableErr
		validation  *validationErr
	)
	switch {
	case errors.As(err, &usage):
		return ExitUsage
	case errors.As(err, &config):
		return ExitConfig
	case errors.As(err, &notFound):
		return ExitNotFound
	case errors.As(err, &unavailable):
		return ExitUnavailable
	case errors.As(err, &validation):
		return ExitValidation
	default:
		return ExitFailed
	}
}

// wrapf adds context while preserving the error's classification, so a nested
// failure keeps its exit code.
func wrapf(err error, format string, args ...any) error {
	if err == nil {
		return nil
	}
	msg := fmt.Sprintf(format, args...)
	wrapped := fmt.Errorf("%s: %w", msg, err)

	var (
		usage       *usageErr
		config      *configErr
		notFound    *notFoundErr
		unavailable *unavailableErr
		validation  *validationErr
	)
	switch {
	case errors.As(err, &usage):
		return &usageErr{err: wrapped}
	case errors.As(err, &config):
		return &configErr{err: wrapped}
	case errors.As(err, &notFound):
		return &notFoundErr{err: wrapped}
	case errors.As(err, &unavailable):
		return &unavailableErr{err: wrapped}
	case errors.As(err, &validation):
		return &validationErr{err: wrapped}
	default:
		return wrapped
	}
}
