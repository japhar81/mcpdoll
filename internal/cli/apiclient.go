// Copyright 2026 The MCPDoll Authors.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// The CLI's client for the control-plane API.
//
// Tenants, users, grants, and keys live in a database, and the CLI deliberately
// does not connect to it. Going through the API means one authorization path
// rather than two, and means an operator running `mcpdoll users create` needs a
// bearer token rather than database credentials — which is the difference
// between a laptop that can administer a deployment and a laptop that can read
// every password hash in it.

// apiError is a non-2xx response, carrying the server's own explanation.
type apiError struct {
	Status   int
	Code     string
	Message  string
	Problems []string
}

func (e *apiError) Error() string {
	if len(e.Problems) == 0 {
		return e.Message
	}
	return e.Message + ": " + strings.Join(e.Problems, "; ")
}

// apiCall performs one request and decodes the result into out.
//
// out may be nil for operations that answer 204. body may be nil for reads.
func apiCall(ctx context.Context, env *Env, method, path string, body, out any) error {
	url := strings.TrimRight(env.APIURL, "/") + path

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token := env.Token(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return unavailableError(fmt.Errorf(
			"cannot reach the control plane at %s: %w", env.APIURL, err))
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return unavailableError(fmt.Errorf("reading the response: %w", err))
	}

	if resp.StatusCode >= 400 {
		return classifyAPIError(resp.StatusCode, raw)
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return unavailableError(fmt.Errorf(
			"%s %s returned a body this version cannot read: %w", method, path, err))
	}
	return nil
}

// classifyAPIError turns a status and body into the CLI's typed errors, so the
// exit code matches what actually happened rather than being 1 for everything.
func classifyAPIError(status int, raw []byte) error {
	var payload struct {
		Code     string   `json:"code"`
		Message  string   `json:"message"`
		Problems []string `json:"problems"`
	}
	_ = json.Unmarshal(raw, &payload)
	if payload.Message == "" {
		payload.Message = fmt.Sprintf("the control plane returned %d", status)
	}
	err := &apiError{
		Status: status, Code: payload.Code,
		Message: payload.Message, Problems: payload.Problems,
	}

	switch {
	case status == http.StatusNotFound:
		return notFoundError(err)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		// Not "unavailable": the control plane answered and refused. Reporting
		// a refusal as an outage sends the operator to restart a service that
		// is working exactly as configured.
		return configError(err)
	case status == http.StatusConflict, status == http.StatusUnprocessableEntity:
		return validationError(err)
	case status >= 500:
		return unavailableError(err)
	default:
		return usageError(err)
	}
}

// apiTimeout bounds every management call. Generous, because minting a key runs
// an Argon2id derivation on purpose.
const apiTimeout = 30 * time.Second

func apiContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, apiTimeout)
}
