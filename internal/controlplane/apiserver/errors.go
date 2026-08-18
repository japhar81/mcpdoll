// Copyright 2026 The MCPDoll Authors.

package apiserver

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/mcpdoll/mcpdoll/internal/controlplane/registry"
)

// Error is the single failure shape every operation returns.
//
// One shape rather than per-endpoint variants, because a client that has to
// branch on which endpoint failed in order to read the message will not bother,
// and will show the user a status code.
type Error struct {
	// Code is a stable machine-readable token. Clients switch on this; the
	// message is for humans and may be reworded.
	Code string `json:"code"`
	// Message is one sentence, written for whoever has to fix the problem.
	Message string `json:"message"`
	// Problems lists every distinct failure. Validation returns all of them at
	// once: a document with six errors should take one round trip to fix, not
	// six.
	Problems []string `json:"problems,omitempty"`
}

// The codes. Adding one is a contract change.
const (
	CodeInvalidRequest = "invalid_request"
	CodeNotFound       = "not_found"
	CodeForbidden      = "forbidden"
	CodeValidation     = "validation_failed"
	CodeUnavailable    = "upstream_unavailable"
	CodeInternal       = "internal_error"
)

func writeJSON(w http.ResponseWriter, log *slog.Logger, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// No caching by default. Several of these responses are identity-scoped and
	// all of them are cheap; a stale registry in a proxy is worse than a
	// re-fetch.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status line is already sent, so this cannot be turned into a 500.
		// Logging it is the only honest option; silently swallowing it would
		// leave a truncated body and no trace of why.
		log.Error("encoding response failed after the header was written",
			slog.String("error", err.Error()))
	}
}

func writeError(w http.ResponseWriter, log *slog.Logger, status int, code, message string, problems ...string) {
	writeJSON(w, log, status, Error{Code: code, Message: message, Problems: problems})
}

// writeProblems renders a validation failure with every problem listed.
//
// registry.Load returns a joined error holding one entry per problem, and
// flattening it back out is what makes the console able to render a list rather
// than a paragraph.
func writeProblems(w http.ResponseWriter, log *slog.Logger, message string, err error) {
	problems := flatten(err)
	writeError(w, log, http.StatusUnprocessableEntity, CodeValidation, message, problems...)
}

func flatten(err error) []string {
	if err == nil {
		return nil
	}
	// A registry validation carries its problems as a slice, so they arrive
	// structured rather than needing the formatted message split apart.
	var validation *registry.ValidationError
	if errors.As(err, &validation) && len(validation.Problems) > 0 {
		return validation.Problems
	}
	var joined interface{ Unwrap() []error }
	if errors.As(err, &joined) {
		var out []string
		for _, e := range joined.Unwrap() {
			out = append(out, flatten(e)...)
		}
		if len(out) > 0 {
			return out
		}
	}
	return []string{err.Error()}
}
