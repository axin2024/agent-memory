package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

type EvidenceSpan struct {
	Nomination string `json:"nomination_id"`
	Sentence   int    `json:"sentence"`
	Start      int    `json:"start"`
	End        int    `json:"end"`
}

type TeamCitation struct {
	EvidenceSpan
	Source   string    `json:"source"`
	GrantRev uint64    `json:"grant_rev"`
	Expires  time.Time `json:"expires_at"`
	Excerpt  string    `json:"excerpt"`
}

type TeamClaimInput struct {
	Purpose        string         `json:"purpose"`
	Topic          string         `json:"topic"`
	Applicability  string         `json:"applicability"`
	ValidFrom      time.Time      `json:"valid_from"`
	Supersedes     string         `json:"supersedes,omitempty"`
	NominationIDs  []string       `json:"nomination_ids"`
	GrantRevisions []uint64       `json:"grant_revisions"`
	Sentences      []string       `json:"sentences"`
	EvidenceSpans  []EvidenceSpan `json:"evidence_spans"`
	UseModel       bool           `json:"use_model,omitempty"`
}

type TeamClaim struct {
	ID            string         `json:"claim_id"`
	Space         string         `json:"space_id"`
	Purpose       string         `json:"purpose"`
	Rev           uint64         `json:"claim_rev"`
	Author        string         `json:"author"`
	Topic         string         `json:"topic"`
	Applicability string         `json:"applicability"`
	Sentences     []string       `json:"sentences,omitempty"`
	ValidFrom     time.Time      `json:"valid_from"`
	Expires       time.Time      `json:"expires_at"`
	Supersedes    string         `json:"supersedes,omitempty"`
	State         string         `json:"review_state"`
	ReviewVotes   []TeamVote     `json:"review_votes"`
	MissingGrants []string       `json:"missing_grants"`
	Citations     []TeamCitation `json:"citations,omitempty"`
	ReviewReason  string         `json:"review_reason,omitempty"`
	Model         *TeamModelNote `json:"model_provenance,omitempty"`
	Withdrawal    uint64         `json:"withdrawal_seq,omitempty"`
	Replay        bool           `json:"replayed,omitempty"`
}

type TeamVote struct {
	Actor    string `json:"actor,omitempty"`
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
	ClaimRev uint64 `json:"claim_rev,omitempty"`
}

type teamEvidence struct {
	ID, Source, Statement, Actor string
	Rev                          uint64
	Start, End                   int
	Expires                      time.Time
	Model                        bool
}

func readTeamEvidence(ctx context.Context, tx *sql.Tx, tenant, space, purpose, id string) (teamEvidence, error) {
	var e teamEvidence
	e.ID = id
	err := tx.QueryRowContext(ctx, `SELECT n.source_hash,src.statement,n.grant_rev,n.start_byte,n.end_byte,
		LEAST(n.expires,src.expires),n.allow_model,n.actor FROM team_nominations n JOIN sources src
		ON (src.scope_hash,src.source_hash)=(n.source_scope,n.source_hash) AND src.digest=n.source_digest
		WHERE n.tenant=? AND n.space_id=? AND n.purpose=? AND n.nomination=? AND n.revoked_seq IS NULL
		AND n.expires>UTC_TIMESTAMP(6) AND src.expires>UTC_TIMESTAMP(6) AND src.statement IS NOT NULL`, tenant, space, purpose, id).
		Scan(&e.Source, &e.Statement, &e.Rev, &e.Start, &e.End, &e.Expires, &e.Model, &e.Actor)
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrMissing
	}
	return e, err
}

func (s *Service) CreateTeamClaim(ctx context.Context, who Identity, space, key string, in TeamClaimInput, model *Model) (TeamClaim, error) {
	var result TeamClaim
	if !validID(key) || !validID(in.Topic) || !textOK(in.Applicability, 220) || in.ValidFrom.IsZero() ||
		len(in.NominationIDs) == 0 || len(in.NominationIDs) > 12 || len(in.NominationIDs) != len(in.GrantRevisions) ||
		len(in.EvidenceSpans) > 48 || in.UseModel && (len(in.Sentences) != 0 || len(in.EvidenceSpans) != 0) ||
		!in.UseModel && (len(in.Sentences) == 0 || len(in.Sentences) > 12) {
		return result, ErrInput
	}
	if err := s.lock(ctx); err != nil {
		return result, err
	}
	defer s.mu.Unlock()
	in.ValidFrom = in.ValidFrom.UTC().Truncate(time.Microsecond)
	requestDigest := hash(encode(in))
	err := s.tx(ctx, func(tx *sql.Tx) error {
		access, err := teamAccess(ctx, tx, who, space, in.Purpose, "author", "maintainer")
		if err != nil {
			return err
		}
		var prior, stamp string
		err = tx.QueryRowContext(ctx, `SELECT claim_id,input_digest FROM team_claims WHERE tenant=? AND space_id=? AND author=? AND idempotency_key=?`,
			who.Tenant, space, who.Actor, key).Scan(&prior, &stamp)
		if err == nil {
			if stamp != requestDigest {
				return ErrConflict
			}
			result, err = teamClaimView(ctx, tx, who.Tenant, space, in.Purpose, prior, true)
			result.Replay = true
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var expires time.Time
		evidence := make(map[string]teamEvidence, len(in.NominationIDs))
		for idx, id := range in.NominationIDs {
			if len(id) != 32 || evidence[id].ID != "" {
				return fmt.Errorf("%w: nomination_ids", ErrInput)
			}
			entry, err := readTeamEvidence(ctx, tx, who.Tenant, space, in.Purpose, id)
			if errors.Is(err, ErrMissing) || err == nil && (entry.Rev != in.GrantRevisions[idx] || in.UseModel && !entry.Model ||
				access.Role == "author" && entry.Actor != who.Actor) {
				result.MissingGrants = append(result.MissingGrants, id)
				continue
			}
			if err != nil {
				return err
			}
			evidence[id] = entry
			if expires.IsZero() || entry.Expires.Before(expires) {
				expires = entry.Expires
			}
		}
		if len(result.MissingGrants) != 0 {
			return fmt.Errorf("%w: grant_revisions", ErrInput)
		}
		if !in.ValidFrom.Before(expires) {
			return fmt.Errorf("%w: valid_from exceeds the evidence lifetime", ErrInput)
		}
		var rev uint64 = 1
		var predecessor any
		if in.Supersedes != "" {
			var state, oldTopic, oldConditions string
			err := tx.QueryRowContext(ctx, `SELECT claim_rev,review_state,topic,applicability FROM team_claims
				WHERE tenant=? AND space_id=? AND purpose=? AND claim_id=?`, who.Tenant, space, in.Purpose, in.Supersedes).
				Scan(&rev, &state, &oldTopic, &oldConditions)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrMissing
			}
			if err != nil {
				return err
			}
			if oldTopic != in.Topic || !slices.Contains([]string{"published", "disputed", "withdrawn", "superseded"}, state) ||
				state == "disputed" && oldConditions == in.Applicability {
				return ErrConflict
			}
			rev++
			predecessor = in.Supersedes
		}
		var note *TeamModelNote
		reason := ""
		phase := "awaiting_review"
		if in.UseModel {
			if model == nil {
				return ErrProvider
			}
			in.Sentences, in.EvidenceSpans, note, err = model.draftTeamClaim(ctx, in.Topic, in.Applicability, in.NominationIDs, evidence)
			if err != nil {
				return err
			}
		}
		spans, err := checkTeamSpans(in.Sentences, in.EvidenceSpans, evidence)
		if err != nil {
			if !in.UseModel {
				return err
			}
			// An unsupported model sentence is visible only as a review flag.
			phase, reason = "disputed", "model-evidence-missing"
			in.Sentences, spans = nil, nil
		}
		id := randomID(16)
		var provenance any
		if note != nil {
			provenance = encode(note)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO team_claims(tenant,space_id,claim_id,purpose,claim_rev,author,topic,applicability,
			content,valid_from,expires,review_state,supersedes,idempotency_key,input_digest,model_provenance,review_reason,review_queue)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,TRUE)`, who.Tenant, space, id, in.Purpose, rev, who.Actor, in.Topic, in.Applicability,
			encode(in.Sentences), in.ValidFrom, expires, phase, predecessor, key, requestDigest, provenance, reason); err != nil {
			return err
		}
		if phase == "disputed" {
			for _, nomination := range in.NominationIDs {
				e := evidence[nomination]
				spans = append(spans, EvidenceSpan{Nomination: nomination, Start: e.Start, End: e.End})
			}
		}
		for _, span := range spans {
			if _, err = tx.ExecContext(ctx, `INSERT INTO team_claim_evidence VALUES(?,?,?,?,?,?,?)`, who.Tenant, space, id,
				span.Sentence, span.Nomination, span.Start, span.End); err != nil {
				return err
			}
		}
		if err = event(ctx, tx, who.Tenant, s.scope(who.Tenant, who.Actor), "team-claim-proposed", id, phase, 0); err != nil {
			return err
		}
		result, err = teamClaimView(ctx, tx, who.Tenant, space, in.Purpose, id, true)
		return err
	})
	return result, err
}

func checkTeamSpans(sentences []string, spans []EvidenceSpan, evidence map[string]teamEvidence) ([]EvidenceSpan, error) {
	if len(sentences) == 0 || len(sentences) > 12 || len(spans) == 0 || len(spans) > 48 {
		return nil, fmt.Errorf("%w: evidence_spans", ErrInput)
	}
	for _, text := range sentences {
		if !textOK(text, 600) || sensitive.MatchString(text) {
			return nil, fmt.Errorf("%w: sentences", ErrInput)
		}
	}
	cited := map[int]bool{}
	used := map[string]bool{}
	seen := map[EvidenceSpan]bool{}
	for _, span := range spans {
		e, ok := evidence[span.Nomination]
		if !ok || span.Sentence < 0 || span.Sentence >= len(sentences) || span.Start < e.Start || span.End > e.End ||
			!validSpan(e.Statement, span.Start, span.End) || seen[span] {
			return nil, fmt.Errorf("%w: evidence_spans", ErrInput)
		}
		seen[span] = true
		cited[span.Sentence], used[span.Nomination] = true, true
	}
	if len(cited) != len(sentences) || len(used) != len(evidence) {
		return nil, fmt.Errorf("%w: cite every sentence and selected nomination", ErrInput)
	}
	return spans, nil
}

func teamClaimView(ctx context.Context, tx *sql.Tx, tenant, space, purpose, id string, review bool) (TeamClaim, error) {
	result := TeamClaim{ID: id, Space: space, Purpose: purpose, MissingGrants: []string{}, ReviewVotes: []TeamVote{}}
	var content, model sql.NullString
	var withdrawn sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT claim_rev,author,topic,applicability,content,valid_from,expires,review_state,
		COALESCE(supersedes,''),COALESCE(review_reason,''),model_provenance,withdrawn_seq FROM team_claims
		WHERE tenant=? AND space_id=? AND purpose=? AND claim_id=?`, tenant, space, purpose, id).Scan(&result.Rev, &result.Author,
		&result.Topic, &result.Applicability, &content, &result.ValidFrom, &result.Expires, &result.State,
		&result.Supersedes, &result.ReviewReason, &model, &withdrawn)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !review && !slices.Contains([]string{"published", "withdrawn", "superseded"}, result.State) {
		return result, ErrMissing
	}
	if err != nil {
		return result, err
	}
	result.Withdrawal = uint64(withdrawn.Int64)
	now := time.Now()
	if !review && result.State == "published" && result.ValidFrom.After(now) {
		return TeamClaim{}, ErrMissing
	}
	if result.State == "superseded" {
		return result, nil
	}
	if result.State == "withdrawn" || !result.Expires.After(now) {
		result.State, result.Applicability = "withdrawn", ""
		return result, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT e.nomination,e.sentence,e.start_byte,e.end_byte,n.source_hash,n.grant_rev,
		LEAST(n.expires,src.expires),src.statement FROM team_claim_evidence e JOIN team_nominations n
		ON (n.tenant,n.space_id,n.nomination)=(e.tenant,e.space_id,e.nomination)
		LEFT JOIN sources src ON (src.scope_hash,src.source_hash)=(n.source_scope,n.source_hash) AND src.digest=n.source_digest
		AND src.expires>UTC_TIMESTAMP(6) WHERE e.tenant=? AND e.space_id=? AND e.claim_id=?
		AND n.revoked_seq IS NULL AND n.expires>UTC_TIMESTAMP(6) ORDER BY e.sentence,e.nomination,e.start_byte`, tenant, space, id)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var cite TeamCitation
		var statement sql.NullString
		var expiry sql.NullTime
		if err = rows.Scan(&cite.Nomination, &cite.Sentence, &cite.Start, &cite.End, &cite.Source, &cite.GrantRev, &expiry, &statement); err != nil {
			return result, err
		}
		if expiry.Valid && validSpan(statement.String, cite.Start, cite.End) {
			cite.Expires, cite.Excerpt = expiry.Time, statement.String[cite.Start:cite.End]
			result.Citations = append(result.Citations, cite)
		}
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	rows.Close()
	var total int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM team_claim_evidence WHERE tenant=? AND space_id=? AND claim_id=?`, tenant, space, id).
		Scan(&total); err != nil {
		return result, err
	}
	if total == 0 || total != len(result.Citations) {
		result.State, result.Applicability, result.Citations = "withdrawn", "", nil
		return result, nil
	}
	if content.Valid && json.Unmarshal([]byte(content.String), &result.Sentences) != nil {
		return result, ErrRecovery
	}
	if model.Valid && json.Unmarshal([]byte(model.String), &result.Model) != nil {
		return result, ErrRecovery
	}
	if review {
		votes, err := tx.QueryContext(ctx, `SELECT actor,decision,reason FROM team_votes WHERE tenant=? AND space_id=? AND claim_id=? ORDER BY created,actor`,
			tenant, space, id)
		if err != nil {
			return result, err
		}
		defer votes.Close()
		for votes.Next() {
			var vote TeamVote
			if err = votes.Scan(&vote.Actor, &vote.Decision, &vote.Reason); err != nil {
				return result, err
			}
			result.ReviewVotes = append(result.ReviewVotes, vote)
		}
		if err = votes.Err(); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (s *Service) TeamClaim(ctx context.Context, who Identity, space, purpose, id string) (TeamClaim, error) {
	var result TeamClaim
	if err := s.lock(ctx); err != nil {
		return result, err
	}
	defer s.mu.Unlock()
	err := s.tx(ctx, func(tx *sql.Tx) error {
		access, err := teamAccess(ctx, tx, who, space, purpose)
		if err != nil {
			return err
		}
		result, err = teamClaimView(ctx, tx, who.Tenant, space, purpose, id, access.Role != "reader")
		if access.Role == "author" && result.Author != who.Actor && slices.Contains([]string{"awaiting_review", "rejected", "disputed"}, result.State) {
			return ErrMissing
		}
		return err
	})
	if err != nil {
		return TeamClaim{}, err
	}
	return result, nil
}

func (s *Service) VoteTeamClaim(ctx context.Context, who Identity, space, purpose, id string, in TeamVote) (TeamClaim, error) {
	var result TeamClaim
	if !slices.Contains([]string{"approve", "reject", "dispute"}, in.Decision) || !textOK(in.Reason, 300) || in.ClaimRev == 0 {
		return result, ErrInput
	}
	if err := s.lock(ctx); err != nil {
		return result, err
	}
	defer s.mu.Unlock()
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := teamAccess(ctx, tx, who, space, purpose, "reviewer", "maintainer"); err != nil {
			return err
		}
		claim, err := teamClaimView(ctx, tx, who.Tenant, space, purpose, id, true)
		if err != nil {
			return err
		}
		if claim.Author == who.Actor {
			return ErrDenied
		}
		if claim.Rev != in.ClaimRev {
			return ErrConflict
		}
		for _, vote := range claim.ReviewVotes {
			if vote.Actor == who.Actor {
				if vote.Decision == in.Decision && vote.Reason == in.Reason {
					result = claim
					return nil
				}
				return ErrConflict
			}
		}
		if claim.State != "awaiting_review" {
			return ErrConflict
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO team_votes(tenant,space_id,claim_id,actor,decision,reason) VALUES(?,?,?,?,?,?)`,
			who.Tenant, space, id, who.Actor, in.Decision, in.Reason); err != nil {
			return err
		}
		if in.Decision != "approve" {
			phase := map[string]string{"reject": "rejected", "dispute": "disputed"}[in.Decision]
			if _, err = tx.ExecContext(ctx, `UPDATE team_claims SET review_state=?,review_reason=?,review_queue=?
				WHERE tenant=? AND space_id=? AND claim_id=?`, phase, in.Reason, phase == "disputed", who.Tenant, space, id); err != nil {
				return err
			}
		} else {
			var approvals int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM team_votes v JOIN team_memberships m ON
				(m.tenant,m.space_id,m.actor)=(v.tenant,v.space_id,v.actor) WHERE v.tenant=? AND v.space_id=? AND v.claim_id=?
				AND v.decision='approve' AND m.purpose=? AND m.active AND m.role IN ('reviewer','maintainer')`,
				who.Tenant, space, id, purpose).Scan(&approvals); err != nil {
				return err
			}
			if approvals >= 2 {
				if err = publishTeamClaim(ctx, tx, who.Tenant, claim); err != nil {
					return err
				}
			}
		}
		if err := event(ctx, tx, who.Tenant, s.scope(who.Tenant, who.Actor), "team-claim-reviewed", id, in.Decision, 0); err != nil {
			return err
		}
		result, err = teamClaimView(ctx, tx, who.Tenant, space, purpose, id, true)
		return err
	})
	return result, err
}

func publishTeamClaim(ctx context.Context, tx *sql.Tx, tenant string, claim TeamClaim) error {
	var duplicate string
	err := tx.QueryRowContext(ctx, `SELECT claim_id FROM team_claims WHERE tenant=? AND space_id=? AND purpose=? AND topic=?
		AND applicability=? AND review_state='published' AND expires>UTC_TIMESTAMP(6) LIMIT 1`, tenant, claim.Space, claim.Purpose,
		claim.Topic, claim.Applicability).Scan(&duplicate)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if duplicate != "" && duplicate != claim.Supersedes {
		return ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE team_claims SET review_state='published',published=UTC_TIMESTAMP(6),review_queue=FALSE
		WHERE tenant=? AND space_id=? AND claim_id=?`, tenant, claim.Space, claim.ID); err != nil {
		return err
	}
	if claim.Supersedes != "" {
		if _, err = tx.ExecContext(ctx, `UPDATE team_claims SET review_state='superseded',review_reason='replaced-by-reviewed-version',review_queue=FALSE
			WHERE tenant=? AND space_id=? AND claim_id=? AND review_state IN ('published','disputed')`, tenant, claim.Space, claim.Supersedes); err != nil {
			return err
		}
	}
	return indexTeamTerms(ctx, tx, tenant, claim, claim.Topic+" "+claim.Applicability+" "+strings.Join(claim.Sentences, " "))
}
