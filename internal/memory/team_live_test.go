package memory

import (
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"
)

func TestTeamLive(t *testing.T) {
	target := os.Getenv("MEMORY_TEAM_LIVE_EMBED_URL")
	if target == "" {
		t.Skip("set MEMORY_TEAM_LIVE_EMBED_URL to the real local embedding backend")
	}
	name, sha := os.Getenv("MEMORY_TEAM_LIVE_EMBED_NAME"), os.Getenv("MEMORY_TEAM_LIVE_EMBED_SHA256")
	config, err := NewTeamEmbedder(target, name, "", sha)
	embed := must(t, config, err)
	ctx, s, db, _, _ := openTest(t, "MEMORY_TEST_TEAM_DSN")
	f := setupTeam(t, ctx, s, "team-live")
	const main, sibling = "workshop", "neighbour"
	f.space(main, "author")
	f.space(sibling, "author")
	products := []string{
		"Cedar", "Maple", "Birch", "Spruce", "Yew", "Bamboo", "Juniper", "Willow", "Ash", "Elm",
		"Lark", "Raven", "Crane", "Heron", "Wren", "Robin", "Kestrel", "Egret", "Owl", "Finch",
		"Amber", "Indigo", "Violet", "Copper", "Silver", "Coral", "Azure", "Olive", "Ivory", "Slate",
		"Comet", "Meteor", "Eclipse", "Orbit", "Nebula", "Pulsar", "Aurora", "Zenith", "Solstice", "Equinox",
		"Quartz", "Basalt", "Marble", "Granite", "Jasper", "Flint", "Obsidian", "Onyx", "Agate", "Mica",
	}
	scenarios := []struct{ noun, fact, direct, paraphrase string }{
		{"pump", "pump seals are replaced in the intake bay.", "where are pump seals replaced?", "which workshop renews the seals?"},
		{"coil", "coil inspection starts at the eastern bench.", "where does coil inspection start?", "where should a technician examine the winding?"},
		{"crate", "crate returns go through the southern loading dock.", "where do crate returns go?", "which dock handles returned containers?"},
		{"trolley", "trolley calibration is carried out on the marked steel platform.", "where is trolley calibration carried out?", "where should staff adjust the equipment gauge?"},
		{"valve", "valve samples are preserved in the green cabinet.", "where are valve samples preserved?", "where do operators keep the testing specimens?"},
		{"filament", "filament stock is counted by the night shift.", "who counts filament stock?", "who checks the thread inventory after dark?"},
		{"sensor", "sensor alarms are checked with the handheld meter.", "how are sensor alarms checked?", "which device confirms an instrument alert?"},
		{"relay", "relay replacements are authorized by the duty engineer.", "who authorizes relay replacements?", "which staff member approves changing the electrical component?"},
		{"coupling", "coupling defects are recorded in the blue ledger.", "where are coupling defects recorded?", "where should the connector faults be documented?"},
		{"filter", "filter cartridges dry on the ventilated rack.", "where do filter cartridges dry?", "where are cleaned inserts left to air out?"},
	}
	input := TeamEvaluationInput{Purpose: f.purpose}
	var revoked []TeamNomination
	for i, product := range products {
		scenario := scenarios[i/5]
		id := fmt.Sprintf("supported-%02d", i)
		n := f.nominate(main, f.author, id, product+" "+scenario.fact, true)
		claim := f.publish(f.create(main, id, f.author, f.draft(id, product+" equipment only", "", n)))
		question := scenario.direct
		if i%5 == 3 {
			question = scenario.paraphrase
		}
		input.Cases = append(input.Cases, TeamEvaluationCase{ID: id, Group: "supported", Question: "For " + product + ", " + question, Expected: claim.ID, Source: n.Source})
		if i >= 25 {
			continue
		}
		cross := fmt.Sprintf("nearby-%02d", i)
		n = f.nominate(sibling, f.author, cross, product+" "+scenario.noun+" equipment must be taken to the restricted annex.", true)
		f.publish(f.create(sibling, cross, f.author, f.draft(cross, product+" project neighbour", "", n)))
		input.Cases = append(input.Cases, TeamEvaluationCase{ID: cross, Group: "cross-project",
			Question: "Does " + product + " " + scenario.noun + " equipment go to the restricted annex?", Forbidden: []string{n.Source}})
		deleted := fmt.Sprintf("withdrawn-%02d", i)
		n = f.nominate(main, f.author, deleted, product+" "+scenario.noun+" materials had a private exception at the closed satellite facility.", true)
		f.publish(f.create(main, deleted, f.author, f.draft(deleted, product+" closed satellite facility", "", n)))
		revoked = append(revoked, n)
		input.Cases = append(input.Cases, TeamEvaluationCase{ID: deleted, Group: "withdrawn",
			Question: "What was the " + product + " " + scenario.noun + " exception at the closed satellite facility?", Forbidden: []string{n.Source}})
	}
	startedIndex := time.Now()
	indexed := 0
	for _, space := range []string{main, sibling} {
		for {
			result, err := s.ReindexTeam(ctx, f.admin, space, f.purpose, embed)
			batch := must(t, result, err)
			indexed += batch.Processed
			if !batch.Remaining {
				break
			}
		}
	}
	indexTime := time.Since(startedIndex).Milliseconds()
	verify(t, indexed == 100, "expected actual embeddings for all claims: %d", indexed)
	for i, n := range revoked {
		out, err := s.WithdrawTeamNomination(ctx, f.author, n.ID, fmt.Sprintf("withdraw-%02d", i))
		verify(t, len(must(t, out, err).Affected) == 1 && !out.Processed, "expected stale vector after immediate withdrawal")
	}
	stamp := time.Now().UTC()
	for i := range input.Cases {
		input.Cases[i].AsOf = stamp
	}
	startedEvaluation := time.Now()
	out, err := s.EvaluateTeam(ctx, f.admin, main, input, embed)
	report := must(t, out, err)
	evaluationTime := time.Since(startedEvaluation).Milliseconds()
	stats := report.Results["hybrid"]
	var version string
	noError(t, db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version))
	stored, err := s.TeamEvaluationReceipt(ctx, f.admin, main, f.purpose, report.ID)
	verify(t, must(t, stored, err).Digest == report.Digest && len(stored.Rows) == 200, "could not read the exact recorded evaluation")
	writeTeamArtifact(t, "live-evaluation.json", map[string]any{"backend": "real loopback embedding model", "name": name, "weights_sha256": sha,
		"engine": os.Getenv("MEMORY_TEAM_LIVE_ENGINE"), "mysql": version, "go": runtime.Version(), "platform": runtime.GOOS + "/" + runtime.GOARCH,
		"input": input, "receipt": report, "indexed_before_revocation": indexed, "index_ms": indexTime, "evaluation_ms": evaluationTime,
		"stale_vectors_retained": true, "withdrawal_count": len(revoked), "embed_fingerprint": embed.ID})
	t.Logf("actual model %s: hybrid %+v; keywords %+v", name, stats, report.Results["keywords"])
	verify(t, stats.TopFiveCorrect >= 45 && stats.Leaks == 0 && stats.ModelQueries == 100 && stats.Fallbacks == 0,
		"real-model acceptance failed: %+v", stats)
	verify(t, report.Results["keywords"].Leaks == 0, "keyword results leaked restricted evidence")
}
