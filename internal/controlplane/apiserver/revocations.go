// Copyright 2026 The MCPDoll Authors.

package apiserver

import (
	"context"
	"net/http"
	"time"

	"github.com/mcpdoll/mcpdoll/internal/api"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/snapshot"
)

// Publishing the revocation list.
//
// Every operation that revokes something calls [Server.publishRevocations]
// before it answers. That ordering matters: returning success and then writing
// the file would mean an operator who revokes a leaked key and immediately
// checks sees "revoked" while the credential still works.

// RevocationHeartbeat is how often the list is republished with nothing new.
//
// Without it, "how old is the list I hold" grows forever in a healthy system —
// a deployment that revoked something last Tuesday would show an eight-day-old
// list and there would be nothing to alert on. Republishing on a timer makes
// the age bounded when distribution is working, so a *growing* age means the
// data plane has stopped receiving the artifact.
//
// That is the number ADR 0023 promised: the maximum staleness of the gateway's
// revocation knowledge, which is exactly the window in which a revocation
// issued now would not yet be enforced.
const RevocationHeartbeat = 30 * time.Second

// RunRevocationHeartbeat republishes the list until ctx is cancelled.
//
// It bumps the version each time. The version has no meaning beyond
// monotonicity — the data plane refuses anything not newer than what it holds —
// so using it as a heartbeat counter costs nothing and avoids a second
// freshness field with its own comparison rules.
func (s *Server) RunRevocationHeartbeat(ctx context.Context) {
	if s.cfg.Store == nil || s.cfg.RevocationsPath == "" {
		return
	}

	// Once at startup, so a data plane that restarts is not waiting a full
	// interval for its first list — and so a deployment that has never revoked
	// anything still publishes an empty, signed one. A list that exists and is
	// empty proves the pipeline works before anybody needs it to.
	if problem := s.bumpAndPublish(ctx); problem != "" {
		s.log.Warn("initial revocation publish failed", "problem", problem)
	}

	ticker := time.NewTicker(RevocationHeartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if problem := s.bumpAndPublish(ctx); problem != "" {
				s.log.Warn("revocation heartbeat failed", "problem", problem)
			}
		}
	}
}

func (s *Server) bumpAndPublish(ctx context.Context) string {
	if _, err := s.cfg.Store.BumpRevocationVersion(ctx); err != nil {
		return err.Error()
	}
	return s.publishRevocations(ctx)
}

// publishRevocations rebuilds, signs, and writes the list.
//
// Returns a problem string rather than an error, because the caller has already
// done the thing the user asked for. A revocation that reached the database and
// failed to publish is *not* a failed revocation — it is one that will take
// effect at snapshot latency instead of immediately, which is exactly the
// pre-ADR-0023 behaviour. Reporting it as a failure would invite a retry that
// revokes nothing new.
func (s *Server) publishRevocations(ctx context.Context) string {
	if s.cfg.Store == nil {
		return ""
	}
	if s.cfg.RevocationsPath == "" {
		return "this control plane has no revocations_path configured, so this takes " +
			"effect at the next snapshot rather than immediately"
	}
	if s.cfg.SigningKeyPath == "" {
		return "this control plane holds no signing key, so it cannot publish a " +
			"revocation list; this takes effect at the next snapshot"
	}

	list, err := s.cfg.Store.RevocationList(ctx)
	if err != nil {
		s.log.Error("building the revocation list failed", "error", err)
		return "the revocation was recorded but the list could not be built: " + err.Error()
	}

	priv, err := snapshot.LoadPrivateKey(s.cfg.SigningKeyPath)
	if err != nil {
		s.log.Error("loading the signing key failed", "error", err)
		return "the revocation was recorded but the signing key could not be loaded"
	}
	signer, err := snapshot.NewSigner(s.cfg.SigningKeyID, priv)
	if err != nil {
		return "the revocation was recorded but the signing key is unusable: " + err.Error()
	}
	signed, err := signer.SignRevocations(list)
	if err != nil {
		s.log.Error("signing the revocation list failed", "error", err)
		return "the revocation was recorded but the list could not be signed"
	}
	if err := snapshot.WriteSignedRevocations(s.cfg.RevocationsPath, signed); err != nil {
		s.log.Error("writing the revocation list failed", "error", err)
		return "the revocation was recorded but the list could not be written to " +
			s.cfg.RevocationsPath
	}

	s.log.Info("published revocation list",
		"version", list.Version, "principals", len(list.PrincipalIds))
	return ""
}

func (s *Server) handleGetRevocations(w http.ResponseWriter, r *http.Request) {
	out := api.RevocationReport{
		Path:        s.cfg.RevocationsPath,
		Revocations: []api.Revocation{},
	}

	if st := s.cfg.Store; st != nil {
		state, err := st.RevocationState(r.Context())
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		out.Version = state.Version
		out.PrunedThrough = state.PrunedThrough

		entries, err := st.ListRevocations(r.Context())
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		for _, e := range entries {
			rev := api.Revocation{
				PrincipalID: e.PrincipalID.String(),
				Kind:        e.Kind,
				Reason:      e.Reason,
				RevokedAt:   e.RevokedAt.UTC().Format(time.RFC3339),
			}
			if e.UserID != nil {
				rev.UserID = e.UserID.String()
			}
			out.Revocations = append(out.Revocations, rev)
		}
	}

	// What the *data plane* is actually applying, which is the number that
	// matters. The control plane's own version says what it published; the gap
	// between the two is the exposure window (ADR 0023).
	if status, err := s.inspectorClient(r).Status(r.Context()); err == nil {
		out.ServingVersion = status.RevocationsVersion
		out.ServingAgeSeconds = status.RevocationsAgeSeconds
		out.InEffect = status.RevocationsVersion >= out.Version
	} else {
		s.log.Warn("reporting revocations without live gateway state", "error", err)
	}

	if s.cfg.RevocationsPath == "" {
		out.Warning = "no revocations_path is configured, so revoking a credential " +
			"takes effect at the next snapshot rather than immediately"
	} else if !out.InEffect {
		out.Warning = "the gateway has not yet applied the published list; a revoked " +
			"credential still works until it does"
	}
	writeJSON(w, s.log, http.StatusOK, out)
}
