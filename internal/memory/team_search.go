package memory

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"unicode"
)

type TeamQuery struct {
	Purpose string    `json:"purpose"`
	Query   string    `json:"query"`
	AsOf    time.Time `json:"as_of,omitempty"`
	Mode    string    `json:"mode,omitempty"`
	Limit   int       `json:"limit,omitempty"`
	Export  bool      `json:"-"`
}

type TeamHit struct {
	TeamClaim
	Score       float64 `json:"score"`
	Explanation string  `json:"explanation"`
}

type TeamSearchResult struct {
	Hits          []TeamHit      `json:"hits"`
	Mode          string         `json:"mode"`
	Model         *TeamModelNote `json:"model_provenance,omitempty"`
	PolicyRev     uint64         `json:"policy_rev"`
	WithdrawalSeq uint64         `json:"withdrawal_seq"`
	IndexSeq      uint64         `json:"indexed_seq"`
	Suppressed    int            `json:"suppressed"`
	Refusal       string         `json:"refusal,omitempty"`
}

type teamRank struct {
	id      string
	lexical float64
	vector  float64
}

func termsForIndex(input string, limit int) []string {
	var result []string
	seen := make(map[string]bool)
	for _, word := range strings.FieldsFunc(strings.ToLower(input), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) }) {
		if len(word) < 2 || len(word) > 64 || seen[word] || slices.Contains([]string{"the", "of", "for", "and", "to", "in", "do", "is", "how", "we", "are", "our", "with", "what", "when", "which", "can"}, word) {
			continue
		}
		seen[word] = true
		result = append(result, word)
		if len(result) == limit {
			break
		}
	}
	return result
}

func indexTeamTerms(ctx context.Context, tx *sql.Tx, tenant string, claim TeamClaim, content string) error {
	terms := termsForIndex(content, 72)
	if len(terms) == 0 {
		return nil
	}
	var args []any
	for _, term := range terms {
		args = append(args, tenant, claim.Space, claim.Purpose, term, claim.ID, 1)
	}
	values := strings.TrimSuffix(strings.Repeat("(?,?,?,?,?,?),", len(terms)), ",")
	_, err := tx.ExecContext(ctx, `INSERT INTO team_search_terms(tenant,space_id,purpose,term,claim_id,weight) VALUES `+values, args...)
	return err
}

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

func lexicalTeamRanks(ctx context.Context, tx *sql.Tx, tenant string, space string, in TeamQuery, ranks map[string]teamRank) error {
	terms := termsForIndex(in.Query, 12)
	if len(terms) == 0 {
		return nil
	}
	// Read bounded postings from the scoped index. Score less frequent terms higher.
	for _, term := range terms {
		rows, err := tx.QueryContext(ctx, `SELECT claim_id FROM team_search_terms
			WHERE tenant=? AND space_id=? AND purpose=? AND term=? ORDER BY claim_id LIMIT 200`, tenant, space, in.Purpose, term)
		if err != nil {
			return err
		}
		ids := make([]string, 0, 32)
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, id := range ids {
			rank := ranks[id]
			rank.id = id
			rank.lexical += 1 / math.Sqrt(float64(len(ids)))
			ranks[id] = rank
		}
	}
	return nil
}

const vectorBits = 10

func teamVectorBucket(vector []float32) uint32 {
	var result uint32
	for bit := range vectorBits {
		var projection float32
		for index, value := range vector {
			seed := uint64(index+1)*0x9e3779b97f4a7c15 ^ uint64(bit+1)*0xbf58476d1ce4e5b9
			seed ^= seed >> 29
			seed *= 0x94d049bb133111eb
			if seed>>63 == 0 {
				projection += value
			} else {
				projection -= value
			}
		}
		if projection >= 0 {
			result |= 1 << bit
		}
	}
	return result
}

func vectorBytes(vector []float32) []byte {
	raw := make([]byte, 0, len(vector)*4)
	for _, value := range vector {
		raw = binary.LittleEndian.AppendUint32(raw, math.Float32bits(value))
	}
	return raw
}

func cosineBytes(vector []float32, raw []byte) (float64, bool) {
	if len(vector)*4 != len(raw) {
		return 0, false
	}
	var score float64
	for index, value := range vector {
		score += float64(value) * float64(math.Float32frombits(binary.LittleEndian.Uint32(raw[4*index:])))
	}
	return score, !math.IsNaN(score) && !math.IsInf(score, 0)
}

func teamVectorRanks(ctx context.Context, tx *sql.Tx, tenant, space, model string, in TeamQuery, vector []float32, ranks map[string]teamRank) error {
	args := []any{tenant, space, in.Purpose, model}
	base := ` FROM team_vectors WHERE tenant=? AND space_id=? AND purpose=? AND model_hash=?`
	var number int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT 1`+base+` LIMIT 513) subset`, args...).Scan(&number); err != nil {
		return err
	}
	if number > 512 {
		bucket := teamVectorBucket(vector)
		args = append(args, bucket)
		for bit := range vectorBits {
			args = append(args, bucket^(1<<bit))
		}
		base += ` AND bucket IN (` + placeholders(1+vectorBits) + `)`
	}
	rows, err := tx.QueryContext(ctx, `SELECT claim_id,vector`+base+` ORDER BY bucket,claim_id LIMIT 768`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var raw []byte
		if err = rows.Scan(&id, &raw); err != nil {
			return err
		}
		score, valid := cosineBytes(vector, raw)
		if valid && score >= 0.36 {
			rank := ranks[id]
			rank.id, rank.vector = id, score
			ranks[id] = rank
		}
	}
	return rows.Err()
}

func teamScore(rank teamRank) float64 { return math.Min(1, rank.lexical)*0.43 + rank.vector*0.57 }

func (s *Service) SearchTeam(ctx context.Context, who Identity, space string, in TeamQuery, embed *TeamEmbedder) (TeamSearchResult, error) {
	result := TeamSearchResult{Hits: []TeamHit{}, Mode: "keywords"}
	if !textOK(in.Query, 1000) || !slices.Contains([]string{"", "keywords", "hybrid"}, in.Mode) || in.Limit < 0 || in.Limit > 20 {
		return result, ErrInput
	}
	if in.Limit == 0 {
		in.Limit = 5
	}
	if in.AsOf.IsZero() {
		in.AsOf = time.Now().UTC()
	}
	if in.AsOf.After(time.Now().Add(time.Second)) {
		return result, ErrInput
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.unavailable {
		return result, ErrRecovery
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelReadCommitted})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	if err := s.teamReadGuard(ctx, tx); err != nil {
		return result, err
	}
	access, err := teamAccess(ctx, tx, who, space, in.Purpose)
	if err != nil {
		return result, err
	}
	if in.Export && !access.CanExport {
		return result, ErrDenied
	}
	result.PolicyRev = access.PolicyRev
	if err := tx.QueryRowContext(ctx, `SELECT withdrawal_seq,indexed_seq FROM team_watermarks WHERE tenant=? AND space_id=?`,
		who.Tenant, space).Scan(&result.WithdrawalSeq, &result.IndexSeq); err != nil {
		return result, err
	}
	ranks := make(map[string]teamRank)
	if err = lexicalTeamRanks(ctx, tx, who.Tenant, space, in, ranks); err != nil {
		return result, err
	}
	if in.Mode == "hybrid" && embed != nil && !sensitive.MatchString(in.Query) {
		vectors, note, err := embed.vectors(ctx, []string{"Instruct: Retrieve team knowledge that directly answers the request\nQuery: " + in.Query})
		if err == nil {
			result.Mode, result.Model = "hybrid", note
			if err = teamVectorRanks(ctx, tx, who.Tenant, space, embed.ID, in, vectors[0], ranks); err != nil {
				return result, err
			}
		}
	}
	ordered := make([]teamRank, 0, len(ranks))
	for _, rank := range ranks {
		ordered = append(ordered, rank)
	}
	slices.SortFunc(ordered, func(a, b teamRank) int {
		if rankA, rankB := teamScore(a), teamScore(b); rankA != rankB {
			if rankA > rankB {
				return -1
			}
			return 1
		}
		return strings.Compare(a.id, b.id)
	})
	for _, rank := range ordered[:min(60, len(ordered))] {
		var valid bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM team_claims WHERE tenant=? AND space_id=? AND purpose=?
			AND claim_id=? AND review_state='published' AND valid_from<=? AND published<=? AND expires>UTC_TIMESTAMP(6))`,
			who.Tenant, space, in.Purpose, rank.id, in.AsOf, in.AsOf).Scan(&valid); err != nil {
			return result, err
		}
		if !valid {
			result.Suppressed++
			continue
		}
		claim, err := teamClaimView(ctx, tx, who.Tenant, space, in.Purpose, rank.id, false)
		if err != nil && !errors.Is(err, ErrMissing) {
			return result, err
		}
		if err != nil || claim.State != "published" || len(claim.Citations) == 0 {
			result.Suppressed++
			continue
		}
		explanation := "关键词"
		if rank.vector > 0 && rank.lexical > 0 {
			explanation = "关键词与语义相似度"
		} else if rank.vector > 0 {
			explanation = "语义相似度"
		}
		result.Hits = append(result.Hits, TeamHit{TeamClaim: claim, Score: math.Round(teamScore(rank)*10000) / 10000, Explanation: explanation})
		if len(result.Hits) == in.Limit {
			break
		}
	}
	if len(result.Hits) == 0 {
		result.Refusal = "no-current-supported-knowledge"
	}
	if err := s.teamReadGuard(ctx, tx); err != nil {
		return TeamSearchResult{}, err
	}
	return result, tx.Commit()
}

func (s *Service) teamReadGuard(ctx context.Context, tx *sql.Tx) error {
	var held bool
	err := tx.QueryRowContext(ctx, "SELECT COALESCE(IS_USED_LOCK(?)=?,0)", s.lockName, s.guardID).Scan(&held)
	if err != nil {
		return err
	}
	if !held {
		return ErrRecovery
	}
	return nil
}

type TeamReindex struct {
	Processed int            `json:"processed"`
	Remaining bool           `json:"remaining"`
	Indexed   uint64         `json:"indexed_seq"`
	Model     *TeamModelNote `json:"model_provenance,omitempty"`
}

func (s *Service) ReindexTeam(ctx context.Context, who Identity, space, purpose string, embed *TeamEmbedder) (TeamReindex, error) {
	var result TeamReindex
	if err := s.lock(ctx); err != nil {
		return result, err
	}
	defer s.mu.Unlock()
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := teamAccess(ctx, tx, who, space, purpose, "maintainer"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE team_claims SET content=NULL,applicability='',review_state='withdrawn',
			review_reason='evidence-expired',review_queue=FALSE WHERE tenant=? AND space_id=? AND purpose=?
			AND expires<=UTC_TIMESTAMP(6) AND review_state NOT IN ('withdrawn','superseded')`, who.Tenant, space, purpose); err != nil {
			return err
		}
		for _, table := range []string{"team_search_terms", "team_vectors"} {
			if _, err := tx.ExecContext(ctx, `DELETE i FROM `+table+` i JOIN team_claims c ON
				(c.tenant,c.space_id,c.claim_id)=(i.tenant,i.space_id,i.claim_id) WHERE i.tenant=? AND i.space_id=?
				AND (c.review_state<>'published' OR c.expires<=UTC_TIMESTAMP(6))`, who.Tenant, space); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE team_watermarks SET indexed_seq=withdrawal_seq WHERE tenant=? AND space_id=?`, who.Tenant, space); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT indexed_seq FROM team_watermarks WHERE tenant=? AND space_id=?`,
			who.Tenant, space).Scan(&result.Indexed); err != nil || embed == nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT c.claim_id FROM team_claims c WHERE c.tenant=? AND c.space_id=? AND c.purpose=?
			AND c.review_state='published' AND c.expires>UTC_TIMESTAMP(6) AND NOT EXISTS (SELECT 1 FROM team_vectors v
			WHERE v.tenant=c.tenant AND v.space_id=c.space_id AND v.purpose=c.purpose AND v.model_hash=? AND v.claim_id=c.claim_id)
			AND NOT EXISTS (SELECT 1 FROM team_claim_evidence e JOIN team_nominations n
			ON (n.tenant,n.space_id,n.nomination)=(e.tenant,e.space_id,e.nomination) WHERE e.tenant=c.tenant AND e.space_id=c.space_id
			AND e.claim_id=c.claim_id AND (NOT n.allow_model OR n.revoked_seq IS NOT NULL OR n.expires<=UTC_TIMESTAMP(6)))
			ORDER BY c.claim_id LIMIT 17`, who.Tenant, space, purpose, embed.ID)
		if err != nil {
			return err
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		result.Remaining = len(ids) > 16
		var indexed []string
		var texts []string
		for _, id := range ids[:min(16, len(ids))] {
			claim, err := teamClaimView(ctx, tx, who.Tenant, space, purpose, id, false)
			if err != nil {
				return err
			}
			if claim.State != "published" {
				return fmt.Errorf("%w: invalid claim reached embedding queue", ErrRecovery)
			}
			indexed = append(indexed, id)
			texts = append(texts, claim.Topic+". "+claim.Applicability+". "+strings.Join(claim.Sentences, " "))
		}
		if len(texts) == 0 {
			return nil
		}
		vectors, note, err := embed.vectors(ctx, texts)
		if err != nil {
			return err
		}
		for index, vector := range vectors {
			if _, err := tx.ExecContext(ctx, `INSERT INTO team_vectors VALUES(?,?,?,?,?,?,?)`,
				who.Tenant, space, purpose, embed.ID, teamVectorBucket(vector), indexed[index], vectorBytes(vector)); err != nil {
				return err
			}
		}
		result.Processed, result.Model = len(vectors), note
		return nil
	})
	return result, err
}
