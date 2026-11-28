package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/axin2024/agent-memory/internal/memory"
)

type teamRoute func(http.ResponseWriter, *http.Request, memory.Identity) (any, error)

func (e Endpoint) teamRoute(run teamRoute) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, present := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !present {
			e.reply(w, r, nil, memory.ErrDenied)
			return
		}
		who, err := e.service.Authenticate(r.Context(), token)
		if err != nil {
			e.reply(w, r, nil, err)
			return
		}
		body, err := run(w, r, who)
		if errors.Is(err, memory.ErrInput) {
			problem := map[string]any{"error": "invalid-input"}
			if _, detail, ok := strings.Cut(err.Error(), ": "); ok {
				problem["field"] = detail
			}
			if claim, ok := body.(memory.TeamClaim); ok && len(claim.MissingGrants) > 0 {
				problem["missing_grants"] = claim.MissingGrants
			}
			writeTeamJSON(w, http.StatusUnprocessableEntity, problem)
			return
		}
		if err == nil && r.Method == http.MethodPost {
			if claim, ok := body.(memory.TeamClaim); ok && strings.HasSuffix(r.URL.Path, "/claims") && !claim.Replay {
				w.Header().Set("Location", "/api/team-spaces/"+claim.Space+"/claims/"+claim.ID+"?purpose="+claim.Purpose)
				writeTeamJSON(w, http.StatusCreated, claim)
				return
			}
		}
		e.reply(w, r, body, err)
	}
}

func writeTeamJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func (e Endpoint) teamRoutes(mux *http.ServeMux) {
	s := e.service
	bind := func(pattern string, run teamRoute) { mux.HandleFunc(pattern, e.teamRoute(run)) }
	space := func(r *http.Request) string { return r.PathValue("space") }
	purpose := func(r *http.Request) string { return r.URL.Query().Get("purpose") }
	id := func(r *http.Request) string { return r.PathValue("id") }
	key := func(r *http.Request) string { return r.Header.Get("Idempotency-Key") }
	bind("GET /api/team-spaces", func(_ http.ResponseWriter, r *http.Request, who memory.Identity) (any, error) {
		return s.TeamSpaces(r.Context(), who)
	})
	for _, prefix := range []string{"/team-spaces/{space}", "/api/team-spaces/{space}"} {
		bind("POST "+prefix+"/claims", func(w http.ResponseWriter, r *http.Request, who memory.Identity) (any, error) {
			return input(w, r, func(in memory.TeamClaimInput) (any, error) {
				if in.UseModel {
					ctx, cancel := teamMaintenance(w, r)
					defer cancel()
					return s.CreateTeamClaim(ctx, who, space(r), key(r), in, e.model)
				}
				return s.CreateTeamClaim(r.Context(), who, space(r), key(r), in, e.model)
			})
		})
	}
	const base = "/api/team-spaces/{space}"
	bind("PUT "+base, func(w http.ResponseWriter, r *http.Request, who memory.Identity) (any, error) {
		return input(w, r, func(in memory.TeamPolicy) (any, error) {
			return s.SetTeamPolicy(r.Context(), who, space(r), in)
		})
	})
	bind("PUT "+base+"/membership", func(w http.ResponseWriter, r *http.Request, who memory.Identity) (any, error) {
		return input(w, r, func(in memory.TeamMembership) (any, error) {
			return s.SetTeamMembership(r.Context(), who, space(r), in)
		})
	})
	for _, suffix := range []string{"/nominations", "/nominations/preview"} {
		bind("POST "+base+suffix, func(w http.ResponseWriter, r *http.Request, who memory.Identity) (any, error) {
			return input(w, r, func(in memory.NominationInput) (any, error) {
				return s.Nominate(r.Context(), who, space(r), in, suffix == "/nominations/preview")
			})
		})
	}
	bind("GET "+base+"/claims/{id}", func(_ http.ResponseWriter, r *http.Request, who memory.Identity) (any, error) {
		return s.TeamClaim(r.Context(), who, space(r), purpose(r), id(r))
	})
	bind("POST "+base+"/claims/{id}/votes", func(w http.ResponseWriter, r *http.Request, who memory.Identity) (any, error) {
		return input(w, r, func(in memory.TeamVote) (any, error) {
			return s.VoteTeamClaim(r.Context(), who, space(r), purpose(r), id(r), in)
		})
	})
	bind("GET "+base+"/review-queue", func(_ http.ResponseWriter, r *http.Request, who memory.Identity) (any, error) {
		return s.TeamQueue(r.Context(), who, space(r), purpose(r))
	})
	bind("POST "+base+"/claims/{id}/assignment", func(w http.ResponseWriter, r *http.Request, who memory.Identity) (any, error) {
		return input(w, r, func(in struct {
			Assignee string `json:"assignee"`
		}) (any, error) {
			err := s.AssignTeamReview(r.Context(), who, space(r), purpose(r), id(r), in.Assignee)
			return map[string]bool{"applied": err == nil}, err
		})
	})
	for _, suffix := range []string{"/search", "/export"} {
		bind("POST "+base+suffix, func(w http.ResponseWriter, r *http.Request, who memory.Identity) (any, error) {
			return input(w, r, func(in memory.TeamQuery) (any, error) {
				in.Export = suffix == "/export"
				return s.SearchTeam(r.Context(), who, space(r), in, e.embed)
			})
		})
	}
	bind("POST "+base+"/reindex", func(w http.ResponseWriter, r *http.Request, who memory.Identity) (any, error) {
		ctx, cancel := teamMaintenance(w, r)
		defer cancel()
		return s.ReindexTeam(ctx, who, space(r), purpose(r), e.embed)
	})
	bind("POST "+base+"/evaluations", func(w http.ResponseWriter, r *http.Request, who memory.Identity) (any, error) {
		var in memory.TeamEvaluationInput
		if err := readBounded(w, r, &in, 192*1024); err != nil {
			return nil, err
		}
		ctx, cancel := teamMaintenance(w, r)
		defer cancel()
		return s.EvaluateTeam(ctx, who, space(r), in, e.embed)
	})
	bind("GET "+base+"/evaluations/{id}", func(_ http.ResponseWriter, r *http.Request, who memory.Identity) (any, error) {
		return s.TeamEvaluationReceipt(r.Context(), who, space(r), purpose(r), id(r))
	})
	bind("GET /api/team-nominations/{id}/impact", func(_ http.ResponseWriter, r *http.Request, who memory.Identity) (any, error) {
		return s.PreviewTeamWithdrawal(r.Context(), who, id(r))
	})
	bind("POST /api/team-nominations/{id}/withdraw", func(_ http.ResponseWriter, r *http.Request, who memory.Identity) (any, error) {
		return s.WithdrawTeamNomination(r.Context(), who, id(r), key(r))
	})
	bind("GET /api/team-withdrawals/{id}", func(_ http.ResponseWriter, r *http.Request, who memory.Identity) (any, error) {
		return s.TeamWithdrawalStatus(r.Context(), who, id(r))
	})
}

func teamMaintenance(w http.ResponseWriter, r *http.Request) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(4 * time.Minute)
	_ = http.NewResponseController(w).SetWriteDeadline(deadline.Add(5 * time.Second))
	return context.WithDeadline(r.Context(), deadline)
}
