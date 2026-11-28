package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func insertTeamFixture(ctx context.Context, tx *sql.Tx, header string, arity int, args []any) error {
	row := "(" + placeholders(arity) + "),"
	values := strings.TrimSuffix(strings.Repeat(row, len(args)/arity), ",")
	_, err := tx.ExecContext(ctx, header+" VALUES "+values, args...)
	return err
}

func seedTeamCapacity(f teamFixture, db *sql.DB, spaces []string, count int) {
	f.t.Helper()
	expiry := f.now.Add(12 * time.Hour)
	scope := f.s.scope(f.author.Tenant, f.author.Actor)
	for from := 0; from < count; from += 500 {
		var sources, nominations, claims, edges, terms []any
		for id := from; id < min(from+500, count); id++ {
			space := spaces[id%len(spaces)]
			topic := fmt.Sprintf("component-%06d", id)
			content := fmt.Sprintf("Inspection component%06d must be recorded in the workshop log.", id)
			claim, nomination := fmt.Sprintf("%032x", 1+id), fmt.Sprintf("%032x", 1+count+id)
			source := hash("capacity:" + topic)
			sources = append(sources, scope, source, hash(content), f.author.Actor, "personal", "load-fixture", topic, f.now, expiry, "user", content)
			var watermark any
			state := "published"
			var body any = encode([]string{content})
			if id%20 == 0 {
				watermark, state, body = 1, "withdrawn", nil
			}
			nominations = append(nominations, f.author.Tenant, space, f.purpose, nomination, f.author.Actor, scope, source, hash(content), 1,
				0, len(content), false, expiry, watermark)
			claims = append(claims, f.author.Tenant, space, claim, f.purpose, 1, f.author.Actor, topic, "workshop",
				body, f.now, expiry, state, f.now, watermark, topic, hash(content))
			edges = append(edges, f.author.Tenant, space, claim, 0, nomination, 0, len(content))
			for _, term := range termsForIndex(content, 72) {
				terms = append(terms, f.author.Tenant, space, f.purpose, term, claim, 1)
			}
		}
		rowset := []struct {
			header string
			arity  int
			args   []any
		}{
			{`INSERT INTO sources (scope_hash,source_hash,digest,app,purpose,session_id,external_id,occurred,expires,kind,statement)`, 11, sources},
			{`INSERT INTO team_nominations (tenant,space_id,purpose,nomination,actor,source_scope,source_hash,source_digest,
				grant_rev,start_byte,end_byte,allow_model,expires,revoked_seq)`, 14, nominations},
			{`INSERT INTO team_claims (tenant,space_id,claim_id,purpose,claim_rev,author,topic,applicability,content,
				valid_from,expires,review_state,published,withdrawn_seq,idempotency_key,input_digest)`, 16, claims},
			{"INSERT INTO team_claim_evidence", 7, edges},
			{"INSERT INTO team_search_terms", 6, terms},
		}
		transaction, err := db.BeginTx(f.ctx, nil)
		tx := must(f.t, transaction, err)
		for _, batch := range rowset {
			if err := insertTeamFixture(f.ctx, tx, batch.header, batch.arity, batch.args); err != nil {
				tx.Rollback()
				f.t.Fatal(err)
			}
		}
		noError(f.t, tx.Commit())
	}
	_, err := db.ExecContext(f.ctx, "UPDATE team_watermarks SET withdrawal_seq=1 WHERE tenant=?", f.author.Tenant)
	noError(f.t, err)
}

type capacityResult struct {
	Item     int     `json:"item"`
	Actor    string  `json:"actor"`
	Space    string  `json:"space"`
	Mode     string  `json:"mode"`
	Duration float64 `json:"duration_ms"`
	Hits     int     `json:"hits"`
	Leaks    int     `json:"leaks"`
	Outcome  string  `json:"outcome"`
	Error    string  `json:"error,omitempty"`
}

func TestTeamCapacity(t *testing.T) {
	if os.Getenv("MEMORY_TEAM_LOAD") != "1" {
		t.Skip("set MEMORY_TEAM_LOAD=1 for the 100,000-row capacity rehearsal")
	}
	ctx, s, db, _, _ := openTest(t, "MEMORY_TEST_TEAM_DSN")
	f := setupTeam(t, ctx, s, "team-capacity")
	const records, requests, concurrency = 100000, 1000, 8
	spaces := []string{"warehouse-west", "warehouse-east"}
	members := []Identity{f.reader, f.author, f.reviewer}
	for i := 0; i < 5; i++ {
		members = append(members, actor(t, ctx, s, f.admin.Tenant, fmt.Sprintf("operator-%d", i), "owner"))
	}
	for _, space := range spaces {
		revision := f.space(space, "author")
		for _, member := range members[3:] {
			membership, err := s.SetTeamMembership(ctx, f.admin, space, TeamMembership{Actor: member.Actor, Purpose: f.purpose,
				Role: "reader", Active: true, PolicyRev: revision})
			revision = must(t, membership, err).PolicyRev
		}
	}
	seeding := time.Now()
	seedTeamCapacity(f, db, spaces, records)
	seedTime := time.Since(seeding).Milliseconds()
	var written int
	noError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM team_claims WHERE tenant=?", f.author.Tenant).Scan(&written))
	verify(t, written == records, "expected exactly %d seeded records, got %d", records, written)
	query := func(i int) capacityResult {
		index := (i*97 + 20) % records
		space := spaces[index%2]
		identity, status := members[i%len(members)], "supported"
		switch {
		case i%10 == 1:
			space, status = spaces[1-index%2], "wrong-space"
		case i%10 == 2:
			identity, status = f.outsider, "outside-tenant"
		case i%10 == 3:
			index, space, status = index/20*20, spaces[0], "withdrawn"
		case index%20 == 0:
			status = "withdrawn"
		}
		result := capacityResult{Item: index, Space: space, Actor: identity.Actor, Mode: status}
		start := time.Now()
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		out, err := s.SearchTeam(callCtx, identity, space, TeamQuery{Purpose: f.purpose, Query: fmt.Sprintf("component%06d", index)}, nil)
		result.Duration = float64(time.Since(start).Nanoseconds()) / 1e6
		result.Hits = len(out.Hits)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || callCtx.Err() != nil {
				result.Outcome = "timeout"
			} else if status == "outside-tenant" && errors.Is(err, ErrMissing) {
				result.Outcome = "denied"
			} else {
				result.Outcome = "error"
				result.Error = err.Error()
			}
			return result
		}
		wanted := fmt.Sprintf("%032x", index+1)
		for _, hit := range out.Hits {
			if status != "supported" || hit.ID != wanted || len(hit.Citations) != 1 ||
				hit.Citations[0].Source != hash("capacity:"+fmt.Sprintf("component-%06d", index)) {
				result.Leaks++
			}
		}
		switch {
		case status == "supported" && result.Hits == 1:
			result.Outcome = "matched"
		case status != "outside-tenant" && status != "supported" && result.Hits == 0:
			result.Outcome = "refused"
		default:
			result.Outcome = "unexpected"
		}
		return result
	}
	warmStarted := time.Now()
	for i := range 100 {
		_ = query(i + requests)
	}
	warmTime := time.Since(warmStarted)
	start := time.Now()
	var wg sync.WaitGroup
	results := make([]capacityResult, requests)
	for worker := range concurrency {
		wg.Go(func() {
			for i := worker; i < requests; i += concurrency {
				results[i] = query(i)
			}
		})
	}
	wg.Wait()
	elapsed := time.Since(start)
	observations := make([]float64, 0, requests)
	outcomes := map[string]int{}
	leaks := 0
	for _, result := range results {
		observations = append(observations, result.Duration)
		outcomes[result.Outcome]++
		leaks += result.Leaks
	}
	slices.Sort(observations)
	p95 := observations[(requests*95+99)/100-1]
	var mysql string
	noError(t, db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&mysql))
	writeTeamArtifact(t, "capacity.json", map[string]any{"fixture": "synthetic indexed MySQL records; actual service keyword retrieval", "go": runtime.Version(),
		"os": runtime.GOOS, "arch": runtime.GOARCH, "logical_cpus": runtime.NumCPU(), "mysql": mysql, "records": records,
		"seed_ms": seedTime, "warmup_requests": 100, "warmup_ms": warmTime.Milliseconds(), "requests": requests, "parallel": concurrency,
		"actors": len(members) + 1, "sample_ms": elapsed.Milliseconds(), "p95_ms": p95, "outcomes": outcomes, "leaks": leaks, "rows": results})
	t.Logf("rows %d requests %d P95 %.3f ms outcomes %v leaks %d", written, requests, p95, outcomes, leaks)
	verify(t, p95 <= 300 && leaks == 0 && outcomes["matched"]+outcomes["refused"]+outcomes["denied"] == requests,
		"capacity acceptance failed")
}
