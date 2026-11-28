package memory

import (
	"bufio"
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const maxJournalBytes = 128 * 1024 * 1024

type entry struct {
	Seq       uint64    `json:"seq"`
	Prev      string    `json:"prev"`
	Kind      string    `json:"kind"`
	Scope     string    `json:"scope"`
	Selector  string    `json:"selector"`
	Operation string    `json:"operation"`
	At        time.Time `json:"at"`
	Sources   []string  `json:"sources,omitempty"`
	MAC       string    `json:"mac"`
}

type Journal struct {
	file        *os.File
	key         []byte
	identity    string
	entries     []entry
	byOperation map[string]entry
}

func InitJournal(path string, key []byte) error {
	if len(key) != 32 {
		return ErrInput
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	j := &Journal{key: key}
	identity := randomID(16)
	_, err = file.WriteString("agent-memory-v1 " + identity + " " + j.digest("journal-header", identity) + "\n")
	return errors.Join(err, file.Sync(), file.Close())
}

func OpenJournal(path string, key []byte) (*Journal, error) {
	if len(key) != 32 {
		return nil, ErrInput
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("%w: journal unavailable", ErrRecovery)
	}
	j := &Journal{file: file, key: append([]byte(nil), key...), byOperation: map[string]entry{}}
	if err = lockJournal(file); err != nil {
		file.Close()
		return nil, fmt.Errorf("%w: journal already in use or inaccessible", ErrRecovery)
	}
	if err = j.load(); err != nil {
		j.Close()
		return nil, err
	}
	return j, nil
}

func (j *Journal) Close() error {
	if j.file == nil {
		return nil
	}
	return j.file.Close()
}

func (j *Journal) load() error {
	info, err := j.file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxJournalBytes {
		return fmt.Errorf("%w: invalid journal file", ErrRecovery)
	}
	reader := bufio.NewReaderSize(j.file, 2*1024*1024)
	header, err := reader.ReadSlice('\n')
	parts := strings.Fields(string(header))
	if err != nil || len(parts) != 3 || parts[0] != "agent-memory-v1" || len(parts[1]) != 32 ||
		subtle.ConstantTimeCompare([]byte(parts[2]), []byte(j.digest("journal-header", parts[1]))) != 1 {
		return fmt.Errorf("%w: journal header does not match the key", ErrRecovery)
	}
	j.identity = parts[1]
	prior := ""
	for {
		line, err := reader.ReadSlice('\n')
		if errors.Is(err, io.EOF) && len(line) == 0 {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: incomplete or oversized journal entry", ErrRecovery)
		}
		var e entry
		if json.Unmarshal(line, &e) != nil || e.Seq != uint64(len(j.entries)+1) || e.Prev != prior ||
			subtle.ConstantTimeCompare([]byte(e.MAC), []byte(j.sign(e))) != 1 {
			return fmt.Errorf("%w: journal sequence or signature mismatch", ErrRecovery)
		}
		j.entries = append(j.entries, e)
		j.byOperation[e.Operation] = e
		prior = e.MAC
	}
	return nil
}

func (j *Journal) sign(e entry) string {
	e.MAC = ""
	body, _ := json.Marshal(e)
	return j.digest("journal-entry", j.identity, string(body))
}

func (j *Journal) append(e entry) (entry, error) {
	if len(j.entries) > 0 {
		e.Prev = j.entries[len(j.entries)-1].MAC
	}
	e.Seq = uint64(len(j.entries) + 1)
	e.MAC = j.sign(e)
	body, _ := json.Marshal(e)
	offset, err := j.file.Seek(0, io.SeekCurrent)
	if err != nil || offset+int64(len(body)+1) > maxJournalBytes {
		return entry{}, ErrRecovery
	}
	if _, err = j.file.Write(append(body, '\n')); err != nil {
		return entry{}, err
	}
	if err = j.file.Sync(); err != nil {
		return entry{}, err
	}
	j.entries = append(j.entries, e)
	j.byOperation[e.Operation] = e
	return e, nil
}

func (s *Service) recover(ctx context.Context) error {
	var seq uint64
	var mac, identity string
	if err := s.db.QueryRowContext(ctx, "SELECT seq,mac,identity FROM journal_state WHERE singleton=1").Scan(&seq, &mac, &identity); err != nil {
		return err
	}
	if identity == "" && seq == 0 && mac == "" {
		_, err := s.db.ExecContext(ctx, "UPDATE journal_state SET identity=? WHERE singleton=1 AND identity=''", s.journal.identity)
		if err != nil {
			return err
		}
		identity = s.journal.identity
	}
	if identity != s.journal.identity {
		return fmt.Errorf("%w: database belongs to another journal", ErrRecovery)
	}
	if seq > uint64(len(s.journal.entries)) || seq == 0 && mac != "" || seq > 0 && s.journal.entries[seq-1].MAC != mac {
		return fmt.Errorf("%w: database watermark is ahead of, or diverged from, this journal", ErrRecovery)
	}
	for _, e := range s.journal.entries[seq:] {
		if err := s.tx(ctx, func(tx *sql.Tx) error { return s.apply(ctx, tx, e) }); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) apply(ctx context.Context, tx *sql.Tx, e entry) error {
	var result sql.Result
	var err error
	switch e.Kind {
	case "forget-slot", "forget-subject":
		if err = withdrawTeamDependencies(ctx, tx, e); err != nil {
			return err
		}
		for _, source := range e.Sources {
			if _, err = tx.ExecContext(ctx, "INSERT IGNORE INTO forgotten_sources VALUES(?,?)", e.Scope, source); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "DELETE FROM sources WHERE scope_hash=? AND source_hash=? AND occurred<=?", e.Scope, source, e.At); err != nil {
				return err
			}
		}
		filter := "scope_hash=? AND occurred<=?"
		args := []any{e.Scope, e.At}
		factFilter := "scope_hash=? AND effective<=?"
		factArgs := []any{e.Scope, e.At}
		if e.Kind == "forget-slot" {
			filter += " AND slot_hash=?"
			args = append(args, e.Selector)
			factFilter += " AND slot_hash=?"
			factArgs = append(factArgs, e.Selector)
			if _, err = tx.ExecContext(ctx, "DELETE FROM source_conflicts WHERE "+filter, args...); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO forget_cuts(scope_hash,selector,cutoff) VALUES(?,?,?) ON DUPLICATE KEY UPDATE cutoff=GREATEST(cutoff,?)",
			e.Scope, e.Selector, e.At, e.At); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT IGNORE INTO forgotten_sources SELECT scope_hash,source_hash FROM sources WHERE "+filter, args...); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT IGNORE INTO forgotten_sources SELECT scope_hash,source_hash FROM facts WHERE "+factFilter, factArgs...); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM facts WHERE "+factFilter, factArgs...); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM sources WHERE "+filter, args...); err != nil {
			return err
		}
		if e.Kind == "forget-subject" {
			if _, err = tx.ExecContext(ctx, "DELETE FROM consent WHERE scope_hash=?", e.Scope); err != nil {
				return err
			}
		}
	case "revoke-consent":
		if _, err = tx.ExecContext(ctx, "DELETE FROM consent WHERE scope_hash=? AND grant_hash=?", e.Scope, e.Selector); err != nil {
			return err
		}
	case "revoke-share":
		if _, err = tx.ExecContext(ctx, "DELETE FROM shares WHERE scope_hash=? AND share_hash=?", e.Scope, e.Selector); err != nil {
			return err
		}
	case "withdraw-team-nomination":
		if err = withdrawTeamDependencies(ctx, tx, e); err != nil {
			return err
		}
	default:
		return ErrRecovery
	}
	if e.Kind != "revoke-share" && e.Kind != "withdraw-team-nomination" {
		if _, err = tx.ExecContext(ctx, "UPDATE subjects SET epoch=epoch+1 WHERE scope_hash=?", e.Scope); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM shares WHERE scope_hash=?", e.Scope); err != nil {
			return err
		}
	}
	result, err = tx.ExecContext(ctx, "UPDATE journal_state SET seq=?,mac=? WHERE singleton=1 AND seq=? AND mac=?", e.Seq, e.MAC, e.Seq-1, e.Prev)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return ErrRecovery
	}
	return nil
}

type Receipt struct {
	ID        string    `json:"id"`
	Watermark uint64    `json:"watermark"`
	At        time.Time `json:"at"`
}

func (s *Service) journalAction(ctx context.Context, who Identity, slot, kind, app, purpose, key string) (Receipt, error) {
	if who.Role != "owner" || !validID(key) {
		return Receipt{}, ErrDenied
	}
	if err := s.lock(ctx); err != nil {
		return Receipt{}, err
	}
	defer s.mu.Unlock()
	scope := s.scope(who.Tenant, who.Actor)
	selector := ""
	switch kind {
	case "forget-slot":
		if !validID(slot) {
			return Receipt{}, ErrInput
		}
		selector = s.slot(scope, slot)
	case "forget-subject":
		selector = strings.Repeat("0", 64)
	case "revoke-consent":
		if !validID(app) || !validID(purpose) {
			return Receipt{}, ErrInput
		}
		selector = s.grant(scope, app, purpose)
	case "revoke-share":
		if len(slot) != 64 {
			return Receipt{}, ErrInput
		}
		selector = hash(slot)
	case "withdraw-team-nomination":
		if len(slot) != 32 || strings.Trim(slot, "0123456789abcdef") != "" {
			return Receipt{}, ErrInput
		}
		selector = slot
	}
	operation := s.journal.digest("operation", scope, key)
	var completed entry
	attempted := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := authorize(ctx, tx, who); err != nil {
			return err
		}
		if prior, ok := s.journal.byOperation[operation]; ok {
			if prior.Kind != kind || prior.Selector != selector {
				return ErrConflict
			}
			completed = prior
			return nil
		}
		if kind == "withdraw-team-nomination" {
			var found bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM team_nominations WHERE nomination=? AND source_scope=?)`,
				selector, scope).Scan(&found); err != nil {
				return err
			}
			if !found {
				return ErrMissing
			}
		}
		draft := entry{Kind: kind, Scope: scope, Selector: selector, Operation: operation, At: time.Now().UTC().Truncate(time.Microsecond)}
		if kind == "forget-slot" {
			sources, err := sourceIDs(ctx, tx, scope, selector, draft.At)
			if err != nil {
				return err
			}
			draft.Sources = sources
		}
		attempted = true
		var err error
		completed, err = s.journal.append(draft)
		if err != nil {
			return err
		}
		if err = s.apply(ctx, tx, completed); err != nil {
			return err
		}
		return event(ctx, tx, who.Tenant, scope, kind, operation, "applied", 0)
	})
	if attempted && err != nil {
		s.unavailable = true
		return Receipt{}, ErrRecovery
	}
	if err != nil {
		return Receipt{}, err
	}
	return Receipt{ID: completed.Operation, Watermark: completed.Seq, At: completed.At}, nil
}

func sourceIDs(ctx context.Context, tx *sql.Tx, scope, selector string, cutoff time.Time) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT source_hash FROM sources WHERE scope_hash=? AND slot_hash=? AND occurred<=?
		ORDER BY source_hash LIMIT 20001`, scope, selector, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		list = append(list, id)
	}
	if len(list) > 20000 {
		return nil, ErrConflict
	}
	return list, rows.Err()
}

func (s *Service) Forget(ctx context.Context, who Identity, slot, key string) (Receipt, error) {
	kind := "forget-slot"
	if slot == "" {
		kind = "forget-subject"
	}
	return s.journalAction(ctx, who, slot, kind, "", "", key)
}

type ForgetProgress struct {
	Receipt
	Status string `json:"status"`
}

func (s *Service) ForgetStatus(ctx context.Context, who Identity, id string) (ForgetProgress, error) {
	if who.Role != "owner" {
		return ForgetProgress{}, ErrDenied
	}
	if len(id) != 64 {
		return ForgetProgress{}, ErrInput
	}
	if _, err := hex.DecodeString(id); err != nil {
		return ForgetProgress{}, ErrInput
	}
	if err := s.lock(ctx); err != nil {
		return ForgetProgress{}, err
	}
	defer s.mu.Unlock()
	var result ForgetProgress
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := authorize(ctx, tx, who); err != nil {
			return err
		}
		operation, found := s.journal.byOperation[id]
		if !found || operation.Scope != s.scope(who.Tenant, who.Actor) ||
			operation.Kind != "forget-slot" && operation.Kind != "forget-subject" {
			return ErrMissing
		}
		var applied uint64
		if err := tx.QueryRowContext(ctx, "SELECT seq FROM journal_state WHERE singleton=1").Scan(&applied); err != nil {
			return err
		}
		if applied < operation.Seq {
			return ErrRecovery
		}
		result = ForgetProgress{Receipt: Receipt{ID: id, Watermark: operation.Seq, At: operation.At}, Status: "applied"}
		return nil
	})
	return result, err
}

func (s *Service) RevokeConsent(ctx context.Context, who Identity, app, purpose, key string) (Receipt, error) {
	return s.journalAction(ctx, who, "", "revoke-consent", app, purpose, key)
}
func (s *Service) RevokeShare(ctx context.Context, who Identity, link, key string) (Receipt, error) {
	return s.journalAction(ctx, who, link, "revoke-share", "", "", key)
}
