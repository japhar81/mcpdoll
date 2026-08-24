// Copyright 2026 Henry Zektser.

// Command redact removes sensitive patterns from tool results before they reach
// a model.
//
// It runs at ON_TOOL_RESULT, which is where content the operator never reviewed
// arrives. A backend that returns a customer record containing a card number has
// not necessarily done anything wrong — but the model does not need the card
// number, and once it is in the context window it is in the transcript, the
// logs, and any downstream tool call the model chooses to make.
//
// Configuration (from the plugin manifest's `config`):
//
//	patterns:    extra regular expressions to redact, beyond the built-ins
//	builtins:    false to disable the built-in patterns
//	placeholder: what a match is replaced with (default "[REDACTED]")
//	annotate:    true to also record what was redacted, for the audit trail
//
// Build:
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o redact.wasm .
package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/mcpdoll/mcpdoll/plugins/sdk"
)

// builtinPatterns are the shapes worth redacting by default.
//
// Each is deliberately specific. A pattern that matched "any 16 digits" would
// redact order numbers and timestamps, and a redaction plugin that mangles
// legitimate content is one an operator turns off — at which point it protects
// nothing. Precision matters more than coverage here.
var builtinPatterns = []struct {
	name    string
	pattern *regexp.Regexp
}{
	{
		// Card numbers, with the Luhn-ish grouping real ones use. Requires
		// separators or a known prefix so a bare 16-digit id does not match.
		name: "card_number",
		pattern: regexp.MustCompile(
			`\b(?:4[0-9]{3}|5[1-5][0-9]{2}|3[47][0-9]{2}|6(?:011|5[0-9]{2}))[- ]?[0-9]{4}[- ]?[0-9]{4}[- ]?[0-9]{4}\b`),
	},
	{
		// US SSN. Requires the hyphens: nine bare digits are far too common.
		name:    "ssn",
		pattern: regexp.MustCompile(`\b[0-9]{3}-[0-9]{2}-[0-9]{4}\b`),
	},
	{
		name: "jwt",
		pattern: regexp.MustCompile(
			`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`),
	},
	{
		name: "bearer_token",
		pattern: regexp.MustCompile(
			`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{20,}`),
	},
	{
		name: "provider_api_key",
		pattern: regexp.MustCompile(
			`\b(?:sk|pk|rk|ghp|gho|ghs|ghu|glpat|xoxb|xoxp|xapp)[-_][A-Za-z0-9_-]{16,}\b`),
	},
	{
		name:    "private_key_block",
		pattern: regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	},
	{
		// AWS access key ids have a fixed, unmistakable shape.
		name:    "aws_access_key_id",
		pattern: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),
	},
}

// Registration happens in init, not main: a reactor module never runs main.
// See sdk.Handle.
func init() { sdk.Handle(handle) }

func main() {}

func handle(inv *sdk.Invocation) *sdk.Verdict {
	// Only results. Redacting a *request* would silently change what the user
	// asked for, which is a different and much more surprising behaviour than
	// removing something from what came back.
	if inv.Result == nil || len(inv.Result.Content) == 0 {
		return sdk.AllowVerdict()
	}

	patterns, err := compilePatterns(inv)
	if err != nil {
		// A bad configuration is the operator's problem to fix, and it should be
		// visible. Allowing rather than denying is the right failure here: the
		// engine's per-effect-class failure policy decides whether a broken check
		// blocks traffic, and that is not the plugin's call.
		return &sdk.Verdict{
			Decision: sdk.Allow,
			Reason:   "redact: " + err.Error(),
			Annotations: map[string]any{
				"config_error": err.Error(),
			},
		}
	}

	placeholder := inv.ConfigString("placeholder", "[REDACTED]")

	var ops []sdk.PatchOp
	counts := map[string]int{}

	for i, block := range inv.Result.Content {
		if block.Text == "" {
			continue
		}
		redacted, hits := redact(block.Text, patterns, placeholder)
		if redacted == block.Text {
			continue
		}
		// Patch only the text, not the whole block: a narrower patch is easier
		// to read in the console's diff and cannot disturb a field this plugin
		// has no opinion about.
		ops = append(ops, sdk.Replace(
			fmt.Sprintf("/result/content/%d/text", i), redacted))
		for name, n := range hits {
			counts[name] += n
		}
	}

	if len(ops) == 0 {
		return sdk.AllowVerdict()
	}

	verdict := sdk.MutateVerdict(summarize(counts), ops)
	if inv.ConfigBool("annotate", true) {
		// What was redacted, never the values. An audit trail that recorded the
		// redacted content would defeat the redaction.
		verdict.Annotations = map[string]any{
			"redactions": counts,
			"blocks":     len(ops),
		}
	}
	return verdict
}

type namedPattern struct {
	name    string
	pattern *regexp.Regexp
}

func compilePatterns(inv *sdk.Invocation) ([]namedPattern, error) {
	var out []namedPattern

	if inv.ConfigBool("builtins", true) {
		for _, b := range builtinPatterns {
			out = append(out, namedPattern{name: b.name, pattern: b.pattern})
		}
	}

	for i, expr := range inv.ConfigStrings("patterns") {
		compiled, err := regexp.Compile(expr)
		if err != nil {
			return nil, fmt.Errorf("configured pattern %d is not a valid regular expression: %w", i, err)
		}
		out = append(out, namedPattern{name: fmt.Sprintf("custom_%d", i), pattern: compiled})
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("no patterns configured and builtins are disabled, so nothing would be redacted")
	}
	return out, nil
}

func redact(text string, patterns []namedPattern, placeholder string) (string, map[string]int) {
	counts := map[string]int{}
	out := text
	for _, p := range patterns {
		matches := p.pattern.FindAllString(out, -1)
		if len(matches) == 0 {
			continue
		}
		counts[p.name] = len(matches)
		out = p.pattern.ReplaceAllString(out, placeholder)
	}
	return out, counts
}

// summarize renders what was redacted, for the audit trail and the console.
// Deliberately names the *categories* and counts, never the values.
func summarize(counts map[string]int) string {
	if len(counts) == 0 {
		return "redacted content"
	}
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names)

	parts := make([]string, 0, len(names))
	total := 0
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s×%d", name, counts[name]))
		total += counts[name]
	}
	return fmt.Sprintf("redacted %d match(es): %s", total, strings.Join(parts, ", "))
}
