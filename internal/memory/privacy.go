package memory

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type Share struct {
	Token   string    `json:"token"`
	Expires time.Time `json:"expires"`
}

type SharedFact struct {
	Slot    string `json:"slot"`
	Value   string `json:"value"`
	Version uint64 `json:"version"`
}

func (s *Service) CreateShare(ctx context.Context, who Identity, slot, purpose string, minutes int) (Share, error) {
	if who.Role != "owner" {
		return Share{}, ErrDenied
	}
	if !validID(slot) || !validID(purpose) || minutes < 1 || minutes > 1440 {
		return Share{}, ErrInput
	}
	if err := s.lock(ctx); err != nil {
		return Share{}, err
	}
	defer s.mu.Unlock()
	scope := s.scope(who.Tenant, who.Actor)
	result := Share{Token: randomID(32), Expires: time.Now().UTC().Truncate(time.Microsecond).Add(time.Duration(minutes) * time.Minute)}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := authorize(ctx, tx, who); err != nil {
			return err
		}
		label := s.slot(scope, slot)
		var version uint64
		err := tx.QueryRowContext(ctx, `SELECT version FROM facts WHERE scope_hash=? AND slot_hash=? AND purpose=?
			AND replaced IS NULL AND expires>UTC_TIMESTAMP(6)`, scope, label, purpose).Scan(&version)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrMissing
		}
		if err != nil {
			return err
		}
		digest := hash(result.Token)
		_, err = tx.ExecContext(ctx, "INSERT INTO shares SELECT ?,scope_hash,?,?,epoch,?,? FROM subjects WHERE scope_hash=?",
			digest, label, purpose, version, result.Expires, scope)
		if err != nil {
			return err
		}
		return event(ctx, tx, who.Tenant, scope, "share", digest, "applied", 0)
	})
	if err != nil {
		return Share{}, err
	}
	return result, nil
}

func (s *Service) ReadShare(ctx context.Context, token string) (SharedFact, error) {
	if len(token) != 64 {
		return SharedFact{}, ErrMissing
	}
	if err := s.lock(ctx); err != nil {
		return SharedFact{}, err
	}
	defer s.mu.Unlock()
	var fact SharedFact
	err := s.db.QueryRowContext(ctx, `SELECT f.slot,f.value,f.version FROM shares l JOIN subjects u ON u.scope_hash=l.scope_hash
		JOIN facts f ON f.scope_hash=l.scope_hash AND f.slot_hash=l.slot_hash AND f.purpose=l.purpose AND f.version=l.version
		WHERE l.share_hash=? AND l.epoch=u.epoch AND l.expires>UTC_TIMESTAMP(6) AND f.expires>UTC_TIMESTAMP(6)
		AND f.replaced IS NULL AND f.effective<=UTC_TIMESTAMP(6)`, hash(token)).Scan(&fact.Slot, &fact.Value, &fact.Version)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrMissing
	}
	return fact, err
}

type Snapshot struct {
	Subject  string    `json:"subject"`
	Consents []Consent `json:"consents"`
	Facts    []Fact    `json:"facts"`
}

func (s *Service) Export(ctx context.Context, who Identity) (Snapshot, error) {
	if who.Role != "owner" {
		return Snapshot{}, ErrDenied
	}
	if err := s.lock(ctx); err != nil {
		return Snapshot{}, err
	}
	defer s.mu.Unlock()
	scope := s.scope(who.Tenant, who.Actor)
	doc := Snapshot{Subject: who.Actor, Consents: []Consent{}, Facts: []Fact{}}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := authorize(ctx, tx, who); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT app,purpose,can_read,can_write,auto_accept,allow_model,expires
			FROM consent WHERE scope_hash=? AND expires>UTC_TIMESTAMP(6) ORDER BY app,purpose`, scope)
		if err != nil {
			return err
		}
		for rows.Next() {
			var c Consent
			if err = rows.Scan(&c.App, &c.Purpose, &c.Read, &c.Write, &c.Auto, &c.Model, &c.Expires); err != nil {
				rows.Close()
				return err
			}
			doc.Consents = append(doc.Consents, c)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		purposes := []string{}
		rows, err = tx.QueryContext(ctx, "SELECT DISTINCT purpose FROM facts WHERE scope_hash=? AND expires>UTC_TIMESTAMP(6) ORDER BY purpose", scope)
		if err != nil {
			return err
		}
		for rows.Next() {
			var p string
			if err = rows.Scan(&p); err != nil {
				rows.Close()
				return err
			}
			purposes = append(purposes, p)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, p := range purposes {
			found, err := recallRows(ctx, tx, scope, Recall{Purpose: p, AsOf: time.Now().UTC(), Limit: 501})
			if err != nil {
				return err
			}
			if len(found) == 501 {
				return ErrConflict
			}
			doc.Facts = append(doc.Facts, found...)
		}
		return event(ctx, tx, who.Tenant, scope, "export", "", "applied", 0)
	})
	return doc, err
}

type AuditEvent struct {
	ID             uint64    `json:"id"`
	Kind           string    `json:"kind"`
	ObjectRef      string    `json:"object_ref"`
	Outcome        string    `json:"outcome"`
	DurationMicros *int64    `json:"duration_us"`
	At             time.Time `json:"at"`
}

func (s *Service) Audit(ctx context.Context, who Identity, after uint64) ([]AuditEvent, error) {
	if who.Role != "audit" && who.Role != "admin" && who.Role != "owner" {
		return nil, ErrDenied
	}
	if err := s.lock(ctx); err != nil {
		return nil, err
	}
	defer s.mu.Unlock()
	found := []AuditEvent{}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := authorize(ctx, tx, who); err != nil {
			return err
		}
		filter := "tenant=? AND id>?"
		args := []any{who.Tenant, after}
		if who.Role == "owner" {
			filter += " AND scope_hash=?"
			args = append(args, s.scope(who.Tenant, who.Actor))
		}
		rows, err := tx.QueryContext(ctx, "SELECT id,kind,object_ref,outcome,duration_us,recorded FROM events WHERE "+filter+" ORDER BY id LIMIT 100", args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item AuditEvent
			if err = rows.Scan(&item.ID, &item.Kind, &item.ObjectRef, &item.Outcome, &item.DurationMicros, &item.At); err != nil {
				return err
			}
			found = append(found, item)
		}
		return rows.Err()
	})
	return found, err
}

func (s *Service) Sweep(ctx context.Context, who Identity) (map[string]int64, error) {
	if who.Role != "admin" {
		return nil, ErrDenied
	}
	if err := s.lock(ctx); err != nil {
		return nil, err
	}
	defer s.mu.Unlock()
	counters := map[string]int64{}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := authorize(ctx, tx, who); err != nil {
			return err
		}
		statements := []struct{ kind, query string }{
			{"source_conflicts", `DELETE c FROM source_conflicts c JOIN sources x ON x.scope_hash=c.scope_hash AND x.source_hash=c.source_hash
				JOIN subjects u ON u.scope_hash=x.scope_hash WHERE u.tenant=? AND x.expires<=UTC_TIMESTAMP(6)`},
			{"model_jobs", `DELETE j FROM model_jobs j JOIN sources x ON x.scope_hash=j.scope_hash AND x.source_hash=j.source_hash
				JOIN subjects u ON u.scope_hash=x.scope_hash WHERE u.tenant=? AND x.expires<=UTC_TIMESTAMP(6)`},
			{"candidates", `DELETE c FROM candidates c JOIN sources x ON x.scope_hash=c.scope_hash AND x.source_hash=c.source_hash
				JOIN subjects u ON u.scope_hash=x.scope_hash WHERE u.tenant=? AND x.expires<=UTC_TIMESTAMP(6)`},
			{"source_bodies", `UPDATE sources x JOIN subjects u ON u.scope_hash=x.scope_hash SET x.statement=NULL
				WHERE u.tenant=? AND x.expires<=UTC_TIMESTAMP(6) AND x.statement IS NOT NULL`},
			{"evidence_bodies", `UPDATE facts x JOIN subjects u ON u.scope_hash=x.scope_hash
				JOIN sources r ON r.scope_hash=x.scope_hash AND r.source_hash=x.source_hash SET x.evidence=NULL
				WHERE u.tenant=? AND r.expires<=UTC_TIMESTAMP(6) AND x.evidence IS NOT NULL`},
			{"prior_facts", `DELETE x FROM facts x JOIN subjects u ON u.scope_hash=x.scope_hash
				WHERE u.tenant=? AND x.expires<=UTC_TIMESTAMP(6) AND x.replaced IS NOT NULL`},
			{"current_values", `UPDATE facts x JOIN subjects u ON u.scope_hash=x.scope_hash SET x.value='',x.evidence=NULL
				WHERE u.tenant=? AND x.expires<=UTC_TIMESTAMP(6) AND x.replaced IS NULL AND (x.value<>'' OR x.evidence IS NOT NULL)`},
			{"links", `DELETE x FROM shares x JOIN subjects u ON u.scope_hash=x.scope_hash
				WHERE u.tenant=? AND x.expires<=UTC_TIMESTAMP(6)`},
			{"expired_consents", `DELETE x FROM consent x JOIN subjects u ON u.scope_hash=x.scope_hash
				WHERE u.tenant=? AND x.expires<=UTC_TIMESTAMP(6)`},
		}
		for _, job := range statements {
			result, err := tx.ExecContext(ctx, job.query, who.Tenant)
			if err != nil {
				return err
			}
			counters[job.kind], err = result.RowsAffected()
			if err != nil {
				return err
			}
		}
		return event(ctx, tx, who.Tenant, "", "expiration", "", "applied", 0)
	})
	return counters, err
}
