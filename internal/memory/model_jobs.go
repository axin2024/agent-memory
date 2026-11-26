package memory

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

const modelReceiptQuery = `SELECT j.id,j.source_hash,COALESCE(j.candidate,''),COALESCE(c.status,j.status),j.model,
	j.prompt_tokens,j.completion_tokens,j.duration_ms,j.response_sha,j.attempt
	FROM model_jobs j LEFT JOIN candidates c ON c.scope_hash=j.scope_hash AND c.candidate=j.candidate`

func readModelReceipt(row *sql.Row) (ModelReceipt, error) {
	var result ModelReceipt
	err := row.Scan(&result.ID, &result.Source, &result.Candidate, &result.Status, &result.Model,
		&result.PromptTokens, &result.CompletionTokens, &result.DurationMillis, &result.ResponseSHA, &result.Attempt)
	return result, err
}

func permittedModelSource(ctx context.Context, tx *sql.Tx, s *Service, who Identity, subject, purpose, source string) (string, proof, uint64, error) {
	scope, grant, err := s.grants(ctx, tx, who, subject, purpose, true)
	if err != nil {
		return "", proof{}, 0, err
	}
	if !grant.Model || who.Role != "app" {
		return "", proof{}, 0, ErrDenied
	}
	p, err := evidence(ctx, tx, scope, source)
	if errors.Is(err, sql.ErrNoRows) {
		return "", proof{}, 0, ErrMissing
	}
	if err != nil {
		return "", proof{}, 0, err
	}
	if p.App != who.Actor || p.Purpose != purpose || p.Evidence == "" || !p.Expires.After(time.Now().UTC()) {
		return "", proof{}, 0, ErrDenied
	}
	var epoch uint64
	err = tx.QueryRowContext(ctx, "SELECT epoch FROM subjects WHERE scope_hash=?", scope).Scan(&epoch)
	return scope, p, epoch, err
}

func (s *Service) QueueEnrichment(ctx context.Context, who Identity, subject, purpose, source string, model *Model) (ModelReceipt, error) {
	if len(source) != 64 || model == nil {
		return ModelReceipt{}, ErrInput
	}
	if _, err := hex.DecodeString(source); err != nil {
		return ModelReceipt{}, ErrInput
	}
	if err := s.lock(ctx); err != nil {
		return ModelReceipt{}, err
	}
	defer s.mu.Unlock()
	var result ModelReceipt
	err := s.tx(ctx, func(tx *sql.Tx) error {
		scope, p, epoch, err := permittedModelSource(ctx, tx, s, who, subject, purpose, source)
		if err != nil {
			return err
		}
		result, err = readModelReceipt(tx.QueryRowContext(ctx, modelReceiptQuery+
			" WHERE j.scope_hash=? AND j.source_hash=? AND j.model=?", scope, source, model.name))
		if err == nil {
			if result.Status == "failed" || result.Status == "discarded" {
				_, err = tx.ExecContext(ctx, `UPDATE model_jobs SET status='queued',principal_hash=?,epoch=?,leased_until=NULL,
					prompt_tokens=0,completion_tokens=0,duration_ms=0,response_sha='' WHERE id=?`, who.tokenHash, epoch, result.ID)
				result = ModelReceipt{ID: result.ID, Source: source, Model: model.name, Status: "queued", Attempt: result.Attempt}
			}
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		result = ModelReceipt{Source: source, Model: model.name}
		err = tx.QueryRowContext(ctx, "SELECT candidate,status FROM candidates WHERE scope_hash=? AND source_hash=?", scope, source).
			Scan(&result.Candidate, &result.Status)
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		instruction := strings.ToLower(p.Evidence)
		if !(strings.HasPrefix(instruction, "i ") || strings.HasPrefix(instruction, "my ") || strings.HasPrefix(instruction, "我")) ||
			attribution.MatchString(instruction) || sensitive.MatchString(instruction) {
			return ErrInput
		}
		result.ID, result.Status = randomID(16), "queued"
		_, err = tx.ExecContext(ctx, `INSERT INTO model_jobs(id,scope_hash,source_hash,model,principal_hash,epoch)
			VALUES(?,?,?,?,?,?)`, result.ID, scope, source, model.name, who.tokenHash, epoch)
		return err
	})
	return result, err
}

func (s *Service) ExtractionStatus(ctx context.Context, who Identity, id string) (ModelReceipt, error) {
	if len(id) != 32 {
		return ModelReceipt{}, ErrInput
	}
	if who.Role != "owner" && who.Role != "app" {
		return ModelReceipt{}, ErrDenied
	}
	if err := s.lock(ctx); err != nil {
		return ModelReceipt{}, err
	}
	defer s.mu.Unlock()
	var result ModelReceipt
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := authorize(ctx, tx, who); err != nil {
			return err
		}
		var subject, app, purpose string
		err := tx.QueryRowContext(ctx, `SELECT u.subject,x.app,x.purpose FROM model_jobs j
			JOIN subjects u ON u.scope_hash=j.scope_hash JOIN sources x ON x.scope_hash=j.scope_hash AND x.source_hash=j.source_hash
			WHERE j.id=? AND u.tenant=? AND x.expires>UTC_TIMESTAMP(6)`, id, who.Tenant).Scan(&subject, &app, &purpose)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrMissing
		}
		if err != nil {
			return err
		}
		if who.Role == "owner" {
			if who.Actor != subject {
				return ErrMissing
			}
		} else {
			_, grant, err := s.grants(ctx, tx, who, subject, purpose, true)
			if errors.Is(err, ErrDenied) || err == nil && (who.Actor != app || !grant.Model) {
				return ErrMissing
			}
			if err != nil {
				return err
			}
		}
		result, err = readModelReceipt(tx.QueryRowContext(ctx, modelReceiptQuery+" WHERE j.id=?", id))
		return err
	})
	return result, err
}

type runningExtraction struct {
	id, scope, source, subject, purpose string
	epoch                               uint64
	attempt                             int
	who                                 Identity
	statement                           string
}

func (s *Service) claimExtraction(ctx context.Context, id, model string) (runningExtraction, error) {
	if err := s.lock(ctx); err != nil {
		return runningExtraction{}, err
	}
	defer s.mu.Unlock()
	job := runningExtraction{who: Identity{Role: "app"}}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT j.id,j.scope_hash,j.source_hash,u.subject,x.purpose,j.epoch,j.attempt,
			u.tenant,x.app,j.principal_hash FROM model_jobs j JOIN subjects u ON u.scope_hash=j.scope_hash
			JOIN sources x ON x.scope_hash=j.scope_hash AND x.source_hash=j.source_hash
			WHERE j.model=? AND (?='' OR j.id=?) AND (j.status='queued' OR j.status='running' AND j.leased_until<=UTC_TIMESTAMP(6))
			ORDER BY j.recorded,j.id LIMIT 1`, model, id, id).Scan(&job.id, &job.scope, &job.source, &job.subject,
			&job.purpose, &job.epoch, &job.attempt, &job.who.Tenant, &job.who.Actor, &job.who.tokenHash)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		_, p, epoch, err := permittedModelSource(ctx, tx, s, job.who, job.subject, job.purpose, job.source)
		if errors.Is(err, ErrDenied) || errors.Is(err, ErrMissing) || err == nil && epoch != job.epoch {
			_, err = tx.ExecContext(ctx, "UPDATE model_jobs SET status='discarded',leased_until=NULL WHERE id=?", job.id)
			return err
		}
		if err != nil {
			return err
		}
		job.attempt++
		job.statement = p.Evidence
		_, err = tx.ExecContext(ctx, `UPDATE model_jobs SET status='running',attempt=?,leased_until=TIMESTAMPADD(SECOND,20,UTC_TIMESTAMP(6))
			WHERE id=?`, job.attempt, job.id)
		return err
	})
	return job, err
}

func (s *Service) finishExtraction(ctx context.Context, job runningExtraction, value extraction, receipt *ModelReceipt, providerErr error) error {
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.mu.Unlock()
	return s.tx(ctx, func(tx *sql.Tx) error {
		var attempt int
		err := tx.QueryRowContext(ctx, "SELECT attempt FROM model_jobs WHERE id=? AND status='running'", job.id).Scan(&attempt)
		if errors.Is(err, sql.ErrNoRows) || err == nil && attempt != job.attempt {
			receipt.Status = "discarded"
			return nil
		}
		if err != nil {
			return err
		}
		_, _, epoch, err := permittedModelSource(ctx, tx, s, job.who, job.subject, job.purpose, job.source)
		if errors.Is(err, ErrDenied) || errors.Is(err, ErrMissing) || err == nil && epoch != job.epoch {
			*receipt = ModelReceipt{ID: job.id, Source: job.source, Model: receipt.Model, Attempt: job.attempt, Status: "discarded"}
			_, err = tx.ExecContext(ctx, "UPDATE model_jobs SET status='discarded',leased_until=NULL WHERE id=?", job.id)
			return err
		}
		if err != nil {
			return err
		}
		var candidate, excerpt any
		receipt.Status = "no-proposal"
		if providerErr != nil {
			receipt.Status = "failed"
		} else if value.Slot != "" {
			var found string
			err = tx.QueryRowContext(ctx, "SELECT candidate FROM candidates WHERE scope_hash=? AND source_hash=?", job.scope, job.source).Scan(&found)
			if errors.Is(err, sql.ErrNoRows) {
				receipt.Candidate, receipt.Status, err = s.addCandidate(ctx, tx, job.who.Tenant, job.scope, job.source, value.Slot, value.Value, "model", false)
			}
			if errors.Is(err, ErrForgotten) {
				receipt.Status = "discarded"
			} else if err != nil {
				return err
			} else if receipt.Candidate != "" {
				candidate, excerpt = receipt.Candidate, value.Evidence
				if _, err = tx.ExecContext(ctx, "UPDATE sources SET slot=?,slot_hash=? WHERE scope_hash=? AND source_hash=?",
					value.Slot, s.slot(job.scope, value.Slot), job.scope, job.source); err != nil {
					return err
				}
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE model_jobs SET status=?,leased_until=NULL,candidate=?,excerpt=?,prompt_tokens=?,
			completion_tokens=?,duration_ms=?,response_sha=? WHERE id=?`, receipt.Status, candidate, excerpt,
			receipt.PromptTokens, receipt.CompletionTokens, receipt.DurationMillis, receipt.ResponseSHA, job.id); err != nil {
			return err
		}
		return event(ctx, tx, job.who.Tenant, job.scope, "model-extract", job.source, receipt.Status, receipt.DurationMillis*1000)
	})
}

func (s *Service) runExtraction(ctx context.Context, id string, model *Model) (ModelReceipt, error) {
	job, err := s.claimExtraction(ctx, id, model.name)
	if err != nil || job.id == "" {
		return ModelReceipt{}, err
	}
	if job.statement == "" {
		return ModelReceipt{ID: job.id, Source: job.source, Status: "discarded", Model: model.name, Attempt: job.attempt}, nil
	}
	start := time.Now()
	value, receipt, providerErr := model.extract(ctx, job.statement)
	receipt.ID, receipt.Source, receipt.Attempt = job.id, job.source, job.attempt
	receipt.DurationMillis = time.Since(start).Milliseconds()
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 4*time.Second)
	defer cancel()
	if err := s.finishExtraction(finish, job, value, &receipt, providerErr); err != nil {
		return ModelReceipt{}, err
	}
	if receipt.Status == "failed" {
		return receipt, providerErr
	}
	return receipt, nil
}

func (s *Service) Enrich(ctx context.Context, who Identity, subject, purpose, source string, model *Model) (ModelReceipt, error) {
	receipt, err := s.QueueEnrichment(ctx, who, subject, purpose, source, model)
	if err != nil || receipt.Status != "queued" && receipt.Status != "running" {
		return receipt, err
	}
	result, err := s.runExtraction(ctx, receipt.ID, model)
	if err == nil && result.ID == "" {
		return s.ExtractionStatus(ctx, who, receipt.ID)
	}
	return result, err
}

func (s *Service) ProcessExtraction(ctx context.Context, model *Model) (bool, error) {
	if model == nil {
		return false, ErrInput
	}
	result, err := s.runExtraction(ctx, "", model)
	if errors.Is(err, ErrProvider) {
		err = nil
	}
	return result.ID != "", err
}
