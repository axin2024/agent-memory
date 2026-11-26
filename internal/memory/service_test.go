package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func must[T any](t *testing.T, v T, err error) T {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func verify(t *testing.T, ok bool, format string, args ...any) {
	t.Helper()
	if !ok {
		t.Fatalf(format, args...)
	}
}

func noError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func openTest(t *testing.T, variable string) (context.Context, *Service, *sql.DB, string, []byte) {
	t.Helper()
	dsn := os.Getenv(variable)
	if dsn == "" {
		t.Skip("set " + variable + " to a disposable MySQL schema")
	}
	ctx := t.Context()
	db, err := Connect(dsn)
	db = must(t, db, err)
	t.Cleanup(func() { db.Close() })
	noError(t, Migrate(ctx, db))
	dir := t.TempDir()
	path := filepath.Join(dir, "journal")
	keyPath := filepath.Join(dir, "key")
	noError(t, WriteKey(keyPath))
	key, err := ReadKey(keyPath)
	key = must(t, key, err)
	noError(t, InitJournal(path, key))
	service, err := Open(ctx, db, path, key)
	service = must(t, service, err)
	t.Cleanup(func() {
		if !service.unavailable {
			service.Close()
		}
	})
	return ctx, service, db, path, key
}

func actor(t *testing.T, ctx context.Context, s *Service, tenant, name, role string) Identity {
	t.Helper()
	token, err := s.Provision(ctx, tenant, name, role, time.Hour)
	token = must(t, token, err)
	who, err := s.Authenticate(ctx, token)
	return must(t, who, err)
}

func grant(t *testing.T, ctx context.Context, s *Service, owner Identity, app, purpose string, auto bool) {
	t.Helper()
	noError(t, s.SetConsent(ctx, owner, Consent{App: app, Purpose: purpose, Read: true, Write: true, Auto: auto, Expires: time.Now().Add(time.Hour)}))
}

func ask(t *testing.T, ctx context.Context, s *Service, who Identity, request Recall) []Fact {
	t.Helper()
	facts, err := s.Recall(ctx, who, request)
	return must(t, facts, err)
}

func write(t *testing.T, ctx context.Context, s *Service, who Identity, in Source) Submission {
	t.Helper()
	res, err := s.Ingest(ctx, who, in)
	return must(t, res, err)
}

func TestPrivacyLifecycle(t *testing.T) {
	ctx, s, db, path, key := openTest(t, "MEMORY_TEST_DSN")
	owner := actor(t, ctx, s, "lifecycle", "riley", "owner")
	a := actor(t, ctx, s, owner.Tenant, "assistant-a", "app")
	b := actor(t, ctx, s, owner.Tenant, "assistant-b", "app")
	untrusted := actor(t, ctx, s, owner.Tenant, "assistant-c", "app")
	other := actor(t, ctx, s, "another-lifecycle", "assistant-a", "app")
	admin := actor(t, ctx, s, owner.Tenant, "manager", "admin")
	auditor := actor(t, ctx, s, owner.Tenant, "reviewer", "audit")
	grant(t, ctx, s, owner, a.Actor, "support", true)
	grant(t, ctx, s, owner, b.Actor, "support", true)
	grant(t, ctx, s, owner, a.Actor, "orders", true)

	one := Source{owner.Actor, "support", "first", "notice", time.Now().Add(-time.Hour), "user", "My long-term preference for notifications is email."}
	base := write(t, ctx, s, a, one)
	verify(t, base.Status == "applied", "first write: %s", base.Status)
	query := Recall{Subject: owner.Actor, Purpose: "support", Slot: "notifications"}
	for _, who := range []Identity{a, b} {
		found := ask(t, ctx, s, who, query)
		verify(t, len(found) == 1 && found[0].Value == "email" && found[0].Source.ID == base.Source, "the two applications did not recall the same source")
	}
	replayed := write(t, ctx, s, a, one)
	verify(t, replayed.Replayed && replayed.Source == base.Source && replayed.Candidate == base.Candidate, "source replay created another record")
	changed := one
	changed.Statement = "My long-term preference for notifications is letter."
	_, err := s.Ingest(ctx, a, changed)
	var conflict *SourceConflictError
	verify(t, errors.As(err, &conflict) && conflict.ID != 0 && !conflict.Replayed, "changed replay was not queued: %v", err)
	_, err = s.Ingest(ctx, a, changed)
	var secondConflict *SourceConflictError
	verify(t, errors.As(err, &secondConflict) && secondConflict.ID == conflict.ID && secondConflict.Replayed, "changed replay duplicated the queue: %v", err)
	var storedSources, queued int
	noError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sources WHERE source_hash=?", base.Source).Scan(&storedSources))
	noError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM source_conflicts WHERE source_hash=?", base.Source).Scan(&queued))
	verify(t, storedSources == 1 && queued == 1, "source or conflict was duplicated")
	page, err := s.SourceConflicts(ctx, owner, 0)
	page = must(t, page, err)
	verify(t, len(page.Items) == 1 && page.Items[0].ID == conflict.ID && page.Items[0].Source == base.Source, "queued conflict lacks provenance")
	for _, who := range []Identity{untrusted, other, auditor} {
		_, err = s.Recall(ctx, who, Recall{Subject: owner.Actor, Purpose: "support", Slot: "notifications"})
		verify(t, errors.Is(err, ErrDenied), "unauthorized recall: %v", err)
	}

	quote := Source{owner.Actor, "support", "quote", "quote", time.Now().Add(-30 * time.Minute), "user", "My friend said “My long-term preference for notifications is push.”"}
	verify(t, write(t, ctx, s, a, quote).Candidate == "", "someone else's preference produced a candidate")
	secret := Source{owner.Actor, "support", "secret", "example", time.Now().Add(-30 * time.Minute), "user", "My password is fixture-only."}
	sensitiveSource := write(t, ctx, s, a, secret)
	verify(t, sensitiveSource.Status == "sensitive", "secret classification was %s", sensitiveSource.Status)
	var byteCount sql.NullInt64
	noError(t, db.QueryRowContext(ctx, "SELECT LENGTH(statement) FROM sources WHERE source_hash=?", sensitiveSource.Source).Scan(&byteCount))
	verify(t, !byteCount.Valid, "sensitive body was stored")

	newer := one
	newer.Session, newer.External, newer.Statement, newer.Occurred = "later", "notice", "Remember my notifications as sms.", time.Now().Add(-time.Minute)
	pending := write(t, ctx, s, b, newer)
	verify(t, pending.Status == "conflict", "different applications should produce a conflict")
	options, err := s.Pending(ctx, owner)
	options = must(t, options, err)
	verify(t, len(options) == 1 && options[0].Evidence == newer.Statement && options[0].Current != nil &&
		options[0].Current.Source.Evidence == one.Statement && options[0].Current.Source.ID == base.Source, "comparison omitted one of the two proofs")
	status, err := s.Review(ctx, owner, pending.Candidate, 1, true)
	verify(t, err == nil && status == "applied", "direct conflict review: %s %v", status, err)
	for _, who := range []Identity{a, b} {
		found := ask(t, ctx, s, who, query)
		verify(t, len(found) == 1 && found[0].Value == "sms" && found[0].Source.ID == pending.Source &&
			found[0].Source.Session == newer.Session && found[0].Source.Evidence == newer.Statement, "missing SMS correction provenance")
	}
	at := time.Now().Add(-30 * time.Minute)
	historical := query
	historical.AsOf = at
	old := ask(t, ctx, s, b, historical)
	verify(t, len(old) == 1 && old[0].Value == "email" && old[0].Replaced != nil, "historical interval or source mismatch")
	late := one
	late.Session, late.External, late.Occurred = "late", "notice", at
	verify(t, write(t, ctx, s, a, late).Status == "stale", "late data replaced a more recent decision")

	separate := one
	separate.Session, separate.Purpose, separate.Statement = "order", "orders", "Remember my notifications as portal."
	verify(t, write(t, ctx, s, a, separate).Status == "applied", "separate purpose did not retain its own interval")
	verify(t, ask(t, ctx, s, a, Recall{Subject: owner.Actor, Purpose: "orders", Slot: "notifications"})[0].Value == "portal", "wrong purpose value")
	edit := Edit{Slot: "notifications", Value: "text message", Purpose: "support", Key: "owner-correction", Expected: 2}
	v, err := s.Edit(ctx, owner, edit)
	verify(t, err == nil && v == 3, "owner correction failed: %v %v", v, err)
	v, err = s.Edit(ctx, owner, edit)
	verify(t, err == nil && v == 3, "correction replay failed: %v %v", v, err)
	for _, who := range []Identity{a, b} {
		current := ask(t, ctx, s, who, query)
		verify(t, len(current) == 1 && current[0].Value == "text message" && current[0].Source.Session == "owner-edit", "application saw an obsolete fact")
	}
	link, err := s.CreateShare(ctx, owner, "notifications", "support", 30)
	link = must(t, link, err)
	shared, err := s.ReadShare(ctx, link.Token)
	verify(t, err == nil && shared.Value == "text message", "share missed selected version")
	remainingLink, err := s.CreateShare(ctx, owner, "notifications", "orders", 30)
	remainingLink = must(t, remainingLink, err)
	snapshot, err := s.Export(ctx, owner)
	snapshot = must(t, snapshot, err)
	verify(t, len(snapshot.Facts) == 2 && len(snapshot.Consents) == 3, "incomplete filtered export")

	var standby *sql.DB
	if dsn := os.Getenv("MEMORY_TEST_RESTORE_DSN"); dsn != "" {
		standby, err = Connect(dsn)
		standby = must(t, standby, err)
		defer standby.Close()
		copySnapshot(t, ctx, db, standby)
	}
	revoked, err := s.RevokeShare(ctx, owner, link.Token, "revoke-one-link")
	revoked = must(t, revoked, err)
	_, err = s.ReadShare(ctx, link.Token)
	verify(t, errors.Is(err, ErrMissing), "revoked link was available")
	_, err = s.ReadShare(ctx, remainingLink.Token)
	noError(t, err)
	_, err = s.RevokeConsent(ctx, owner, b.Actor, "support", "revoke-assistant-b")
	noError(t, err)
	_, err = s.Recall(ctx, b, historical)
	verify(t, errors.Is(err, ErrDenied), "historical access bypassed current consent")
	receipt, err := s.Forget(ctx, owner, "notifications", "forget-notifications")
	receipt = must(t, receipt, err)
	verify(t, receipt.Watermark == revoked.Watermark+2, "watermark does not include consent revocation")
	retry, err := s.Forget(ctx, owner, "notifications", "forget-notifications")
	verify(t, err == nil && retry == receipt, "forget replay did not preserve receipt")
	_, err = s.Ingest(ctx, a, one)
	verify(t, errors.Is(err, ErrForgotten), "old source was accepted after forgetting")
	_, err = s.Ingest(ctx, a, changed)
	verify(t, errors.Is(err, ErrForgotten), "conflicting body was accepted after forgetting")
	page, err = s.SourceConflicts(ctx, owner, 0)
	verify(t, err == nil && len(page.Items) == 0, "forgetting left a queue item")
	noError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM source_conflicts WHERE source_hash=?", base.Source).Scan(&queued))
	verify(t, queued == 0, "forgetting left durable conflict metadata")
	freshIdentity := one
	freshIdentity.External, freshIdentity.Session = "old-other", "backlog"
	_, err = s.Ingest(ctx, a, freshIdentity)
	verify(t, errors.Is(err, ErrForgotten), "old timestamp bypassed the slot cutoff")
	verify(t, len(ask(t, ctx, s, a, query)) == 0 && len(ask(t, ctx, s, a, historical)) == 0, "deleted value was recalled")
	_, err = s.ReadShare(ctx, remainingLink.Token)
	verify(t, errors.Is(err, ErrMissing), "share survived forgetting")
	_, err = s.Audit(ctx, auditor, 0)
	noError(t, err)
	_, err = s.Export(ctx, admin)
	verify(t, errors.Is(err, ErrDenied), "administrator received fact bodies")
	noError(t, s.SetPolicy(ctx, admin, 1, 30))
	_, err = s.Sweep(ctx, admin)
	noError(t, err)

	if standby != nil {
		var count int
		noError(t, standby.QueryRowContext(ctx, "SELECT COUNT(*) FROM facts WHERE value='text message'").Scan(&count))
		verify(t, count == 1, "backup did not contain pre-deletion facts")
		noError(t, standby.QueryRowContext(ctx, "SELECT COUNT(*) FROM source_conflicts WHERE source_hash=?", base.Source).Scan(&count))
		verify(t, count == 1, "backup did not contain the original conflict")
		noError(t, s.Close())
		backup, err := Open(ctx, standby, path, key)
		backup = must(t, backup, err)
		defer backup.Close()
		verify(t, len(ask(t, ctx, backup, a, query)) == 0 && len(ask(t, ctx, backup, a, historical)) == 0, "restore reintroduced a forgotten value")
		_, err = backup.Recall(ctx, b, historical)
		verify(t, errors.Is(err, ErrDenied), "restore revived withdrawn consent")
		_, err = backup.ReadShare(ctx, link.Token)
		verify(t, errors.Is(err, ErrMissing), "restore revived withdrawn capability")
		_, err = backup.Ingest(ctx, a, one)
		verify(t, errors.Is(err, ErrForgotten), "restored backup accepted forbidden replay")
		noError(t, standby.QueryRowContext(ctx, "SELECT COUNT(*) FROM source_conflicts WHERE source_hash=?", base.Source).Scan(&count))
		verify(t, count == 0, "restored backup retained deleted conflict metadata")
		t.Logf("old MySQL backup caught up through sequence %d", receipt.Watermark)
	}
}

func copySnapshot(t *testing.T, ctx context.Context, source, standby *sql.DB) {
	t.Helper()
	noError(t, Migrate(ctx, standby))
	var original, target string
	noError(t, source.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&original))
	noError(t, standby.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&target))
	for _, name := range []string{original, target} {
		verify(t, name != "" && strings.Trim(name, "abcdefghijklmnopqrstuvwxyz0123456789_") == "", "test requires simple disposable database names")
	}
	verify(t, original != target, "snapshot destination must be independent")
	tx, err := standby.BeginTx(ctx, nil)
	tx = must(t, tx, err)
	defer tx.Rollback()
	for _, table := range []string{"subjects", "principals", "policy", "consent", "sources", "source_conflicts", "candidates", "model_jobs", "facts", "shares", "journal_state", "forget_cuts", "forgotten_sources", "events"} {
		if table == "journal_state" {
			_, err = tx.ExecContext(ctx, "DELETE FROM journal_state")
			noError(t, err)
		}
		_, err = tx.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s SELECT * FROM %s.%s", table, original, table))
		noError(t, err)
	}
	noError(t, tx.Commit())
}

func TestFrozenRecall(t *testing.T) {
	ctx, s, _, _, _ := openTest(t, "MEMORY_TEST_EVAL_DSN")
	a := actor(t, ctx, s, "frozen", "primary", "app")
	b := actor(t, ctx, s, "held-out", "primary", "app")
	records := []map[string]any{}
	latencies := []int64{}
	slots := []string{"notifications", "language", "units", "timezone", "delivery-method"}
	for i := 0; i < 100; i++ {
		subject := fmt.Sprintf("subject-%03d", i)
		owner := actor(t, ctx, s, a.Tenant, subject, "owner")
		other := actor(t, ctx, s, b.Tenant, subject, "owner")
		neighbor := actor(t, ctx, s, a.Tenant, fmt.Sprintf("neighbor-%03d", i), "owner")
		grant(t, ctx, s, owner, a.Actor, "support", true)
		grant(t, ctx, s, other, b.Actor, "support", true)
		grant(t, ctx, s, neighbor, a.Actor, "support", true)
		slot := slots[i%len(slots)]
		at := time.Now().Add(-2 * time.Hour)
		request := Recall{Subject: subject, Purpose: "support", Query: slot + " deliberate match"}
		input := Source{subject, "support", "initial", "pref", at, "user", "Remember my " + slot + " as approved-" + fmt.Sprint(i) + "."}
		expected := write(t, ctx, s, a, input)
		input.Statement = "Remember my " + slot + " as unrelated deliberate match " + fmt.Sprint(i) + "."
		distractor := write(t, ctx, s, b, input)
		input.Subject = neighbor.Actor
		nearby := write(t, ctx, s, a, input)
		verify(t, distractor.Source != expected.Source && nearby.Source != expected.Source, "source identity collision across tenants or users")
		group := map[string]any{"case": subject, "slot": slot, "expected": "traceable-hit"}
		switch (i / 5) % 4 {
		case 1:
			group["expected"] = "corrected-version"
			edit := Edit{Slot: slot, Value: "corrected-" + fmt.Sprint(i), Purpose: "support", Key: "correction", Expected: 1}
			v, err := s.Edit(ctx, owner, edit)
			verify(t, err == nil && v == 2, "correction %d: %v", i, err)
		case 2:
			group["expected"] = "forgotten"
			_, err := s.Forget(ctx, owner, slot, "forget")
			noError(t, err)
			request.AsOf = at.Add(time.Minute)
		case 3:
			group["expected"] = "currently-unauthorized"
			_, err := s.RevokeConsent(ctx, owner, a.Actor, "support", "revoke")
			noError(t, err)
			request.AsOf = at.Add(time.Minute)
		}
		start := time.Now()
		result, err := s.Recall(ctx, a, request)
		latency := time.Since(start).Microseconds()
		latencies = append(latencies, latency)
		group["duration_us"] = latency
		switch group["expected"] {
		case "forgotten":
			verify(t, err == nil && len(result) == 0, "forgotten case %d leaked", i)
		case "currently-unauthorized":
			verify(t, errors.Is(err, ErrDenied), "unauthorized case %d leaked: %v", i, err)
		case "corrected-version":
			verify(t, err == nil && len(result) == 1 && result[0].Version == 2 && result[0].Source.Session == "owner-edit" &&
				result[0].Source.Evidence != "", "owner correction lacked attribution, case %d: %v", i, err)
		default:
			verify(t, err == nil && len(result) == 1 && result[0].Source.ID == expected.Source && result[0].Source.Evidence != "", "attribution mismatch %d: %v", i, err)
		}
		if len(result) == 1 {
			verify(t, result[0].Source.ID != distractor.Source && result[0].Source.ID != nearby.Source, "an unrelated source leaked")
			group["source"] = result[0].Source.ID
			group["version"] = result[0].Version
		}
		records = append(records, group)
	}
	slices.Sort(latencies)
	doc := map[string]any{"groups": len(records), "cross_scope_leaks": 0, "misses": 0,
		"higher_scoring_decoys": map[string]int{"different_tenant": 100, "different_user": 100},
		"p95_us":                latencies[94], "mode": "deterministic", "cases": records}
	if path := os.Getenv("MEMORY_TEST_EVIDENCE"); path != "" {
		encoded, err := json.MarshalIndent(doc, "", "  ")
		encoded = must(t, encoded, err)
		noError(t, os.WriteFile(path, append(encoded, '\n'), 0600))
	}
	t.Logf("100 groups, 50 attributed hits, 25 forgotten, 25 unauthorized, P95 %d us", latencies[94])
}

func TestJournalRejectsWrongKeyAndTornTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal")
	key := make([]byte, 32)
	noError(t, InitJournal(path, key))
	wrong := make([]byte, 32)
	wrong[0] = 42
	_, err := OpenJournal(path, wrong)
	verify(t, errors.Is(err, ErrRecovery), "wrong key accepted")
	j, err := OpenJournal(path, key)
	j = must(t, j, err)
	_, err = OpenJournal(path, key)
	verify(t, errors.Is(err, ErrRecovery), "two processes can hold the journal")
	_, err = j.append(entry{Kind: "forget-slot", Scope: "scope", Selector: "slot", Operation: "operation", At: time.Now().UTC()})
	noError(t, err)
	noError(t, j.Close())
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	file = must(t, file, err)
	_, err = file.WriteString("{\"seq\":")
	noError(t, err)
	noError(t, file.Close())
	_, err = OpenJournal(path, key)
	verify(t, errors.Is(err, ErrRecovery), "accepted an incomplete journal tail")
}

func TestLiveModel(t *testing.T) {
	url := os.Getenv("MEMORY_TEST_MODEL_URL")
	if url == "" {
		t.Skip("set a local compatible real model")
	}
	ctx, s, _, _, _ := openTest(t, "MEMORY_TEST_MODEL_DSN")
	owner := actor(t, ctx, s, "model-test", "case-owner", "owner")
	app := actor(t, ctx, s, owner.Tenant, "assistant", "app")
	noError(t, s.SetConsent(ctx, owner, Consent{App: app.Actor, Purpose: "support", Read: true, Write: true, Model: true, Expires: time.Now().Add(time.Hour)}))
	model, err := NewModel(url, os.Getenv("MEMORY_TEST_MODEL"), "")
	model = must(t, model, err)
	records := []ModelReceipt{}
	inputs := []string{"I prefer receiving notifications by text message every time.", "My preferred language for future conversations is Spanish.", "I want all future measurements in metric units.", "I prefer the timezone UTC for future appointments.", "I need to send a card for an event."}
	for i, input := range inputs {
		source := write(t, ctx, s, app, Source{owner.Actor, "support", "model-evaluation", fmt.Sprintf("case-%d", i),
			time.Now().Add(-time.Second), "user", input})
		job, err := s.QueueEnrichment(ctx, app, owner.Actor, "support", source.Source, model)
		verify(t, err == nil && job.Status == "queued" && job.ID != "", "did not queue live inference: %v", err)
		worked, err := s.ProcessExtraction(ctx, model)
		verify(t, err == nil && worked, "did not process live inference: %v", err)
		result, err := s.ExtractionStatus(ctx, app, job.ID)
		verify(t, err == nil && result.Attempt == 1 && result.DurationMillis > 0, "did not recover the measured model receipt: %v", err)
		records = append(records, result)
		t.Logf("case %d: %s, %d ms, error=%v", i, result.Status, result.DurationMillis, err)
		if result.Status == "failed" {
			t.Log("provider response did not pass validation")
		}
	}
	verify(t, len(ask(t, ctx, s, app, Recall{Subject: owner.Actor, Purpose: "support"})) == 0, "model promoted an unconfirmed proposal")
	if path := os.Getenv("MEMORY_TEST_MODEL_EVIDENCE"); path != "" {
		pending, err := s.Pending(ctx, owner)
		pending = must(t, pending, err)
		result := map[string]any{"inputs": inputs, "results": records, "pending": pending, "auto_promotions": 0}
		encoded, err := json.MarshalIndent(result, "", "  ")
		encoded = must(t, encoded, err)
		noError(t, os.WriteFile(path, append(encoded, '\n'), 0600))
	}
}
