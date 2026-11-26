package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type Consent struct {
	App     string    `json:"app"`
	Purpose string    `json:"purpose"`
	Read    bool      `json:"read"`
	Write   bool      `json:"write"`
	Auto    bool      `json:"auto_accept"`
	Model   bool      `json:"allow_model"`
	Expires time.Time `json:"expires"`
}

type Source struct {
	Subject   string    `json:"subject"`
	Purpose   string    `json:"purpose"`
	Session   string    `json:"session"`
	External  string    `json:"external_id"`
	Occurred  time.Time `json:"occurred"`
	Kind      string    `json:"kind"`
	Statement string    `json:"statement"`
}

type Submission struct {
	Source     string `json:"source"`
	Candidate  string `json:"candidate,omitempty"`
	Status     string `json:"status"`
	Replayed   bool   `json:"replayed"`
	Confidence string `json:"confidence,omitempty"`
}

var (
	explicit    = regexp.MustCompile(`(?i)^(?:remember my|my long-term preference for) ([a-z0-9_-]+) (?:is|as) ([^\r\n]+)$`)
	explicitZH  = regexp.MustCompile(`^我的长期偏好[：:]([a-z0-9_-]+)[=:：](.+)$`)
	sensitive   = regexp.MustCompile(`(?i)\b(?:password|passcode|secret|api[_ -]?key|credit[_ -]?card|bearer|private[_ -]?key)\b|密码|身份证|银行卡|[A-Z0-9._%+\-]+@[A-Z0-9.\-]+\.[A-Z]{2,}|\d(?:[ .()-]*\d){6,}|[a-z0-9_\-]{34,}`)
	attribution = regexp.MustCompile(`(?i)["“”‘’<>]|\b(said|friend|colleague|quoted|someone|today only|for now|temporary)\b|他说|她说|朋友|暂时|仅今天`)
)

func textOK(value string, limit int) bool {
	if !utf8.ValidString(value) || strings.TrimSpace(value) != value || value == "" || utf8.RuneCountInString(value) > limit {
		return false
	}
	for _, ch := range value {
		if unicode.IsControl(ch) {
			return false
		}
	}
	return true
}

func claim(statement string) (string, string) {
	if attribution.MatchString(statement) {
		return "", ""
	}
	matches := explicit.FindStringSubmatch(statement)
	if matches == nil {
		matches = explicitZH.FindStringSubmatch(statement)
	}
	if matches == nil {
		return "", ""
	}
	slot := strings.ToLower(matches[1])
	value := strings.TrimSpace(strings.TrimSuffix(matches[2], "."))
	if !validID(slot) || !textOK(value, 240) || sensitive.MatchString(value) {
		return "", ""
	}
	return slot, value
}

func event(ctx context.Context, tx *sql.Tx, tenant, scope, kind, object, outcome string, micros int64) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO events(tenant,scope_hash,kind,object_ref,outcome,duration_us) VALUES(?,?,?,?,?,?)",
		tenant, scope, kind, object, outcome, micros)
	return err
}

func (s *Service) SetConsent(ctx context.Context, who Identity, c Consent) error {
	if who.Role != "owner" {
		return ErrDenied
	}
	now := time.Now()
	if !validID(c.App) || !validID(c.Purpose) || (!c.Read && !c.Write) || ((c.Auto || c.Model) && !c.Write) ||
		!c.Expires.After(now) || c.Expires.After(now.Add(90*24*time.Hour)) {
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
		var prev Consent
		err := tx.QueryRowContext(ctx, "SELECT can_read,can_write,auto_accept,allow_model,expires FROM consent WHERE scope_hash=? AND app=? AND purpose=?",
			scope, c.App, c.Purpose).Scan(&prev.Read, &prev.Write, &prev.Auto, &prev.Model, &prev.Expires)
		if err == nil {
			if prev.Read == c.Read && prev.Write == c.Write && prev.Auto == c.Auto && prev.Model == c.Model && prev.Expires.Equal(c.Expires.UTC().Truncate(time.Microsecond)) {
				return nil
			}
			return ErrConflict
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		target := s.grant(scope, c.App, c.Purpose)
		_, err = tx.ExecContext(ctx, "INSERT INTO consent VALUES(?,?,?,?,?,?,?,?,?)", scope, c.App, c.Purpose, target, c.Read, c.Write, c.Auto, c.Model, c.Expires.UTC())
		if err != nil {
			return err
		}
		return event(ctx, tx, who.Tenant, scope, "consent", target, "applied", 0)
	})
}

func (s *Service) grants(ctx context.Context, tx *sql.Tx, who Identity, subject, purpose string, write bool) (string, Consent, error) {
	var c Consent
	if err := authorize(ctx, tx, who); err != nil {
		return "", c, err
	}
	if !validID(subject) || !validID(purpose) {
		return "", c, ErrInput
	}
	scope := s.scope(who.Tenant, subject)
	if who.Role == "owner" && who.Actor == subject {
		return scope, Consent{Read: true, Write: true}, nil
	}
	if who.Role != "app" {
		return "", c, ErrDenied
	}
	err := tx.QueryRowContext(ctx, "SELECT can_read,can_write,auto_accept,allow_model,expires FROM consent "+
		"WHERE scope_hash=? AND app=? AND purpose=? AND expires>UTC_TIMESTAMP(6)", scope, who.Actor, purpose).
		Scan(&c.Read, &c.Write, &c.Auto, &c.Model, &c.Expires)
	if errors.Is(err, sql.ErrNoRows) || err == nil && ((write && !c.Write) || (!write && !c.Read)) {
		return "", c, ErrDenied
	}
	return scope, c, err
}

func retention(ctx context.Context, tx *sql.Tx, tenant string) (int, int, error) {
	var raw, facts int
	err := tx.QueryRowContext(ctx, "SELECT source_days,fact_days FROM policy WHERE tenant=?", tenant).Scan(&raw, &facts)
	if errors.Is(err, sql.ErrNoRows) {
		return 14, 180, nil
	}
	return raw, facts, err
}

func (s *Service) SetPolicy(ctx context.Context, who Identity, raw, facts int) error {
	if who.Role != "admin" {
		return ErrDenied
	}
	if raw < 1 || raw > 90 || facts < raw || facts > 730 {
		return ErrInput
	}
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.mu.Unlock()
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := authorize(ctx, tx, who); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO policy VALUES(?,?,?) ON DUPLICATE KEY UPDATE source_days=?,fact_days=?", who.Tenant, raw, facts, raw, facts); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sources x JOIN subjects u ON u.scope_hash=x.scope_hash
			SET x.expires=LEAST(x.expires,TIMESTAMPADD(DAY,?,x.occurred)) WHERE u.tenant=?`, raw, who.Tenant); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE facts x JOIN subjects u ON u.scope_hash=x.scope_hash
			SET x.expires=LEAST(x.expires,TIMESTAMPADD(DAY,?,x.effective)) WHERE u.tenant=?`, facts, who.Tenant); err != nil {
			return err
		}
		return event(ctx, tx, who.Tenant, "", "retention", "", "applied", 0)
	})
}

func suppression(ctx context.Context, tx *sql.Tx, scope, source, slotHash string, at time.Time) error {
	var count int
	err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM forgotten_sources WHERE scope_hash=? AND source_hash=?", scope, source).Scan(&count)
	if err != nil {
		return err
	}
	if count > 0 {
		return ErrForgotten
	}
	err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM forget_cuts WHERE scope_hash=? AND selector IN (?,?) AND cutoff>=?",
		scope, strings.Repeat("0", 64), slotHash, at).Scan(&count)
	if err != nil {
		return err
	}
	if count > 0 {
		return ErrForgotten
	}
	return nil
}

func (s *Service) Ingest(ctx context.Context, who Identity, source Source) (Submission, error) {
	begun := time.Now()
	now := begun.UTC()
	at := source.Occurred.UTC().Truncate(time.Microsecond)
	if !validID(source.Session) || !validID(source.External) || at.IsZero() || at.After(now) ||
		!textOK(source.Statement, 750) || (source.Kind != "user" && source.Kind != "quotation" && source.Kind != "transient") {
		return Submission{}, ErrInput
	}
	if err := s.lock(ctx); err != nil {
		return Submission{}, err
	}
	defer s.mu.Unlock()
	originalKind := source.Kind
	slot, value := "", ""
	if sensitive.MatchString(source.Statement) {
		source.Kind = "sensitive"
	}
	if source.Kind == "user" && attribution.MatchString(source.Statement) {
		source.Kind = "quotation"
	}
	if source.Kind == "user" {
		slot, value = claim(source.Statement)
	}
	var state Submission
	var conflict *SourceConflictError
	err := s.tx(ctx, func(tx *sql.Tx) error {
		scope, c, err := s.grants(ctx, tx, who, source.Subject, source.Purpose, true)
		if err != nil {
			return err
		}
		sourceHash := s.source(scope, who.Actor, source.Session, source.External)
		state.Source = sourceHash
		label := s.slot(scope, slot)
		if err = suppression(ctx, tx, scope, sourceHash, label, at); err != nil {
			return err
		}
		digest := s.journal.digest("content", sourceHash, source.Purpose, at.Format(time.RFC3339Nano), originalKind, source.Statement)
		var old string
		var expires time.Time
		err = tx.QueryRowContext(ctx, "SELECT digest,kind,expires FROM sources WHERE scope_hash=? AND source_hash=?", scope, sourceHash).
			Scan(&old, &state.Status, &expires)
		switch {
		case err == nil && old != digest:
			state.Status = "conflict"
			conflict = &SourceConflictError{}
			if !expires.After(now) {
				return nil
			}
			conflict.ID, conflict.Replayed, err = queueSourceConflict(ctx, tx, scope, sourceHash, digest, label, at)
			if err != nil || conflict.Replayed {
				return err
			}
			return event(ctx, tx, who.Tenant, scope, "source-conflict", sourceHash, "queued", time.Since(begun).Microseconds())
		case err == nil:
			state.Replayed = true
			var mode string
			err = tx.QueryRowContext(ctx, "SELECT candidate,status,extraction FROM candidates WHERE scope_hash=? AND source_hash=?", scope, sourceHash).
				Scan(&state.Candidate, &state.Status, &mode)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			state.Confidence = confidence(mode)
			return err
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		raw, _, err := retention(ctx, tx, who.Tenant)
		if err != nil {
			return err
		}
		expires = at.AddDate(0, 0, raw)
		var statement, storedSlot, storedHash any
		state.Status = source.Kind
		if source.Kind == "user" && expires.After(now) {
			statement = source.Statement
		}
		if slot != "" {
			storedSlot, storedHash = slot, label
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO sources(scope_hash,source_hash,digest,app,purpose,session_id,external_id,occurred,expires,kind,statement,slot,slot_hash)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, scope, sourceHash, digest, who.Actor, source.Purpose, source.Session, source.External, at, expires, source.Kind, statement, storedSlot, storedHash)
		if err != nil {
			return err
		}
		if slot != "" && statement != nil {
			state.Candidate, state.Status, err = s.addCandidate(ctx, tx, who.Tenant, scope, sourceHash, slot, value, "rule", c.Auto)
			if err != nil {
				return err
			}
			state.Confidence = confidence("rule")
		}
		return event(ctx, tx, who.Tenant, scope, "source", sourceHash, state.Status, time.Since(begun).Microseconds())
	})
	if err == nil && conflict != nil {
		err = conflict
	}
	return state, err
}

func normalize(value string) string {
	tokens := strings.FieldsFunc(strings.ToLower(value), func(ch rune) bool { return !unicode.IsLetter(ch) && !unicode.IsNumber(ch) })
	return strings.Join(tokens, " ")
}

func encode(obj any) string {
	data, _ := json.Marshal(obj)
	return string(data)
}
