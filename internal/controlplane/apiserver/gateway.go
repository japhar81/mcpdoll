// Copyright 2026 Henry Zektser.

package apiserver

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mcpdoll/mcpdoll/internal/controlplane/inspector"
)

// CallToolRequest is callTool's body.
//
// The tool name and audience are in the path rather than here: they identify
// the thing being acted on, and having them in one place means a request cannot
// disagree with its own URL.
type CallToolRequest struct {
	// Credential is the API key to act as. Inspection presents what the
	// principal presents rather than re-deriving what they should see
	// (ADR 0019).
	Credential string         `json:"credential"`
	Subject    string         `json:"subject,omitempty"`
	Groups     []string       `json:"groups,omitempty"`
	Arguments  map[string]any `json:"arguments,omitempty"`

	// RequestState continues a deferred call. It is the signed envelope the
	// gateway issued, echoed back unchanged.
	RequestState string `json:"request_state,omitempty"`
	// Responses answers the gateway's input requests, keyed by request id.
	// Each value is accept, decline, cancel, or text:<value>.
	Responses map[string]string `json:"responses,omitempty"`
}

func (s *Server) handleCallTool(w http.ResponseWriter, r *http.Request) {
	var req CallToolRequest
	if !decodeBody(w, r, s.log, &req) {
		return
	}

	responses, err := parseResponses(req.Responses)
	if err != nil {
		writeError(w, s.log, http.StatusBadRequest, CodeInvalidRequest, err.Error())
		return
	}

	result, err := s.inspectorClient(r).Call(r.Context(), inspector.CallRequest{
		Credential:   req.Credential,
		Tool:         chi.URLParam(r, "toolName"),
		Arguments:    req.Arguments,
		Identity:     inspector.Identity{Subject: req.Subject, Groups: req.Groups},
		RequestState: req.RequestState,
		Responses:    responses,
	})
	if err != nil {
		s.writeInspectorError(w, err)
		return
	}

	// A tool that returned an error is a successful inspection: the operator
	// asked what happens when this tool is called, and this is what happens.
	// Mapping it to a 4xx would make "the gateway denied it" indistinguishable
	// from "your request was malformed".
	writeJSON(w, s.log, http.StatusOK, result)
}

// parseResponses turns the request's answer map into MCP input responses.
//
// Only elicitation is supported. That covers confirmation, which is the case a
// human actually completes from a console; sampling and roots requests are
// answered by a client framework, not by a person clicking a button.
func parseResponses(in map[string]string) (sdk.InputResponseMap, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := sdk.InputResponseMap{}
	for id, value := range in {
		switch {
		case value == "accept" || value == "decline" || value == "cancel":
			out[id] = &sdk.ElicitResult{Action: value}
		case strings.HasPrefix(value, "text:"):
			out[id] = &sdk.ElicitResult{
				Action:  "accept",
				Content: map[string]any{"value": strings.TrimPrefix(value, "text:")},
			}
		default:
			return nil, &responseError{id: id, value: value}
		}
	}
	return out, nil
}

type responseError struct{ id, value string }

func (e *responseError) Error() string {
	return "responses[" + e.id + "] = " + e.value +
		": must be accept, decline, cancel, or text:<value>"
}
