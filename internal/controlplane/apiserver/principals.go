// Copyright 2026 Henry Zektser.

package apiserver

import (
	"context"
	"net/http"
	"time"

	"github.com/mcpdoll/mcpdoll/internal/dataplane/snapshot"
)

// Publishing the principal set.
//
// Every operation that changes who exists or what they hold calls this before
// answering. It costs a few database reads and a signature — no discovery — so
// minting a key or issuing a grant is usable in about a second rather than at
// the next snapshot (ADR 0024).

// PrincipalHeartbeat is how often the set is republished unchanged.
//
// The same reasoning as the revocation list's: without it, "how old is the set
// the gateway holds" grows forever in a healthy system and there is nothing to
// alert on. With it, a growing age means the data plane has stopped receiving
// the artifact.
const PrincipalHeartbeat = 30 * time.Second

// publishPrincipals rebuilds, signs, and writes the set.
//
// Returns a problem string rather than an error, for the same reason
// [Server.publishRevocations] does: the caller has already done what the user
// asked. A grant that reached the database and failed to publish is not a
// failed grant — it is one that takes effect at the next publish instead of
// immediately, and reporting it as a failure invites a retry that changes
// nothing.
func (s *Server) publishPrincipals(ctx context.Context) string {
	if s.cfg.Store == nil {
		return ""
	}
	if s.cfg.PrincipalsPath == "" {
		return "this control plane has no principals_path configured, so this takes " +
			"effect at the next snapshot rather than immediately"
	}
	if s.cfg.SigningKeyPath == "" {
		return "this control plane holds no signing key, so it cannot publish a " +
			"principal set; this takes effect at the next snapshot"
	}

	set, err := s.cfg.Store.PublishPrincipalSet(ctx)
	if err != nil {
		s.log.Error("building the principal set failed", "error", err)
		return "the change was saved but the principal set could not be built: " + err.Error()
	}

	priv, err := snapshot.LoadPrivateKey(s.cfg.SigningKeyPath)
	if err != nil {
		s.log.Error("loading the signing key failed", "error", err)
		return "the change was saved but the signing key could not be loaded"
	}
	signer, err := snapshot.NewSigner(s.cfg.SigningKeyID, priv)
	if err != nil {
		return "the change was saved but the signing key is unusable: " + err.Error()
	}
	signed, err := signer.SignPrincipals(set)
	if err != nil {
		s.log.Error("signing the principal set failed", "error", err)
		return "the change was saved but the principal set could not be signed"
	}
	if err := snapshot.WriteSignedPrincipals(s.cfg.PrincipalsPath, signed); err != nil {
		s.log.Error("writing the principal set failed", "error", err)
		return "the change was saved but the principal set could not be written to " +
			s.cfg.PrincipalsPath
	}

	s.log.Info("published principal set",
		"version", set.Version, "principals", len(set.Principals))
	return ""
}

// RunPrincipalHeartbeat republishes the set until ctx is cancelled.
func (s *Server) RunPrincipalHeartbeat(ctx context.Context) {
	if s.cfg.Store == nil || s.cfg.PrincipalsPath == "" {
		return
	}
	if problem := s.publishPrincipals(ctx); problem != "" {
		s.log.Warn("initial principal publish failed", "problem", problem)
	}

	ticker := time.NewTicker(PrincipalHeartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if problem := s.publishPrincipals(ctx); problem != "" {
				s.log.Warn("principal heartbeat failed", "problem", problem)
			}
		}
	}
}

// warnPrincipals stamps a publish problem on a response that otherwise
// succeeded, so a caller who checks immediately is not told a change is live
// when it is not.
func warnPrincipals(w http.ResponseWriter, problem string) {
	if problem != "" {
		w.Header().Set("X-MCPDoll-Warning", problem)
	}
}
