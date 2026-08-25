// Copyright 2026 Henry Zektser.

package apiserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/mcpdoll/mcpdoll/internal/api"
	"github.com/mcpdoll/mcpdoll/internal/controlplane/registry"
	"github.com/mcpdoll/mcpdoll/internal/controlplane/snapshotter"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/snapshot"
	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// maxRequestBytes bounds every request body.
//
// The snapshot endpoints accept an uploaded snapshot, so the limit has to be
// large enough for a real one — a few thousand tools with schemas — and small
// enough that an unauthenticated 500MB POST cannot exhaust memory before the
// auth middleware has even run.
const maxRequestBytes = 32 << 20

func decodeBody(w http.ResponseWriter, r *http.Request, log *slog.Logger, dst any) bool {
	body := http.MaxBytesReader(w, r.Body, maxRequestBytes)
	dec := json.NewDecoder(body)
	// Unknown fields are a client that thinks it is setting something. Silently
	// ignoring `allowUnreachable` when the field is `allow_unreachable` means a
	// build that was supposed to tolerate a down backend fails, and the caller
	// has no way to tell why.
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, log, http.StatusRequestEntityTooLarge, CodeInvalidRequest,
				fmt.Sprintf("request body exceeds %d bytes", maxRequestBytes))
			return false
		}
		writeError(w, log, http.StatusBadRequest, CodeInvalidRequest,
			"request body is not valid JSON for this operation: "+err.Error())
		return false
	}
	// A second value in the stream means the client sent two documents and
	// believes both were applied.
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		writeError(w, log, http.StatusBadRequest, CodeInvalidRequest,
			"request body has trailing content after the JSON object")
		return false
	}
	return true
}

// ValidateRegistryRequest is validateRegistry's body.
type ValidateRegistryRequest struct {
	// Content is the registry document as YAML. Validation runs against what
	// the caller sends rather than what is on the server's disk, which is what
	// makes this usable as a pull-request check.
	Content string `json:"content"`
}

func (s *Server) handleValidateRegistry(w http.ResponseWriter, r *http.Request) {
	var req ValidateRegistryRequest
	if !decodeBody(w, r, s.log, &req) {
		return
	}
	if req.Content == "" {
		writeError(w, s.log, http.StatusBadRequest, CodeInvalidRequest,
			"content is required: send the registry document to validate")
		return
	}

	spec, err := registryParse(req.Content)
	if err != nil {
		writeProblems(w, s.log, "the registry document is not valid", err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, api.NewRegistrySummary(spec))
}

// InspectSnapshotRequest is inspectSnapshot's body.
type InspectSnapshotRequest struct {
	// Content is a base64-encoded signed snapshot.
	//
	// The bytes travel in the request rather than a path travelling in it: a
	// server that reads whatever file path a caller names is an arbitrary file
	// read, and no amount of validating the path afterwards makes that a good
	// starting point. The CLI reads files because it runs as the user.
	Content string `json:"content"`
	// Tools includes every tool rather than summarising by audience.
	Tools bool `json:"tools,omitempty"`
}

func (s *Server) handleInspectSnapshot(w http.ResponseWriter, r *http.Request) {
	var req InspectSnapshotRequest
	if !decodeBody(w, r, s.log, &req) {
		return
	}
	raw, err := base64.StdEncoding.DecodeString(req.Content)
	if err != nil || len(raw) == 0 {
		writeError(w, s.log, http.StatusBadRequest, CodeInvalidRequest,
			"content must be a base64-encoded signed snapshot")
		return
	}

	var signed snapshotpb.SignedSnapshot
	if err := proto.Unmarshal(raw, &signed); err != nil {
		writeError(w, s.log, http.StatusBadRequest, CodeInvalidRequest,
			"content is not a signed snapshot: "+err.Error())
		return
	}
	s.writeSnapshot(w, "uploaded", &signed, req.Tools)
}

func (s *Server) handleGetCurrentSnapshot(w http.ResponseWriter, r *http.Request) {
	if s.cfg.SnapshotPath == "" {
		writeError(w, s.log, http.StatusNotFound, CodeNotFound,
			"this control plane has no snapshot path configured")
		return
	}
	signed, err := snapshot.ReadSignedSnapshot(s.cfg.SnapshotPath)
	if err != nil {
		status, code := http.StatusInternalServerError, CodeInternal
		if errors.Is(err, os.ErrNotExist) {
			status, code = http.StatusNotFound, CodeNotFound
		}
		writeError(w, s.log, status, code,
			fmt.Sprintf("reading %s: %v", s.cfg.SnapshotPath, err))
		return
	}
	s.writeSnapshot(w, s.cfg.SnapshotPath, signed,
		r.URL.Query().Get("tools") == "true")
}

// writeSnapshot renders a snapshot that has NOT had its signature checked.
//
// Deliberate: a snapshot signed by a key this control plane does not hold is
// exactly the one an operator most needs to look at, and refusing to display it
// would turn a diagnosable situation into a blank screen. `verifySnapshot` is
// the operation that answers the signature question, and the rendered result
// says which key signed these bytes so the two can be reconciled.
func (s *Server) writeSnapshot(
	w http.ResponseWriter,
	source string,
	signed *snapshotpb.SignedSnapshot,
	includeTools bool,
) {
	snap, err := snapshot.ParseUnverified(signed)
	if err != nil {
		writeError(w, s.log, http.StatusUnprocessableEntity, CodeValidation,
			"the snapshot could not be parsed: "+err.Error())
		return
	}
	view, buildErr := snapshot.Build(snap)
	reason := ""
	if buildErr != nil {
		reason = buildErr.Error()
		view = nil
	}
	writeJSON(w, s.log, http.StatusOK,
		api.NewSnapshot(source, signed, snap, view, reason, includeTools))
}

// VerifySnapshotRequest is verifySnapshot's body.
type VerifySnapshotRequest struct {
	Content string `json:"content"`
	// TrustedKeys are keyID:base64PublicKey entries. Supplied by the caller
	// rather than taken from server config, so this operation answers "would
	// *this* data plane accept it?" for any data plane, not only the local one.
	TrustedKeys []string `json:"trusted_keys"`
}

func (s *Server) handleVerifySnapshot(w http.ResponseWriter, r *http.Request) {
	var req VerifySnapshotRequest
	if !decodeBody(w, r, s.log, &req) {
		return
	}
	if len(req.TrustedKeys) == 0 {
		writeError(w, s.log, http.StatusBadRequest, CodeInvalidRequest,
			"trusted_keys is required: verifying against no key would always fail, "+
				"and defaulting to the server's own key would answer a different question")
		return
	}

	raw, err := base64.StdEncoding.DecodeString(req.Content)
	if err != nil || len(raw) == 0 {
		writeError(w, s.log, http.StatusBadRequest, CodeInvalidRequest,
			"content must be a base64-encoded signed snapshot")
		return
	}
	var signed snapshotpb.SignedSnapshot
	if err := proto.Unmarshal(raw, &signed); err != nil {
		writeError(w, s.log, http.StatusBadRequest, CodeInvalidRequest,
			"content is not a signed snapshot: "+err.Error())
		return
	}

	verifier, err := snapshot.NewVerifier(req.TrustedKeys)
	if err != nil {
		writeError(w, s.log, http.StatusBadRequest, CodeInvalidRequest,
			"trusted_keys: "+err.Error())
		return
	}
	snap, err := verifier.Verify(&signed)
	if err != nil {
		writeJSON(w, s.log, http.StatusUnprocessableEntity, Error{
			Code:     CodeValidation,
			Message:  "the signature is not valid: " + err.Error(),
			Problems: []string{fmt.Sprintf("snapshot names key %q", signed.KeyId)},
		})
		return
	}

	view, err := snapshot.Build(snap)
	if err != nil {
		writeProblems(w, s.log,
			"the signature is valid but the snapshot would not activate", err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, api.VerifyReport{
		Source: "uploaded", Valid: true, Version: snap.Version,
		KeyID: signed.KeyId, Tenants: view.TenantSlugs(), Tools: len(snap.Tools),
	})
}

// BuildSnapshotRequest is buildSnapshot's body.
type BuildSnapshotRequest struct {
	// AllowUnreachable builds even if a backend is down, omitting its tools.
	AllowUnreachable bool `json:"allow_unreachable,omitempty"`
	// DryRun validates and reports without writing the snapshot file.
	DryRun bool `json:"dry_run,omitempty"`
	// DiscoverTimeoutMs bounds each backend's discovery.
	DiscoverTimeoutMs int `json:"discover_timeout_ms,omitempty"`
	// Concurrency is how many backends to discover at once.
	Concurrency int `json:"concurrency,omitempty"`
	// Version overrides the snapshot version. Absent means a Unix timestamp,
	// which is what a grants-only republish wants: monotonic without anybody
	// coordinating, and nothing to forget to bump.
	Version int64 `json:"version,omitempty"`
}

// buildFailure is a build that did not happen, carrying enough to answer over
// HTTP and little enough to just log.
//
// The build path has two callers now — a request and a timer (ADR 0025) — and
// they need different things from a failure. Returning one of these rather than
// writing to a ResponseWriter is what lets the timer call the same code.
type buildFailure struct {
	status  int
	code    string
	message string
	// Set when the failure is a list of problems with the registry or the
	// discovered backends rather than a single fault. Rendered differently.
	problems error
}

func (b *buildFailure) Error() string { return b.message }

// buildAndPublish resolves the registry, builds a snapshot, and — unless this
// is a dry run or nothing changed — signs and writes it.
//
// Callable without a ResponseWriter on purpose: the rebuild loop runs this on a
// timer, and a build that only a request could trigger is the thing ADR 0025
// removes.
func (s *Server) buildAndPublish(
	ctx context.Context, req BuildSnapshotRequest,
) (api.BuildReport, *buildFailure) {
	if s.cfg.SigningKeyPath == "" {
		return api.BuildReport{}, &buildFailure{
			status: http.StatusNotFound, code: CodeNotFound,
			message: "this control plane holds no signing key, so it cannot build a snapshot; " +
				"build with `mcpdoll snapshot build` where the key lives",
		}
	}

	spec, err := registry.Load(s.cfg.RegistryPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return api.BuildReport{}, &buildFailure{
				status: http.StatusNotFound, code: CodeNotFound,
				message: fmt.Sprintf("no registry document at %s", s.cfg.RegistryPath),
			}
		}
		return api.BuildReport{}, &buildFailure{
			message:  fmt.Sprintf("%s is not a valid registry document", s.cfg.RegistryPath),
			problems: err,
		}
	}

	priv, err := snapshot.LoadPrivateKey(s.cfg.SigningKeyPath)
	if err != nil {
		s.log.Error("loading signing key failed", "error", err.Error())
		return api.BuildReport{}, &buildFailure{
			status: http.StatusInternalServerError, code: CodeInternal,
			message: "the configured signing key could not be loaded",
		}
	}
	signer, err := snapshot.NewSigner(s.cfg.SigningKeyID, priv)
	if err != nil {
		return api.BuildReport{}, &buildFailure{
			status: http.StatusInternalServerError, code: CodeInternal,
			message: "the configured signing key is unusable: " + err.Error(),
		}
	}

	opts := snapshotter.Options{
		Spec:             spec,
		Signer:           signer,
		AllowUnreachable: req.AllowUnreachable,
		Concurrency:      req.Concurrency,
		Version:          req.Version,
	}

	// Tenancy and RBAC come from the database. Without them a binding names a
	// tenant the build does not carry, which is a build failure — correctly, so
	// the message names the missing half rather than producing a snapshot that
	// admits tools for a tenant no principal could belong to.
	//
	// readAt is when the state was read, and it is what makes pruning safe
	// below: anything revoked after this moment is *not* in the snapshot, so
	// pruning it would silently un-revoke a credential.
	readAt := time.Now()
	if s.cfg.Store != nil {
		state, err := s.cfg.Store.SnapshotState(ctx)
		if err != nil {
			return api.BuildReport{}, &buildFailure{
				status: http.StatusInternalServerError, code: CodeInternal,
				message: "the tenancy state could not be read: " + err.Error(),
			}
		}
		opts.Tenants = state.Tenants
	}
	if req.DiscoverTimeoutMs > 0 {
		opts.DiscoverTimeout = time.Duration(req.DiscoverTimeoutMs) * time.Millisecond
	}

	result, err := snapshotter.Build(ctx, opts)
	if err != nil {
		return api.BuildReport{}, &buildFailure{
			message: "the snapshot could not be built", problems: err,
		}
	}

	report := api.BuildReport{
		Version:        result.Snapshot.Version,
		SnapshotID:     result.Snapshot.Id,
		RegistryDigest: result.Snapshot.RegistryDigest,
		KeyID:          signer.KeyID(),
		PublicKey:      snapshot.TrustedKeyEntry(signer.KeyID(), signer.PublicKey()),
		Namespaces:     len(result.Snapshot.Namespaces),
		Servers:        len(result.Snapshot.Servers),
		Tools:          len(result.Snapshot.Tools),
		Toolsets:       len(result.Snapshot.Toolsets),
		Plugins:        len(result.Snapshot.Plugins),
		Backends:       backendReports(result.Discovered),
		Warnings:       result.Warnings,
		DryRun:         req.DryRun,
	}

	if req.DryRun {
		return report, nil
	}
	if s.cfg.SnapshotPath == "" {
		return api.BuildReport{}, &buildFailure{
			status: http.StatusConflict, code: CodeInvalidRequest,
			message: "this control plane has no snapshot path configured; " +
				"pass dry_run to build without writing",
		}
	}

	// Publishing an identical snapshot is not free: the data plane recomposes
	// every principal's view on a version change, so a timer that republished
	// every minute would churn that cache all day to deliver nothing.
	//
	// This is why `last_built_at` is reported separately from the version — see
	// [Server.RebuildState]. A version that only moves on change cannot, on its
	// own, distinguish a quiet deployment from a rebuild loop that has died.
	if same, err := publishedIsIdentical(s.cfg.SnapshotPath, result.Snapshot); err != nil {
		s.log.Warn("comparing against the published snapshot failed", "error", err)
	} else if same {
		report.Unchanged = true
		report.Output = s.cfg.SnapshotPath
		return report, nil
	}

	if err := snapshot.WriteSignedSnapshot(s.cfg.SnapshotPath, result.Signed); err != nil {
		s.log.Error("writing snapshot failed", "error", err.Error())
		return api.BuildReport{}, &buildFailure{
			status: http.StatusInternalServerError, code: CodeInternal,
			message: "the snapshot was built but could not be written",
		}
	}
	report.Output = s.cfg.SnapshotPath

	// A revocation is redundant once a snapshot built after it is serving,
	// because that snapshot already omits the credential. Pruning here is
	// what stops the signed list growing forever.
	//
	// Only after the file is written: pruning against a snapshot that never
	// reached disk would drop denials nothing else carries.
	if s.cfg.Store != nil {
		if _, err := s.cfg.Store.PruneRevocations(
			ctx, result.Snapshot.Version, readAt); err != nil {
			// Not fatal. The snapshot is published and correct; the list is
			// merely larger than it needs to be, which costs bytes rather
			// than safety.
			s.log.Warn("pruning revocations failed", "error", err)
		} else if problem := s.publishRevocations(ctx); problem != "" {
			report.Warnings = append(report.Warnings, problem)
		}
	}
	return report, nil
}

func (s *Server) handleBuildSnapshot(w http.ResponseWriter, r *http.Request) {
	var req BuildSnapshotRequest
	if !decodeBody(w, r, s.log, &req) {
		return
	}
	report, fail := s.buildAndPublish(r.Context(), req)
	if fail != nil {
		if fail.problems != nil {
			writeProblems(w, s.log, fail.message, fail.problems)
			return
		}
		writeError(w, s.log, fail.status, fail.code, fail.message)
		return
	}
	s.noteRebuild(report, nil)
	writeJSON(w, s.log, http.StatusOK, report)
}

func backendReports(in []snapshotter.BackendReport) []api.BackendReport {
	out := make([]api.BackendReport, 0, len(in))
	for _, b := range in {
		admitted := b.Admitted
		if admitted == nil {
			admitted = []string{}
		}
		out = append(out, api.BackendReport{
			ServerID: b.ServerID, ServerName: b.ServerName, Endpoint: b.Endpoint,
			NegotiatedVersion: b.NegotiatedVersion, ToolCount: b.ToolCount,
			Admitted: admitted, Excluded: b.Excluded,
			ObservedAt: b.ObservedAt.UTC().Format(time.RFC3339),
		})
	}
	return out
}

// GenerateSigningKeyRequest is generateSigningKey's body.
type GenerateSigningKeyRequest struct {
	KeyID string `json:"key_id"`
}

func (s *Server) handleGenerateSigningKey(w http.ResponseWriter, r *http.Request) {
	var req GenerateSigningKeyRequest
	if !decodeBody(w, r, s.log, &req) {
		return
	}
	if req.KeyID == "" {
		writeError(w, s.log, http.StatusBadRequest, CodeInvalidRequest,
			"key_id is required: it is recorded in every snapshot this key signs, "+
				"and a verifier selects the public key by it")
		return
	}
	if !safeKeyID(req.KeyID) {
		writeError(w, s.log, http.StatusBadRequest, CodeInvalidRequest,
			"key_id must be letters, digits, hyphen, or underscore: it becomes a filename")
		return
	}
	if s.cfg.KeyDir == "" {
		writeError(w, s.log, http.StatusNotFound, CodeNotFound,
			"this control plane has no key directory configured, so it will not "+
				"mint a key it cannot store; use `mcpdoll keys generate`")
		return
	}

	pub, priv, err := snapshot.GenerateKey()
	if err != nil {
		s.log.Error("generating a signing key failed", "error", err.Error())
		writeError(w, s.log, http.StatusInternalServerError, CodeInternal,
			"the key could not be generated")
		return
	}
	if err := snapshot.WriteKeyPair(s.cfg.KeyDir, req.KeyID, pub, priv); err != nil {
		s.log.Error("writing a signing key failed", "error", err.Error())
		writeError(w, s.log, http.StatusInternalServerError, CodeInternal,
			"the key was generated but could not be written")
		return
	}

	// The private half stays on the server's disk. Returning it would put a key
	// that can publish configuration to every data-plane instance into a
	// browser's memory, a proxy's access log, and whatever the client does next.
	writeJSON(w, s.log, http.StatusCreated, api.SigningKey{
		KeyID:      req.KeyID,
		Directory:  s.cfg.KeyDir,
		PublicKey:  base64.StdEncoding.EncodeToString(pub),
		TrustEntry: snapshot.TrustedKeyEntry(req.KeyID, pub),
	})
}

func safeKeyID(id string) bool {
	if len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

// registryParse validates a registry document supplied in a request.
//
// It goes through a temporary file rather than reimplementing registry.Load's
// parsing, because the value of this endpoint is that it applies *exactly* the
// same rules a build will. A second parser that drifts by one rule would make
// this check worse than useless: it would say yes and then the build would say
// no.
func registryParse(content string) (*registry.Spec, error) {
	f, err := os.CreateTemp("", "mcpdoll-registry-*.yaml")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())

	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return registry.Load(f.Name())
}
