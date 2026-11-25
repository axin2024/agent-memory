package memory

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"
)

type Candidate struct {
	ID         string    `json:"id"`
	Slot       string    `json:"slot"`
	Value      string    `json:"value"`
	Status     string    `json:"status"`
	Extraction string    `json:"extraction"`
	Source     string    `json:"source"`
	Purpose    string    `json:"purpose"`
	At         time.Time `json:"occurred"`
	Evidence   string    `json:"evidence"`
	Version    uint64    `json:"current_version"`
}

type Fact struct {
	Slot      string     `json:"slot"`
	Value     string     `json:"value"`
	Version   uint64     `json:"version"`
	Source    SourceRef  `json:"source"`
	Purpose   string     `json:"purpose"`
	Effective time.Time  `json:"effective"`
	Replaced  *time.Time `json:"replaced,omitempty"`
	Expires   time.Time  `json:"expires"`
	Authority string     `json:"authority"`
	Reason    string     `json:"reason,omitempty"`
}

type SourceRef struct {
	ID       string `json:"id"`
	App      string `json:"app"`
	Session  string `json:"session"`
	External string `json:"external_id"`
	Evidence string `json:"evidence,omitempty"`
}

type proof struct {
	SourceRef
	Purpose     string
	At, Expires time.Time
}

type current struct {
	Version    uint64
	At         time.Time
	Authority  int
	App, Value string
}

func latest(ctx context.Context, tx *sql.Tx, scope, slot, purpose string) (current, error) {
	var c current
	err := tx.QueryRowContext(ctx, `SELECT version,effective,authority,app,value FROM facts WHERE scope_hash=? AND slot_hash=?
		AND purpose=? ORDER BY version DESC LIMIT 1`, scope, slot, purpose).Scan(&c.Version, &c.At, &c.Authority, &c.App, &c.Value)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return c, err
}

func evidence(ctx context.Context, tx *sql.Tx, scope, source string) (proof, error) {
	var p proof
	p.ID = source
	err := tx.QueryRowContext(ctx, "SELECT app,purpose,session_id,external_id,occurred,expires,COALESCE(statement,'') FROM sources WHERE scope_hash=? AND source_hash=?",
		scope, source).Scan(&p.App, &p.Purpose, &p.Session, &p.External, &p.At, &p.Expires, &p.Evidence)
	return p, err
}

func (s *Service) addCandidate(ctx context.Context, tx *sql.Tx, tenant, scope, source, slot, value, mode string, auto bool) (string, string, error) {
	id := randomID(16)
	label := s.slot(scope, slot)
	p, err := evidence(ctx, tx, scope, source)
	if err != nil {
		return "", "", err
	}
	if err = suppression(ctx, tx, scope, source, label, p.At); err != nil {
		return "", "", err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO candidates VALUES(?,?,?,?,?,?,'pending',?)", scope, id, source, slot, label, value, mode)
	if err != nil {
		return "", "", err
	}
	if !auto {
		return id, "pending", nil
	}
	status, _, err := s.resolveCandidate(ctx, tx, tenant, scope, id, slot, label, value, p, 1)
	return id, status, err
}

func (s *Service) resolveCandidate(ctx context.Context, tx *sql.Tx, tenant, scope, id, slot, label, value string, p proof, authority int) (string, uint64, error) {
	c, err := latest(ctx, tx, scope, label, p.Purpose)
	if err != nil {
		return "", 0, err
	}
	_, factDays, err := retention(ctx, tx, tenant)
	if err != nil {
		return "", 0, err
	}
	status := "applied"
	expires := p.At.AddDate(0, 0, factDays)
	switch {
	case c.Version != 0 && !p.At.After(c.At) || !expires.After(time.Now().UTC()):
		status = "stale"
	case authority == 1 && c.Version != 0 && c.Value != value && (c.Authority > authority || c.App != p.App):
		status = "conflict"
	}
	if status == "applied" {
		if c.Version != 0 {
			if _, err = tx.ExecContext(ctx, "UPDATE facts SET replaced=? WHERE scope_hash=? AND slot_hash=? AND purpose=? AND replaced IS NULL",
				p.At, scope, label, p.Purpose); err != nil {
				return "", 0, err
			}
		}
		c.Version++
		_, err = tx.ExecContext(ctx, `INSERT INTO facts(scope_hash,slot,slot_hash,version,value,source_hash,app,purpose,session_id,external_id,
			evidence,effective,expires,authority) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			scope, slot, label, c.Version, value, p.ID, p.App, p.Purpose, p.Session, p.External, p.Evidence, p.At, expires, authority)
		if err != nil {
			return "", 0, err
		}
	}
	_, err = tx.ExecContext(ctx, "UPDATE candidates SET status=? WHERE scope_hash=? AND candidate=?", status, scope, id)
	return status, c.Version, err
}

func (s *Service) Review(ctx context.Context, who Identity, id string, expected uint64, accept bool) (string, error) {
	if who.Role != "owner" {
		return "", ErrDenied
	}
	if len(id) != 32 {
		return "", ErrInput
	}
	if err := s.lock(ctx); err != nil {
		return "", err
	}
	defer s.mu.Unlock()
	scope := s.scope(who.Tenant, who.Actor)
	var status string
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := authorize(ctx, tx, who); err != nil {
			return err
		}
		var slot, label, value, source string
		err := tx.QueryRowContext(ctx, "SELECT slot,slot_hash,value,source_hash,status FROM candidates WHERE scope_hash=? AND candidate=?",
			scope, id).Scan(&slot, &label, &value, &source, &status)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrMissing
		}
		if err != nil {
			return err
		}
		if status != "pending" && status != "conflict" {
			return ErrConflict
		}
		p, err := evidence(ctx, tx, scope, source)
		if err != nil {
			return err
		}
		if p.Evidence == "" || !p.Expires.After(time.Now().UTC()) {
			return ErrConflict
		}
		c, err := latest(ctx, tx, scope, label, p.Purpose)
		if err != nil {
			return err
		}
		if c.Version != expected {
			return ErrConflict
		}
		if !accept {
			status = "rejected"
			if _, err = tx.ExecContext(ctx, "UPDATE candidates SET status='rejected' WHERE scope_hash=? AND candidate=?", scope, id); err != nil {
				return err
			}
		} else {
			if err = suppression(ctx, tx, scope, source, label, p.At); err != nil {
				return err
			}
			status, _, err = s.resolveCandidate(ctx, tx, who.Tenant, scope, id, slot, label, value, p, 2)
			if err != nil {
				return err
			}
		}
		return event(ctx, tx, who.Tenant, scope, "review", hash(id), status, 0)
	})
	return status, err
}

type Edit struct {
	Slot     string `json:"slot"`
	Value    string `json:"value"`
	Purpose  string `json:"purpose"`
	Key      string `json:"key"`
	Expected uint64 `json:"expected_version"`
}

func (s *Service) Edit(ctx context.Context, who Identity, in Edit) (uint64, error) {
	if who.Role != "owner" {
		return 0, ErrDenied
	}
	if !validID(in.Slot) || !validID(in.Purpose) || !validID(in.Key) || !textOK(in.Value, 240) || sensitive.MatchString(in.Value) {
		return 0, ErrInput
	}
	if err := s.lock(ctx); err != nil {
		return 0, err
	}
	defer s.mu.Unlock()
	scope := s.scope(who.Tenant, who.Actor)
	label := s.slot(scope, in.Slot)
	source := s.source(scope, who.Actor, "owner-edit", in.Key)
	statement := "Owner correction: " + in.Slot + " = " + in.Value
	digest := s.journal.digest("edit", in.Slot, in.Purpose, in.Value)
	now := time.Now().UTC().Truncate(time.Microsecond)
	var version uint64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := authorize(ctx, tx, who); err != nil {
			return err
		}
		if err := suppression(ctx, tx, scope, source, label, now); err != nil {
			return err
		}
		var previous string
		err := tx.QueryRowContext(ctx, "SELECT digest FROM sources WHERE scope_hash=? AND source_hash=?", scope, source).Scan(&previous)
		if err == nil {
			if digest != previous {
				return ErrConflict
			}
			err = tx.QueryRowContext(ctx, "SELECT version FROM facts WHERE scope_hash=? AND source_hash=?", scope, source).Scan(&version)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrConflict
			}
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		c, err := latest(ctx, tx, scope, label, in.Purpose)
		if err != nil {
			return err
		}
		if c.Version != in.Expected {
			return ErrConflict
		}
		raw, _, err := retention(ctx, tx, who.Tenant)
		if err != nil {
			return err
		}
		rawUntil := now.AddDate(0, 0, raw)
		_, err = tx.ExecContext(ctx, "INSERT INTO sources VALUES(?,?,?,?,?,?,?,?,?,'user',?,?,?)",
			scope, source, digest, who.Actor, in.Purpose, "owner-edit", in.Key, now, rawUntil, statement, in.Slot, label)
		if err != nil {
			return err
		}
		id, status, err := s.addCandidate(ctx, tx, who.Tenant, scope, source, in.Slot, in.Value, "manual", false)
		if err != nil {
			return err
		}
		p, err := evidence(ctx, tx, scope, source)
		if err != nil {
			return err
		}
		status, version, err = s.resolveCandidate(ctx, tx, who.Tenant, scope, id, in.Slot, label, in.Value, p, 3)
		if err != nil {
			return err
		}
		if status != "applied" {
			return ErrConflict
		}
		return event(ctx, tx, who.Tenant, scope, "owner-edit", source, status, 0)
	})
	return version, err
}

func (s *Service) Pending(ctx context.Context, who Identity) ([]Candidate, error) {
	if who.Role != "owner" {
		return nil, ErrDenied
	}
	if err := s.lock(ctx); err != nil {
		return nil, err
	}
	defer s.mu.Unlock()
	found := []Candidate{}
	scope := s.scope(who.Tenant, who.Actor)
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := authorize(ctx, tx, who); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT c.candidate,c.slot,c.value,c.status,c.extraction,c.source_hash,s.purpose,s.occurred,s.statement,
			COALESCE((SELECT MAX(f.version) FROM facts f WHERE f.scope_hash=c.scope_hash AND f.slot_hash=c.slot_hash AND f.purpose=s.purpose),0)
			FROM candidates c JOIN sources s ON s.scope_hash=c.scope_hash AND s.source_hash=c.source_hash
			WHERE c.scope_hash=? AND s.statement IS NOT NULL AND s.expires>UTC_TIMESTAMP(6) AND c.status IN ('pending','conflict')
			ORDER BY s.occurred DESC,c.candidate LIMIT 200`, scope)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c Candidate
			if err = rows.Scan(&c.ID, &c.Slot, &c.Value, &c.Status, &c.Extraction, &c.Source, &c.Purpose, &c.At, &c.Evidence, &c.Version); err != nil {
				return err
			}
			found = append(found, c)
		}
		return rows.Err()
	})
	return found, err
}

type Recall struct {
	Subject string    `json:"subject"`
	Purpose string    `json:"purpose"`
	Slot    string    `json:"slot,omitempty"`
	Query   string    `json:"query,omitempty"`
	AsOf    time.Time `json:"as_of,omitempty"`
	Limit   int       `json:"limit,omitempty"`
}

type ranked struct {
	Fact
	score int
}

func recallRows(ctx context.Context, tx *sql.Tx, scope string, in Recall) ([]Fact, error) {
	rows, err := tx.QueryContext(ctx, `SELECT f.slot,f.value,f.version,f.source_hash,f.app,f.purpose,f.session_id,f.external_id,
		CASE WHEN x.expires>UTC_TIMESTAMP(6) THEN COALESCE(f.evidence,'') ELSE '' END,f.effective,f.replaced,f.expires,f.authority
		FROM facts f LEFT JOIN sources x ON f.scope_hash=x.scope_hash AND f.source_hash=x.source_hash
		WHERE f.scope_hash=? AND f.purpose=? AND (?='' OR f.slot=?)
		AND f.effective<=? AND (f.replaced IS NULL OR f.replaced>?) AND f.expires>UTC_TIMESTAMP(6)
		ORDER BY f.effective DESC,f.slot LIMIT 501`, scope, in.Purpose, in.Slot, in.Slot, in.AsOf, in.AsOf)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ranks := []ranked{}
	for rows.Next() {
		var f Fact
		var authority int
		if err = rows.Scan(&f.Slot, &f.Value, &f.Version, &f.Source.ID, &f.Source.App, &f.Purpose, &f.Source.Session, &f.Source.External,
			&f.Source.Evidence, &f.Effective, &f.Replaced, &f.Expires, &authority); err != nil {
			return nil, err
		}
		f.Authority = map[int]string{1: "explicit-rule", 2: "review", 3: "owner-edit"}[authority]
		f.Reason = "authorized scope, purpose and effective interval"
		score := 0
		if in.Query != "" {
			slotTokens := strings.Fields(normalize(f.Slot))
			valueTokens := strings.Fields(normalize(f.Value))
			for _, token := range strings.Fields(normalize(in.Query)) {
				if slices.Contains(slotTokens, token) {
					score += 3
				}
				if slices.Contains(valueTokens, token) {
					score++
				}
			}
			if score == 0 {
				continue
			}
			f.Reason += "; matched query terms"
		}
		ranks = append(ranks, ranked{f, score})
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	slices.SortStableFunc(ranks, func(a, b ranked) int { return b.score - a.score })
	result := []Fact{}
	for _, rank := range ranks[:min(in.Limit, len(ranks))] {
		result = append(result, rank.Fact)
	}
	return result, nil
}

func (s *Service) Recall(ctx context.Context, who Identity, in Recall) ([]Fact, error) {
	if in.Limit == 0 {
		in.Limit = 20
	}
	now := time.Now().UTC()
	if in.AsOf.IsZero() {
		in.AsOf = now
	}
	if in.Limit < 1 || in.Limit > 100 || in.AsOf.After(now) || in.Slot != "" && !validID(in.Slot) ||
		in.Query != "" && (!textOK(in.Query, 200) || normalize(in.Query) == "") {
		return nil, ErrInput
	}
	if err := s.lock(ctx); err != nil {
		return nil, err
	}
	defer s.mu.Unlock()
	var found []Fact
	err := s.tx(ctx, func(tx *sql.Tx) error {
		start := time.Now()
		scope, _, err := s.grants(ctx, tx, who, in.Subject, in.Purpose, false)
		if err != nil {
			return err
		}
		found, err = recallRows(ctx, tx, scope, in)
		if err != nil {
			return err
		}
		outcome := "miss"
		if len(found) > 0 {
			outcome = "hit"
		}
		return event(ctx, tx, who.Tenant, scope, "recall", s.journal.digest("query", in.Purpose, in.Slot), "authorized-"+outcome, time.Since(start).Microseconds())
	})
	return found, err
}
