package memory

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
)

var (
	ErrInput     = errors.New("invalid input")
	ErrDenied    = errors.New("access denied")
	ErrMissing   = errors.New("object not found")
	ErrConflict  = errors.New("operation conflicts with current state")
	ErrForgotten = errors.New("source suppressed by the active forgetting watermark")
	ErrRecovery  = errors.New("service requires journal recovery")
)

//go:embed schema/*.sql
var schema embed.FS

var migrations = []string{"schema/01.sql", "schema/02.sql", "schema/03.sql", "schema/04.sql"}

type Service struct {
	db          *sql.DB
	guard       *sql.Conn
	lockName    string
	journal     *Journal
	mu          sync.Mutex
	unavailable bool
}

type Identity struct {
	Tenant    string `json:"tenant"`
	Actor     string `json:"actor"`
	Role      string `json:"role"`
	tokenHash string
}

func Connect(dsn string) (*sql.DB, error) {
	config, err := mysql.ParseDSN(dsn)
	if err != nil || config.DBName == "" {
		return nil, ErrInput
	}
	config.ParseTime = true
	config.Loc = time.UTC
	config.MultiStatements = false
	config.Timeout = 3 * time.Second
	config.ReadTimeout = 12 * time.Second
	config.WriteTimeout = 12 * time.Second
	config.Params = map[string]string{"time_zone": "'+00:00'"}
	connector, err := mysql.NewConnector(config)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(12)
	db.SetMaxIdleConns(3)
	return db, nil
}

func acquire(ctx context.Context, db *sql.DB) (*sql.Conn, string, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, "", err
	}
	var database string
	if err = conn.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&database); err != nil {
		conn.Close()
		return nil, "", err
	}
	name := "agent-memory:" + hash(database)[:40]
	var locked sql.NullInt64
	err = conn.QueryRowContext(ctx, "SELECT GET_LOCK(?,0)", name).Scan(&locked)
	if err != nil || !locked.Valid || locked.Int64 != 1 {
		conn.Close()
		return nil, "", fmt.Errorf("%w: schema is in use", ErrRecovery)
	}
	return conn, name, nil
}

func unlock(conn *sql.Conn, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := conn.ExecContext(ctx, "DO RELEASE_LOCK(?)", name)
	return errors.Join(err, conn.Close())
}

func Migrate(ctx context.Context, db *sql.DB) error {
	conn, name, err := acquire(ctx, db)
	if err != nil {
		return err
	}
	defer unlock(conn, name)
	if _, err = conn.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS migrations(version INT PRIMARY KEY)"); err != nil {
		return err
	}
	for index, file := range migrations {
		version := index + 1
		var count int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM migrations WHERE version=?", version).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			continue
		}
		data, err := schema.ReadFile(file)
		if err != nil {
			return err
		}
		for part := range strings.SplitSeq(string(data), ";") {
			if query := strings.TrimSpace(part); query != "" {
				if _, err := conn.ExecContext(ctx, query); err != nil {
					return fmt.Errorf("migration %d: %w", version, err)
				}
			}
		}
		if _, err = conn.ExecContext(ctx, "INSERT INTO migrations(version) VALUES(?)", version); err != nil {
			return err
		}
	}
	return nil
}

func Open(ctx context.Context, db *sql.DB, path string, key []byte) (*Service, error) {
	guard, name, err := acquire(ctx, db)
	if err != nil {
		return nil, err
	}
	var migrated int
	err = guard.QueryRowContext(ctx, "SELECT COUNT(*) FROM migrations WHERE version BETWEEN 1 AND ?", len(migrations)).Scan(&migrated)
	if err != nil || migrated != len(migrations) {
		unlock(guard, name)
		return nil, fmt.Errorf("%w: run agent-memory migrate before opening this database", ErrRecovery)
	}
	journal, err := OpenJournal(path, key)
	if err != nil {
		unlock(guard, name)
		return nil, err
	}
	service := &Service{db: db, guard: guard, lockName: name, journal: journal}
	if err = service.recover(ctx); err != nil {
		service.Close()
		return nil, err
	}
	return service, nil
}

func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unavailable = true
	return errors.Join(s.journal.Close(), unlock(s.guard, s.lockName))
}

func (s *Service) lock(ctx context.Context) error {
	s.mu.Lock()
	if err := s.ready(ctx); err != nil {
		s.mu.Unlock()
		return err
	}
	return nil
}

func (s *Service) Check(ctx context.Context) error {
	if err := s.lock(ctx); err != nil {
		return err
	}
	s.mu.Unlock()
	return nil
}

func (s *Service) ready(ctx context.Context) error {
	if s.unavailable {
		return ErrRecovery
	}
	var owns bool
	err := s.guard.QueryRowContext(ctx, "SELECT COALESCE(IS_USED_LOCK(?)=CONNECTION_ID(),0)", s.lockName).Scan(&owns)
	if err != nil || !owns {
		s.unavailable = true
		return ErrRecovery
	}
	return nil
}

func (s *Service) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) Provision(ctx context.Context, tenant, actor, role string, life time.Duration) (string, error) {
	if !validID(tenant) || !validID(actor) || life < time.Minute || life > 30*24*time.Hour ||
		(role != "owner" && role != "app" && role != "audit" && role != "admin") {
		return "", ErrInput
	}
	if err := s.lock(ctx); err != nil {
		return "", err
	}
	defer s.mu.Unlock()
	token := randomID(32)
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if role == "owner" {
			if _, err := tx.ExecContext(ctx, "INSERT IGNORE INTO subjects(tenant,subject,scope_hash) VALUES(?,?,?)", tenant, actor, s.scope(tenant, actor)); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO principals VALUES(?,?,?,?,?)", hash(token), tenant, actor, role, time.Now().UTC().Add(life))
		return err
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

func (s *Service) Authenticate(ctx context.Context, token string) (Identity, error) {
	if len(token) != 64 {
		return Identity{}, ErrDenied
	}
	digest := hash(token)
	result := Identity{tokenHash: digest}
	err := s.db.QueryRowContext(ctx, "SELECT tenant,actor,role FROM principals WHERE token=? AND expires>UTC_TIMESTAMP(6)", digest).Scan(&result.Tenant, &result.Actor, &result.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return Identity{}, ErrDenied
	}
	return result, err
}

func authorize(ctx context.Context, tx *sql.Tx, identity Identity) error {
	var exists int
	err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM principals WHERE token=? AND tenant=? AND actor=? AND role=? AND expires>UTC_TIMESTAMP(6)",
		identity.tokenHash, identity.Tenant, identity.Actor, identity.Role).Scan(&exists)
	if err != nil {
		return err
	}
	if exists != 1 {
		return ErrDenied
	}
	return nil
}

func (s *Service) scope(tenant, subject string) string {
	return s.journal.digest("subject", tenant, subject)
}
func (s *Service) slot(scope, slot string) string { return s.journal.digest("slot", scope, slot) }
func (s *Service) source(scope, app, session, external string) string {
	return s.journal.digest("source", scope, app, session, external)
}
func (s *Service) grant(scope, app, purpose string) string {
	return s.journal.digest("consent", scope, app, purpose)
}
func hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func randomID(size int) string { b := make([]byte, size); rand.Read(b); return hex.EncodeToString(b) }
func validID(value string) bool {
	if len(value) == 0 || len(value) > 48 {
		return false
	}
	for i, c := range value {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || i != 0 && (c == '-' || c == '_') {
			continue
		}
		return false
	}
	return true
}

func WriteKey(path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.WriteString(randomID(32) + "\n")
	syncErr := file.Sync()
	return errors.Join(writeErr, syncErr, file.Close())
}

func ReadKey(path string) ([]byte, error) {
	file, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(file)))
	if err != nil || len(key) != 32 {
		return nil, ErrInput
	}
	return key, nil
}

func (j *Journal) digest(label string, values ...string) string {
	h := hmac.New(sha256.New, j.key)
	fmt.Fprint(h, label)
	for _, value := range values {
		fmt.Fprintf(h, "/%d/%s", len(value), value)
	}
	return hex.EncodeToString(h.Sum(nil))
}
