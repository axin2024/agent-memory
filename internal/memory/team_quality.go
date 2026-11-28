package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

type TeamQueueItem struct {
	ID      string    `json:"claim_id"`
	Topic   string    `json:"topic"`
	State   string    `json:"review_state"`
	Reason  string    `json:"review_reason"`
	Owner   string    `json:"review_owner,omitempty"`
	Expires time.Time `json:"expires_at"`
}

func (s *Service) TeamQueue(ctx context.Context, who Identity, space, purpose string) ([]TeamQueueItem, error) {
	items := []TeamQueueItem{}
	if err := s.lock(ctx); err != nil {
		return nil, err
	}
	defer s.mu.Unlock()
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := teamAccess(ctx, tx, who, space, purpose, "maintainer", "reviewer"); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT claim_id,topic,review_state,
			CASE WHEN expires<=UTC_TIMESTAMP(6) THEN 'evidence-expired' WHEN review_state='published' AND
			expires<=UTC_TIMESTAMP(6)+INTERVAL 7 DAY THEN 'evidence-expires-soon' ELSE COALESCE(NULLIF(review_reason,''),'awaiting-votes') END,
			COALESCE(review_owner,''),expires FROM team_claims WHERE tenant=? AND space_id=? AND purpose=?
			AND (review_queue OR (review_state='published' AND expires<=UTC_TIMESTAMP(6)+INTERVAL 7 DAY))
			ORDER BY expires,claim_id LIMIT 200`, who.Tenant, space, purpose)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item TeamQueueItem
			if err = rows.Scan(&item.ID, &item.Topic, &item.State, &item.Reason, &item.Owner, &item.Expires); err != nil {
				return err
			}
			items = append(items, item)
		}
		return rows.Err()
	})
	return items, err
}

func (s *Service) AssignTeamReview(ctx context.Context, who Identity, space, purpose, id, assignee string) error {
	if !validID(assignee) {
		return ErrInput
	}
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.mu.Unlock()
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := teamAccess(ctx, tx, who, space, purpose, "maintainer"); err != nil {
			return err
		}
		var found bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM team_memberships WHERE tenant=? AND space_id=?
			AND purpose=? AND actor=? AND role IN ('reviewer','maintainer') AND active)`, who.Tenant, space, purpose, assignee).
			Scan(&found); err != nil || !found {
			if err != nil {
				return err
			}
			return ErrMissing
		}
		result, err := tx.ExecContext(ctx, `UPDATE team_claims SET review_owner=?,review_queue=TRUE WHERE tenant=? AND space_id=?
			AND purpose=? AND claim_id=? AND (review_queue OR (review_state='published' AND expires<=UTC_TIMESTAMP(6)+INTERVAL 7 DAY))`,
			assignee, who.Tenant, space, purpose, id)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return ErrConflict
		}
		return event(ctx, tx, who.Tenant, s.scope(who.Tenant, who.Actor), "team-review-assigned", id, "applied", 0)
	})
}

type TeamEvaluationCase struct {
	ID        string    `json:"id"`
	Group     string    `json:"group"`
	Question  string    `json:"question"`
	AsOf      time.Time `json:"as_of"`
	Expected  string    `json:"claim_id,omitempty"`
	Source    string    `json:"expected_source,omitempty"`
	Forbidden []string  `json:"forbidden_sources,omitempty"`
}

type TeamEvaluationInput struct {
	Purpose string               `json:"purpose"`
	Cases   []TeamEvaluationCase `json:"cases"`
}

type TeamEvaluationStats struct {
	Queries        int `json:"queries"`
	Supported      int `json:"supported"`
	TopFiveCorrect int `json:"top_five_correct"`
	Refused        int `json:"refused"`
	Leaks          int `json:"leaks"`
	ModelQueries   int `json:"model_queries"`
	Fallbacks      int `json:"keyword_fallbacks"`
}

type TeamEvaluationRow struct {
	Case         string         `json:"case_id"`
	Group        string         `json:"group"`
	Mode         string         `json:"mode"`
	Claims       []string       `json:"claim_ids"`
	Correct      bool           `json:"correct"`
	Refused      bool           `json:"refused"`
	Unauthorized int            `json:"unauthorized"`
	Model        *TeamModelNote `json:"model_provenance,omitempty"`
}

type TeamEvaluation struct {
	ID      string                         `json:"evaluation_id"`
	Digest  string                         `json:"frozen_sha256"`
	Results map[string]TeamEvaluationStats `json:"summary"`
	Rows    []TeamEvaluationRow            `json:"rows"`
}

func (s *Service) validateTeamEvaluator(ctx context.Context, who Identity, space, purpose string) error {
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.mu.Unlock()
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := teamAccess(ctx, tx, who, space, purpose, "maintainer")
		return err
	})
}

func (s *Service) EvaluateTeam(ctx context.Context, who Identity, space string, in TeamEvaluationInput, embed *TeamEmbedder) (TeamEvaluation, error) {
	result := TeamEvaluation{ID: randomID(16), Results: map[string]TeamEvaluationStats{}}
	if len(in.Cases) == 0 || len(in.Cases) > 100 || !validID(in.Purpose) {
		return result, ErrInput
	}
	seen := make(map[string]bool)
	for _, entry := range in.Cases {
		if !validID(entry.ID) || seen[entry.ID] || entry.AsOf.IsZero() || entry.AsOf.After(time.Now().Add(time.Second)) ||
			!textOK(entry.Question, 500) || sensitive.MatchString(entry.Question) ||
			!slices.Contains([]string{"supported", "cross-project", "withdrawn"}, entry.Group) ||
			entry.Group == "supported" && len(entry.Expected) != 32 || entry.Source != "" && len(entry.Source) != 64 ||
			entry.Group != "supported" && len(entry.Forbidden) == 0 || len(entry.Forbidden) > 20 {
			return result, fmt.Errorf("%w: cases", ErrInput)
		}
		for _, forbidden := range entry.Forbidden {
			if len(forbidden) != 64 {
				return result, fmt.Errorf("%w: cases.forbidden_sources", ErrInput)
			}
		}
		seen[entry.ID] = true
	}
	if err := s.validateTeamEvaluator(ctx, who, space, in.Purpose); err != nil {
		return result, err
	}
	result.Digest = hash(encode(in))
	for _, mode := range []string{"keywords", "hybrid"} {
		var stats TeamEvaluationStats
		for _, entry := range in.Cases {
			out, err := s.SearchTeam(ctx, who, space, TeamQuery{Purpose: in.Purpose, Query: entry.Question, AsOf: entry.AsOf, Mode: mode, Limit: 5}, embed)
			if err != nil {
				return result, err
			}
			row := TeamEvaluationRow{Case: entry.ID, Group: entry.Group, Mode: mode, Model: out.Model, Refused: len(out.Hits) == 0, Claims: []string{}}
			for _, hit := range out.Hits {
				row.Claims = append(row.Claims, hit.ID)
				for _, cite := range hit.Citations {
					if hit.ID == entry.Expected && (entry.Source == "" || cite.Source == entry.Source) {
						row.Correct = true
					}
					if slices.Contains(entry.Forbidden, cite.Source) || hit.Space != space || hit.Purpose != in.Purpose {
						row.Unauthorized++
					}
				}
			}
			stats.Queries++
			stats.Leaks += row.Unauthorized
			if entry.Group == "supported" {
				stats.Supported++
			}
			if row.Correct {
				stats.TopFiveCorrect++
			}
			if row.Refused {
				stats.Refused++
			}
			if row.Model != nil {
				stats.ModelQueries++
			} else if mode == "hybrid" {
				stats.Fallbacks++
			}
			result.Rows = append(result.Rows, row)
		}
		result.Results[mode] = stats
	}
	if err := s.lock(ctx); err != nil {
		return result, err
	}
	defer s.mu.Unlock()
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := teamAccess(ctx, tx, who, space, in.Purpose, "maintainer"); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO team_evaluations(tenant,space_id,evaluation,actor,frozen_digest,input,report)
			VALUES(?,?,?,?,?,?,?)`, who.Tenant, space, result.ID, who.Actor, result.Digest,
			encode(map[string]any{"purpose": in.Purpose, "case_count": len(in.Cases)}), encode(result))
		return err
	})
	return result, err
}

func (s *Service) TeamEvaluationReceipt(ctx context.Context, who Identity, space, purpose, id string) (TeamEvaluation, error) {
	var result TeamEvaluation
	if err := s.lock(ctx); err != nil {
		return result, err
	}
	defer s.mu.Unlock()
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := teamAccess(ctx, tx, who, space, purpose, "maintainer"); err != nil {
			return err
		}
		var raw []byte
		err := tx.QueryRowContext(ctx, `SELECT report FROM team_evaluations WHERE tenant=? AND space_id=? AND evaluation=?
			AND input->>'$.purpose'=?`, who.Tenant, space, id, purpose).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrMissing
		}
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, &result)
	})
	return result, err
}
