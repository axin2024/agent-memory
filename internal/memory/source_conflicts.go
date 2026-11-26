package memory

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type SourceConflictError struct {
	ID       uint64
	Replayed bool
}

func (e *SourceConflictError) Error() string { return ErrConflict.Error() }
func (e *SourceConflictError) Unwrap() error { return ErrConflict }

type SourceConflict struct {
	ID       uint64    `json:"id,string"`
	Source   string    `json:"source"`
	App      string    `json:"app"`
	Purpose  string    `json:"purpose"`
	Session  string    `json:"session"`
	External string    `json:"external_id"`
	Recorded time.Time `json:"recorded"`
}

type SourceConflictPage struct {
	Items     []SourceConflict `json:"items"`
	NextAfter uint64           `json:"next_after,string,omitempty"`
}

func queueSourceConflict(ctx context.Context, tx *sql.Tx, scope, source, digest, slot string, at time.Time) (uint64, bool, error) {
	var id uint64
	err := tx.QueryRowContext(ctx, `SELECT id FROM source_conflicts
		WHERE scope_hash=? AND source_hash=? AND incoming_digest=?`, scope, source, digest).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO source_conflicts(scope_hash,source_hash,incoming_digest,slot_hash,occurred)
		VALUES(?,?,?,?,?)`, scope, source, digest, slot, at)
	if err != nil {
		return 0, false, err
	}
	inserted, err := result.LastInsertId()
	return uint64(inserted), false, err
}

func (s *Service) SourceConflicts(ctx context.Context, who Identity, after uint64) (SourceConflictPage, error) {
	if who.Role != "owner" {
		return SourceConflictPage{}, ErrDenied
	}
	if err := s.lock(ctx); err != nil {
		return SourceConflictPage{}, err
	}
	defer s.mu.Unlock()
	page := SourceConflictPage{Items: []SourceConflict{}}
	const size = 50
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := authorize(ctx, tx, who); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT c.id,c.source_hash,x.app,x.purpose,x.session_id,x.external_id,c.recorded
			FROM source_conflicts c JOIN sources x ON x.scope_hash=c.scope_hash AND x.source_hash=c.source_hash
			WHERE c.scope_hash=? AND c.dismissed IS NULL AND c.id>? AND x.expires>UTC_TIMESTAMP(6)
			ORDER BY c.id LIMIT ?`, s.scope(who.Tenant, who.Actor), after, size+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item SourceConflict
			if err := rows.Scan(&item.ID, &item.Source, &item.App, &item.Purpose, &item.Session, &item.External, &item.Recorded); err != nil {
				return err
			}
			if len(page.Items) == size {
				page.NextAfter = page.Items[size-1].ID
				break
			}
			page.Items = append(page.Items, item)
		}
		return rows.Err()
	})
	return page, err
}

func (s *Service) DismissSourceConflict(ctx context.Context, who Identity, id uint64) error {
	if who.Role != "owner" {
		return ErrDenied
	}
	if id == 0 {
		return ErrInput
	}
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.mu.Unlock()
	scope := s.scope(who.Tenant, who.Actor)
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := authorize(ctx, tx, who); err != nil {
			return err
		}
		var source string
		var dismissed sql.NullTime
		err := tx.QueryRowContext(ctx, `SELECT c.source_hash,c.dismissed FROM source_conflicts c
			JOIN sources x ON x.scope_hash=c.scope_hash AND x.source_hash=c.source_hash
			WHERE c.scope_hash=? AND c.id=? AND x.expires>UTC_TIMESTAMP(6)`, scope, id).Scan(&source, &dismissed)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrMissing
		}
		if err != nil || dismissed.Valid {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE source_conflicts SET dismissed=UTC_TIMESTAMP(6) WHERE scope_hash=? AND id=?", scope, id); err != nil {
			return err
		}
		return event(ctx, tx, who.Tenant, scope, "source-conflict", source, "dismissed", 0)
	})
}
