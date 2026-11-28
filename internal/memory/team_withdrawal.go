package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type TeamWithdrawalItem struct {
	Space   string `json:"space_id"`
	Claim   string `json:"claim_id"`
	Indexed uint64 `json:"indexed_seq"`
	Reproof bool   `json:"needs_new_evidence"`
}

type TeamWithdrawal struct {
	Receipt
	Affected      []TeamWithdrawalItem `json:"affected_claims"`
	AffectedCount int                  `json:"affected_count"`
	Processed     bool                 `json:"index_caught_up"`
}

func withdrawTeamDependencies(ctx context.Context, tx *sql.Tx, e entry) error {
	filter := "n.source_scope=? AND n.revoked_seq IS NULL"
	args := []any{e.Scope}
	switch e.Kind {
	case "withdraw-team-nomination":
		filter += " AND n.nomination=?"
		args = append(args, e.Selector)
	case "forget-slot":
		filter += ` AND EXISTS(SELECT 1 FROM sources src WHERE (src.scope_hash,src.source_hash)=(n.source_scope,n.source_hash)
			AND src.slot_hash=? AND src.occurred<=?)`
		args = append(args, e.Selector, e.At)
	case "forget-subject":
		filter += ` AND EXISTS(SELECT 1 FROM sources src WHERE (src.scope_hash,src.source_hash)=(n.source_scope,n.source_hash)
			AND src.occurred<=?)`
		args = append(args, e.At)
	default:
		return ErrRecovery
	}
	if _, err := tx.ExecContext(ctx, `INSERT IGNORE INTO team_withdrawals(tenant,space_id,seq,claim_id)
		SELECT x.tenant,x.space_id,?,x.claim_id FROM team_claim_evidence x JOIN team_nominations n
		ON (n.tenant,n.space_id,n.nomination)=(x.tenant,x.space_id,x.nomination) WHERE `+filter,
		append([]any{e.Seq}, args...)...); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE team_nominations n SET n.revoked_seq=? WHERE `+filter,
		append([]any{e.Seq}, args...)...); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE team_watermarks w JOIN team_nominations n ON (n.tenant,n.space_id)=(w.tenant,w.space_id)
		SET w.withdrawal_seq=GREATEST(w.withdrawal_seq,?) WHERE n.source_scope=? AND n.revoked_seq=?`, e.Seq, e.Scope, e.Seq); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE team_claims c JOIN team_withdrawals w ON (w.tenant,w.space_id,w.claim_id)=(c.tenant,c.space_id,c.claim_id)
		SET c.content=NULL,c.review_state='withdrawn',c.withdrawn_seq=COALESCE(c.withdrawn_seq,w.seq),c.review_reason='source-withdrawn',
		c.applicability='',c.review_queue=EXISTS(SELECT 1 FROM team_claim_evidence x JOIN team_nominations n
			ON (n.tenant,n.space_id,n.nomination)=(x.tenant,x.space_id,x.nomination)
			WHERE (x.tenant,x.space_id,x.claim_id)=(c.tenant,c.space_id,c.claim_id) AND n.revoked_seq IS NULL AND n.expires>UTC_TIMESTAMP(6))
		WHERE w.seq=?`, e.Seq); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE v FROM team_votes v JOIN team_withdrawals w ON
		(w.tenant,w.space_id,w.claim_id)=(v.tenant,v.space_id,v.claim_id) WHERE w.seq=?`, e.Seq)
	return err
}

func (s *Service) WithdrawTeamNomination(ctx context.Context, who Identity, id, key string) (TeamWithdrawal, error) {
	receipt, err := s.journalAction(ctx, who, id, "withdraw-team-nomination", "", "", key)
	if err != nil {
		return TeamWithdrawal{}, err
	}
	return s.TeamWithdrawalStatus(ctx, who, receipt.ID)
}

func (s *Service) TeamWithdrawalStatus(ctx context.Context, who Identity, operation string) (TeamWithdrawal, error) {
	result := TeamWithdrawal{Processed: true, Affected: []TeamWithdrawalItem{}}
	if who.Role != "owner" {
		return result, ErrDenied
	}
	if err := s.lock(ctx); err != nil {
		return result, err
	}
	defer s.mu.Unlock()
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := authorize(ctx, tx, who); err != nil {
			return err
		}
		entry, ok := s.journal.byOperation[operation]
		if !ok || entry.Scope != s.scope(who.Tenant, who.Actor) ||
			(entry.Kind != "withdraw-team-nomination" && entry.Kind != "forget-slot" && entry.Kind != "forget-subject") {
			return ErrMissing
		}
		result.Receipt = Receipt{ID: entry.Operation, At: entry.At, Watermark: entry.Seq}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MIN(w.indexed_seq>=r.seq), TRUE)
			FROM team_withdrawals r JOIN team_watermarks w ON (w.tenant,w.space_id)=(r.tenant,r.space_id)
			WHERE r.tenant=? AND r.seq=?`, who.Tenant, entry.Seq).Scan(&result.AffectedCount, &result.Processed); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT c.space_id,c.claim_id,w.indexed_seq,c.review_queue FROM team_withdrawals r
			JOIN team_watermarks w ON (w.tenant,w.space_id)=(r.tenant,r.space_id)
			JOIN team_claims c ON (c.tenant,c.space_id,c.claim_id)=(r.tenant,r.space_id,r.claim_id)
			WHERE r.tenant=? AND r.seq=? ORDER BY c.space_id,c.claim_id LIMIT 500`, who.Tenant, entry.Seq)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item TeamWithdrawalItem
			if err = rows.Scan(&item.Space, &item.Claim, &item.Indexed, &item.Reproof); err != nil {
				return err
			}
			result.Affected = append(result.Affected, item)
		}
		return rows.Err()
	})
	return result, err
}

type TeamWithdrawalPreview struct {
	Space         string `json:"space_id"`
	Nomination    string `json:"nomination_id"`
	AffectedCount int    `json:"affected_count"`
}

func (s *Service) PreviewTeamWithdrawal(ctx context.Context, who Identity, id string) (TeamWithdrawalPreview, error) {
	result := TeamWithdrawalPreview{Nomination: id}
	if who.Role != "owner" || len(id) != 32 {
		return result, ErrInput
	}
	if err := s.lock(ctx); err != nil {
		return result, err
	}
	defer s.mu.Unlock()
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := authorize(ctx, tx, who); err != nil {
			return err
		}
		err := tx.QueryRowContext(ctx, `SELECT n.space_id,(SELECT COUNT(DISTINCT e.claim_id) FROM team_claim_evidence e
			WHERE (e.tenant,e.space_id,e.nomination)=(n.tenant,n.space_id,n.nomination)) FROM team_nominations n
			WHERE n.tenant=? AND n.nomination=? AND n.source_scope=?`, who.Tenant, id, s.scope(who.Tenant, who.Actor)).
			Scan(&result.Space, &result.AffectedCount)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: nomination", ErrMissing)
		}
		return err
	})
	return result, err
}
