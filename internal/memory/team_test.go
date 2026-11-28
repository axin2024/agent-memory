package memory

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type teamFixture struct {
	t                       *testing.T
	ctx                     context.Context
	s                       *Service
	admin, author, reviewer Identity
	reader, outsider, app   Identity
	purpose                 string
	now                     time.Time
}

func setupTeam(t *testing.T, ctx context.Context, s *Service, tenant string) teamFixture {
	f := teamFixture{t: t, ctx: ctx, s: s, purpose: "operations", now: time.Now().UTC().Add(-time.Minute).Truncate(time.Second)}
	f.admin = actor(t, ctx, s, tenant, "maintainer", "owner")
	f.author = actor(t, ctx, s, tenant, "author", "owner")
	f.reviewer = actor(t, ctx, s, tenant, "reviewer", "owner")
	f.reader = actor(t, ctx, s, tenant, "reader", "owner")
	f.outsider = actor(t, ctx, s, "other-"+tenant, "author", "owner")
	f.app = actor(t, ctx, s, tenant, "external", "app")
	return f
}

func (f teamFixture) space(space, authorRole string) uint64 {
	f.t.Helper()
	value, err := f.s.SetTeamPolicy(f.ctx, f.admin, space, TeamPolicy{Label: space, Purpose: f.purpose, Active: true})
	revision := must(f.t, value, err).PolicyRev
	for _, member := range []struct {
		who  Identity
		role string
	}{{f.author, authorRole}, {f.reviewer, "reviewer"}, {f.reader, "reader"}} {
		updated, err := f.s.SetTeamMembership(f.ctx, f.admin, space, TeamMembership{Actor: member.who.Actor, Purpose: f.purpose,
			Role: member.role, Active: true, CanExport: member.role == "reader", PolicyRev: revision})
		revision = must(f.t, updated, err).PolicyRev
	}
	return revision
}

func (f teamFixture) nominate(space string, who Identity, external, sentence string, allowModel bool) TeamNomination {
	f.t.Helper()
	source := write(f.t, f.ctx, f.s, who, Source{Subject: who.Actor, Purpose: "personal", Session: "rehearsal",
		External: external, Occurred: f.now, Statement: sentence, Kind: "user"})
	out, err := f.s.Nominate(f.ctx, who, space, NominationInput{Purpose: f.purpose, Source: source.Source, GrantRev: 1, End: len(sentence),
		Expires: f.now.Add(10 * time.Hour), AllowModel: allowModel}, false)
	return must(f.t, out, err)
}

func (f teamFixture) draft(topic, applicability, supersedes string, nomination TeamNomination) TeamClaimInput {
	return TeamClaimInput{Purpose: f.purpose, Topic: topic, Applicability: applicability, ValidFrom: f.now, Supersedes: supersedes,
		NominationIDs: []string{nomination.ID}, GrantRevisions: []uint64{nomination.GrantRev}, Sentences: []string{nomination.Excerpt},
		EvidenceSpans: []EvidenceSpan{{Nomination: nomination.ID, Start: nomination.Start, End: nomination.End}}}
}

func (f teamFixture) create(space, id string, who Identity, input TeamClaimInput) TeamClaim {
	f.t.Helper()
	result, err := f.s.CreateTeamClaim(f.ctx, who, space, id, input, nil)
	return must(f.t, result, err)
}

func (f teamFixture) publish(claim TeamClaim) TeamClaim {
	f.t.Helper()
	vote := TeamVote{ClaimRev: claim.Rev, Decision: "approve", Reason: "Matched the authorized evidence"}
	first, err := f.s.VoteTeamClaim(f.ctx, f.reviewer, claim.Space, claim.Purpose, claim.ID, vote)
	verify(f.t, must(f.t, first, err).State == "awaiting_review", "first approval published claim %s", claim.ID)
	second, err := f.s.VoteTeamClaim(f.ctx, f.admin, claim.Space, claim.Purpose, claim.ID, vote)
	published := must(f.t, second, err)
	verify(f.t, published.State == "published" && len(published.ReviewVotes) == 2, "expected two-person publication: %+v", published)
	return published
}

func (f teamFixture) search(space, text string) TeamSearchResult {
	f.t.Helper()
	result, err := f.s.SearchTeam(f.ctx, f.reader, space, TeamQuery{Purpose: f.purpose, Query: text}, nil)
	return must(f.t, result, err)
}

func TestTeamLifecycle(t *testing.T) {
	ctx, s, db, path, key := openTest(t, "MEMORY_TEST_TEAM_DSN")
	f := setupTeam(t, ctx, s, "team-life")
	const north, south = "north", "south"
	f.space(north, "author")
	policyRev := f.space(south, "reader")
	phrase := "Aurora returns must be recorded at the northern station."
	n := f.nominate(north, f.author, "return-policy", phrase, true)
	preview, err := s.Nominate(ctx, f.author, north, n.NominationInput, true)
	verify(t, must(t, preview, err).Excerpt == phrase, "nomination preview did not match the requested bytes")
	again, err := s.Nominate(ctx, f.author, north, n.NominationInput, false)
	verify(t, must(t, again, err).ID == n.ID && again.Replay, "nomination retry made a second row")
	_, err = s.Nominate(ctx, f.author, south, n.NominationInput, false)
	verify(t, errors.Is(err, ErrDenied), "reader could nominate: %v", err)
	_, err = s.Nominate(ctx, f.outsider, north, n.NominationInput, false)
	verify(t, errors.Is(err, ErrMissing), "outside tenant saw the space: %v", err)
	grant(t, ctx, s, f.author, f.app.Actor, "personal", false)
	third := write(t, ctx, s, f.app, Source{Subject: f.author.Actor, Purpose: "personal", Session: "relay", External: "outside",
		Occurred: f.now, Statement: "Aurora returns are sent through the distribution hub.", Kind: "user"})
	external := n.NominationInput
	external.Source, external.End = third.Source, 6
	_, err = s.Nominate(ctx, f.author, north, external, false)
	verify(t, errors.Is(err, ErrInput), "third-party material was transferable: %v", err)

	input := f.draft("aurora-returns", "all stations", "", n)
	conflict := f.create(north, "original", f.author, input)
	retry := f.create(north, "original", f.author, input)
	verify(t, retry.Replay && retry.ID == conflict.ID, "claim retry returned another id")
	input.Applicability = "different request"
	_, err = s.CreateTeamClaim(ctx, f.author, north, "original", input, nil)
	verify(t, errors.Is(err, ErrConflict), "changed claim accepted same key: %v", err)
	_, err = s.TeamClaim(ctx, f.reader, north, f.purpose, conflict.ID)
	verify(t, errors.Is(err, ErrMissing), "reader could read an unreviewed claim: %v", err)
	_, err = s.VoteTeamClaim(ctx, f.author, north, f.purpose, conflict.ID, TeamVote{ClaimRev: 1, Decision: "approve", Reason: "Own draft"})
	verify(t, errors.Is(err, ErrDenied), "author could vote on own draft: %v", err)
	noError(t, s.AssignTeamReview(ctx, f.admin, north, f.purpose, conflict.ID, f.reviewer.Actor))
	disputed, err := s.VoteTeamClaim(ctx, f.reviewer, north, f.purpose, conflict.ID,
		TeamVote{ClaimRev: 1, Decision: "dispute", Reason: "Stations use distinct return instructions"})
	verify(t, must(t, disputed, err).State == "disputed", "conflict was not queued")
	onlySouth := f.nominate(north, f.author, "other-station", "Aurora returns for local stock go to the southern station.", true)
	first := f.publish(f.create(north, "north-rule", f.author, f.draft("aurora-returns", "northern station", conflict.ID, n)))
	second := f.publish(f.create(north, "south-rule", f.author, f.draft("aurora-returns", "southern station", conflict.ID, onlySouth)))
	old, err := s.TeamClaim(ctx, f.reader, north, f.purpose, conflict.ID)
	verify(t, must(t, old, err).State == "superseded" && len(old.Sentences) == 0, "old evidence lacked replacement marker: %+v", old)
	local := f.nominate(south, f.admin, "local-rule", "Aurora returns go directly to the inland depot.", true)
	southRule := f.create(south, "inland", f.admin, f.draft("aurora-returns", "inland depot", "", local))
	// The space owner authored this claim. Two other qualified reviewers must sign.
	member, err := s.SetTeamMembership(ctx, f.admin, south, TeamMembership{Actor: f.reader.Actor, Purpose: f.purpose,
		PolicyRev: policyRev, Active: true, Role: "reviewer", CanExport: true})
	policyRev = must(t, member, err).PolicyRev
	vote := TeamVote{ClaimRev: southRule.Rev, Decision: "approve", Reason: "Checked the inland receipt policy"}
	one, err := s.VoteTeamClaim(ctx, f.reader, south, f.purpose, southRule.ID, vote)
	verify(t, must(t, one, err).State == "awaiting_review", "one reviewer published")
	two, err := s.VoteTeamClaim(ctx, f.reviewer, south, f.purpose, southRule.ID, vote)
	verify(t, must(t, two, err).State == "published", "two distinct reviewers did not publish")
	northResults := f.search(north, "Aurora returns")
	verify(t, len(northResults.Hits) == 2, "expected both regional rules: %+v", northResults)
	southResults := f.search(south, "Aurora returns")
	verify(t, len(southResults.Hits) == 1 && southResults.Hits[0].ID == southRule.ID, "cross-space answer: %+v", southResults)
	export := TeamQuery{Purpose: f.purpose, Query: "Aurora returns", Export: true}
	exported, err := s.SearchTeam(ctx, f.reader, south, export, nil)
	verify(t, len(must(t, exported, err).Hits) == 1, "expected one scoped export")
	_, err = s.SetTeamMembership(ctx, f.admin, south, TeamMembership{Actor: f.reader.Actor, Purpose: f.purpose, PolicyRev: policyRev, Role: "reviewer"})
	noError(t, err)
	_, err = s.SearchTeam(ctx, f.reader, south, export, nil)
	verify(t, errors.Is(err, ErrMissing), "leaver could reuse current credential to export: %v", err)

	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop",
			"message": map[string]string{"content": `{"claims":[{"text":"Aurora returns are always refunded.","citations":[]}]}`}}}})
	}))
	t.Cleanup(endpoint.Close)
	replay, err := NewModel(endpoint.URL+"/v1/chat/completions", "frozen-fixture", "")
	model := must(t, replay, err)
	unsupported := f.draft("unsupported", "all stations", "", n)
	unsupported.UseModel, unsupported.Sentences, unsupported.EvidenceSpans = true, nil, nil
	modelResult, err := s.CreateTeamClaim(ctx, f.author, north, "model-test", unsupported, model)
	verify(t, must(t, modelResult, err).State == "disputed" && modelResult.ReviewReason == "model-evidence-missing" &&
		modelResult.Model != nil && len(modelResult.Sentences) == 0, "unsupported model conclusion advanced: %+v", modelResult)
	_, err = s.VoteTeamClaim(ctx, f.admin, north, f.purpose, modelResult.ID, TeamVote{ClaimRev: 1, Decision: "approve", Reason: "Check guard"})
	verify(t, errors.Is(err, ErrConflict), "could approve a disputed model claim: %v", err)
	// Use a separate source to keep the recall fan-out exactly three.
	side := f.nominate(north, f.author, "recall-fanout", "Violet crates are collected at the northern loading bay.", false)
	var dependent []string
	for _, applicability := range []string{"weekday", "weekend", "holiday"} {
		item := f.publish(f.create(north, applicability, f.author, f.draft("violet-crates", applicability, "", side)))
		dependent = append(dependent, item.ID)
	}
	restoreDSN := os.Getenv("MEMORY_TEST_TEAM_RESTORE_DSN")
	if restoreDSN == "" {
		t.Fatal("set MEMORY_TEST_TEAM_RESTORE_DSN to a second disposable schema")
	}
	destination, err := Connect(restoreDSN)
	standby := must(t, destination, err)
	defer standby.Close()
	copySnapshot(t, ctx, db, standby)
	impact, err := s.PreviewTeamWithdrawal(ctx, f.author, side.ID)
	verify(t, must(t, impact, err).AffectedCount == 3, "withdrawal preview omitted a dependency: %+v", impact)
	result, err := s.WithdrawTeamNomination(ctx, f.author, side.ID, "revoke-violet")
	verify(t, len(must(t, result, err).Affected) == 3 && result.AffectedCount == 3 && !result.Processed && result.Watermark > 0,
		"expected authoritative withdrawal before reindex: %+v", result)
	for _, hit := range f.search(north, "Violet crates").Hits {
		verify(t, !slices.Contains(dependent, hit.ID), "stale postings returned a withdrawn claim")
	}
	fallback, err := s.SearchTeam(ctx, f.reader, north, TeamQuery{Purpose: f.purpose, Query: "Aurora returns", Mode: "hybrid"}, nil)
	verify(t, must(t, fallback, err).Mode == "keywords" && len(fallback.Hits) == 2 &&
		slices.ContainsFunc(fallback.Hits, func(hit TeamHit) bool { return hit.ID == first.ID || hit.ID == second.ID }), "unaffected manual claims disappeared: %+v", fallback)
	reindexed, err := s.ReindexTeam(ctx, f.admin, north, f.purpose, nil)
	verify(t, must(t, reindexed, err).Indexed == result.Watermark, "index watermark did not advance: %+v", reindexed)
	readback, err := s.TeamWithdrawalStatus(ctx, f.author, result.ID)
	verify(t, must(t, readback, err).Processed, "withdrawal not fully indexed")
	noError(t, s.Close())
	restoredService, err := Open(ctx, standby, path, key)
	restored := must(t, restoredService, err)
	defer restored.Close()
	f.s = restored
	verify(t, len(f.search(north, "Violet crates").Hits) == 0, "old snapshot leaked withheld knowledge")
	readback, err = restored.TeamWithdrawalStatus(ctx, f.author, result.ID)
	verify(t, must(t, readback, err).Watermark == result.Watermark, "old snapshot missed withdrawal journal")
	t.Logf("verified two scoped results, one refused model fixture and three dependent withdrawals at sequence %d", result.Watermark)
	writeTeamArtifact(t, "lifecycle.json", map[string]any{"reviewed_claims": []string{first.ID, second.ID, southRule.ID},
		"unsupported_model": modelResult, "withdrawal": result, "after_index": reindexed, "restored_withdrawal": readback,
		"models": "one replay fixture; manual operations otherwise", "withdrawn_sources_returned": 0, "restore_leaks": 0})
}

func writeTeamArtifact(t *testing.T, name string, value any) {
	t.Helper()
	folder := strings.TrimSpace(os.Getenv("MEMORY_TEAM_EVIDENCE"))
	if folder == "" {
		return
	}
	noError(t, os.MkdirAll(folder, 0700))
	data, err := json.MarshalIndent(value, "", "  ")
	noError(t, err)
	noError(t, os.WriteFile(filepath.Join(folder, name), append(data, '\n'), 0600))
}
