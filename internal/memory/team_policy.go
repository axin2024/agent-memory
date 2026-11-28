package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"
	"unicode/utf8"
)

type TeamSpace struct {
	ID        string `json:"space_id"`
	Label     string `json:"label"`
	Owner     string `json:"owner"`
	PolicyRev uint64 `json:"policy_rev"`
	Purpose   string `json:"purpose,omitempty"`
	Role      string `json:"role,omitempty"`
	CanExport bool   `json:"can_export,omitempty"`
}

type TeamPolicy struct {
	Label     string `json:"label"`
	Purpose   string `json:"purpose"`
	PolicyRev uint64 `json:"policy_rev"`
	Active    bool   `json:"active"`
}

type TeamMembership struct {
	Actor     string `json:"actor"`
	Purpose   string `json:"purpose"`
	Role      string `json:"role"`
	CanExport bool   `json:"can_export"`
	Active    bool   `json:"active"`
	PolicyRev uint64 `json:"policy_rev"`
}

func teamSpace(ctx context.Context, tx *sql.Tx, who Identity, space string) (TeamSpace, error) {
	result := TeamSpace{ID: space}
	if !validID(space) {
		return result, ErrInput
	}
	if err := authorize(ctx, tx, who); err != nil {
		return result, err
	}
	err := tx.QueryRowContext(ctx, `SELECT owner,label,policy_rev FROM team_spaces WHERE tenant=? AND space_id=?`,
		who.Tenant, space).Scan(&result.Owner, &result.Label, &result.PolicyRev)
	if errors.Is(err, sql.ErrNoRows) {
		return result, ErrMissing
	}
	return result, err
}

func teamAccess(ctx context.Context, tx *sql.Tx, who Identity, space, purpose string, roles ...string) (TeamSpace, error) {
	result, err := teamSpace(ctx, tx, who, space)
	if err != nil {
		return result, err
	}
	if !validID(purpose) {
		return result, ErrInput
	}
	result.Purpose = purpose
	err = tx.QueryRowContext(ctx, `SELECT m.role,m.can_export FROM team_memberships m JOIN team_purposes p
		ON (p.tenant,p.space_id,p.purpose)=(m.tenant,m.space_id,m.purpose) WHERE m.tenant=? AND m.space_id=?
		AND m.purpose=? AND m.actor=? AND m.active AND p.active`, who.Tenant, space, purpose, who.Actor).
		Scan(&result.Role, &result.CanExport)
	if errors.Is(err, sql.ErrNoRows) {
		return result, ErrMissing
	}
	if err != nil {
		return result, err
	}
	if len(roles) != 0 && !slices.Contains(roles, result.Role) {
		return result, ErrDenied
	}
	return result, nil
}

func (s *Service) SetTeamPolicy(ctx context.Context, who Identity, space string, in TeamPolicy) (TeamSpace, error) {
	var result TeamSpace
	if !validID(space) || !validID(in.Purpose) || !textOK(in.Label, 160) || in.PolicyRev == 0 && !in.Active {
		return result, ErrInput
	}
	if err := s.lock(ctx); err != nil {
		return result, err
	}
	defer s.mu.Unlock()
	err := s.tx(ctx, func(tx *sql.Tx) error {
		current, err := teamSpace(ctx, tx, who, space)
		if errors.Is(err, ErrMissing) && in.PolicyRev == 0 && (who.Role == "owner" || who.Role == "admin") {
			if _, err = tx.ExecContext(ctx, `INSERT INTO team_spaces(tenant,space_id,owner,label,policy_rev) VALUES(?,?,?,?,1)`,
				who.Tenant, space, who.Actor, in.Label); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO team_watermarks VALUES(?,?,0,0)`, who.Tenant, space); err != nil {
				return err
			}
			current = TeamSpace{ID: space, Owner: who.Actor, PolicyRev: 1}
		} else {
			if err != nil {
				return err
			}
			if current.Owner != who.Actor {
				return ErrMissing
			}
			if current.PolicyRev != in.PolicyRev {
				return ErrConflict
			}
			if _, err = tx.ExecContext(ctx, `UPDATE team_spaces SET label=?,policy_rev=policy_rev+1 WHERE tenant=? AND space_id=?`,
				in.Label, who.Tenant, space); err != nil {
				return err
			}
			current.PolicyRev++
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO team_purposes VALUES(?,?,?,?) ON DUPLICATE KEY UPDATE active=?`,
			who.Tenant, space, in.Purpose, in.Active, in.Active); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT IGNORE INTO team_memberships VALUES(?,?,?,?,'maintainer',TRUE,TRUE)`,
			who.Tenant, space, in.Purpose, current.Owner); err != nil {
			return err
		}
		current.Label, current.Purpose, current.Role, current.CanExport = in.Label, in.Purpose, "maintainer", in.Active
		result = current
		return event(ctx, tx, who.Tenant, s.scope(who.Tenant, who.Actor), "team-policy", space, "applied", 0)
	})
	return result, err
}

func (s *Service) SetTeamMembership(ctx context.Context, who Identity, space string, in TeamMembership) (TeamMembership, error) {
	if !validID(in.Actor) || !slices.Contains([]string{"reader", "author", "reviewer", "maintainer"}, in.Role) {
		return TeamMembership{}, ErrInput
	}
	if err := s.lock(ctx); err != nil {
		return TeamMembership{}, err
	}
	defer s.mu.Unlock()
	err := s.tx(ctx, func(tx *sql.Tx) error {
		access, err := teamAccess(ctx, tx, who, space, in.Purpose, "maintainer")
		if err != nil {
			return err
		}
		if access.Owner == in.Actor || (in.Role == "maintainer" && access.Owner != who.Actor) {
			return ErrDenied
		}
		if access.PolicyRev != in.PolicyRev {
			return ErrConflict
		}
		var actorExists bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM principals WHERE tenant=? AND actor=? AND role IN ('owner','admin')
			AND expires>UTC_TIMESTAMP(6))`, who.Tenant, in.Actor).Scan(&actorExists); err != nil {
			return err
		}
		if in.Active && !actorExists {
			return ErrMissing
		}
		if access.Owner != who.Actor {
			var role string
			err := tx.QueryRowContext(ctx, `SELECT role FROM team_memberships WHERE tenant=? AND space_id=? AND purpose=? AND actor=?`,
				who.Tenant, space, in.Purpose, in.Actor).Scan(&role)
			if err == nil && role == "maintainer" {
				return ErrDenied
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO team_memberships VALUES(?,?,?,?,?,?,?)
			ON DUPLICATE KEY UPDATE role=?,can_export=?,active=?`, who.Tenant, space, in.Purpose, in.Actor,
			in.Role, in.CanExport, in.Active, in.Role, in.CanExport, in.Active); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE team_spaces SET policy_rev=policy_rev+1 WHERE tenant=? AND space_id=?`, who.Tenant, space); err != nil {
			return err
		}
		in.PolicyRev++
		return event(ctx, tx, who.Tenant, s.scope(who.Tenant, in.Actor), "team-membership", space, "applied", 0)
	})
	return in, err
}

func (s *Service) TeamSpaces(ctx context.Context, who Identity) ([]TeamSpace, error) {
	result := []TeamSpace{}
	if err := s.lock(ctx); err != nil {
		return result, err
	}
	defer s.mu.Unlock()
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := authorize(ctx, tx, who); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT s.space_id,s.label,s.owner,s.policy_rev,m.purpose,m.role,m.can_export FROM team_spaces s
			JOIN team_memberships m ON (m.tenant,m.space_id)=(s.tenant,s.space_id) JOIN team_purposes p
			ON (p.tenant,p.space_id,p.purpose)=(m.tenant,m.space_id,m.purpose)
			WHERE s.tenant=? AND m.actor=? AND m.active AND p.active ORDER BY s.space_id,m.purpose LIMIT 250`, who.Tenant, who.Actor)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var row TeamSpace
			if err = rows.Scan(&row.ID, &row.Label, &row.Owner, &row.PolicyRev, &row.Purpose, &row.Role, &row.CanExport); err != nil {
				return err
			}
			result = append(result, row)
		}
		return rows.Err()
	})
	return result, err
}

type NominationInput struct {
	Purpose    string    `json:"purpose"`
	Source     string    `json:"source"`
	GrantRev   uint64    `json:"grant_rev"`
	Start      int       `json:"start"`
	End        int       `json:"end"`
	Expires    time.Time `json:"expires_at"`
	AllowModel bool      `json:"allow_model"`
}

type TeamNomination struct {
	NominationInput
	ID      string `json:"nomination_id"`
	Excerpt string `json:"excerpt"`
	Replay  bool   `json:"replayed,omitempty"`
}

func validSpan(text string, start, end int) bool {
	return start >= 0 && end > start && end <= len(text) && utf8.ValidString(text[start:end])
}

func (s *Service) Nominate(ctx context.Context, who Identity, space string, in NominationInput, preview bool) (TeamNomination, error) {
	result := TeamNomination{NominationInput: in}
	if who.Role != "owner" || len(in.Source) != 64 || !in.Expires.After(time.Now()) || in.GrantRev == 0 {
		return result, ErrInput
	}
	if err := s.lock(ctx); err != nil {
		return result, err
	}
	defer s.mu.Unlock()
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := teamAccess(ctx, tx, who, space, in.Purpose, "author", "maintainer"); err != nil {
			return err
		}
		scope := s.scope(who.Tenant, who.Actor)
		var statement, digest string
		var expiry time.Time
		err := tx.QueryRowContext(ctx, `SELECT statement,digest,expires FROM sources WHERE scope_hash=? AND source_hash=?
			AND kind='user' AND statement IS NOT NULL AND app=? AND expires>UTC_TIMESTAMP(6)`, scope, in.Source, who.Actor).
			Scan(&statement, &digest, &expiry)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: source has no transferable owner evidence", ErrInput)
		}
		if err != nil {
			return err
		}
		in.Expires = in.Expires.UTC().Truncate(time.Microsecond)
		if !validSpan(statement, in.Start, in.End) || in.Expires.After(expiry) {
			return fmt.Errorf("%w: evidence span or sharing expiry", ErrInput)
		}
		result = TeamNomination{NominationInput: in, Excerpt: statement[in.Start:in.End]}
		var old TeamNomination
		var revoked sql.NullInt64
		err = tx.QueryRowContext(ctx, `SELECT nomination,start_byte,end_byte,expires,allow_model,revoked_seq FROM team_nominations
			WHERE tenant=? AND space_id=? AND purpose=? AND source_hash=? AND grant_rev=?`, who.Tenant, space, in.Purpose, in.Source, in.GrantRev).
			Scan(&old.ID, &old.Start, &old.End, &old.Expires, &old.AllowModel, &revoked)
		if err == nil {
			if revoked.Valid || old.Start != in.Start || old.End != in.End || !old.Expires.Equal(in.Expires) || old.AllowModel != in.AllowModel {
				return ErrConflict
			}
			result.ID, result.Replay = old.ID, true
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var previous uint64
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(grant_rev),0) FROM team_nominations
			WHERE tenant=? AND space_id=? AND purpose=? AND source_hash=?`, who.Tenant, space, in.Purpose, in.Source).Scan(&previous); err != nil {
			return err
		}
		if in.GrantRev != previous+1 {
			return ErrConflict
		}
		if preview {
			return nil
		}
		result.ID = randomID(16)
		if _, err = tx.ExecContext(ctx, `INSERT INTO team_nominations(tenant,space_id,purpose,nomination,actor,source_scope,source_hash,
			source_digest,grant_rev,start_byte,end_byte,allow_model,expires) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			who.Tenant, space, in.Purpose, result.ID, who.Actor, scope, in.Source, digest, in.GrantRev, in.Start, in.End, in.AllowModel, in.Expires); err != nil {
			return err
		}
		return event(ctx, tx, who.Tenant, scope, "team-nomination", result.ID, "applied", 0)
	})
	return result, err
}
