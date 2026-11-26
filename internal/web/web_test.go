package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axin2024/agent-memory/internal/memory"
)

func TestHTTP(t *testing.T) {
	dsn := os.Getenv("MEMORY_TEST_WEB_DSN")
	if dsn == "" {
		t.Skip("set MEMORY_TEST_WEB_DSN to a disposable schema")
	}
	ctx := t.Context()
	db, err := memory.Connect(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := memory.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "journal")
	key := make([]byte, 32)
	if err := memory.InitJournal(path, key); err != nil {
		t.Fatal(err)
	}
	service, err := memory.Open(ctx, db, path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	owner, err := service.Provision(ctx, "sandbox", "riley", "owner", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	app, err := service.Provision(ctx, "sandbox", "assistant", "app", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	otherUser, err := service.Provision(ctx, "sandbox", "someone-else", "owner", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	otherTenant, err := service.Provision(ctx, "isolated", "riley", "owner", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var retries atomic.Int32
	waiting, release := make(chan struct{}), make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		value := map[string]string{"slot": "language", "value": "French", "evidence": "use French"}
		if strings.Contains(string(body), "rare fixture measurements") {
			if retries.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			value = map[string]string{"slot": "none", "value": "", "evidence": ""}
		}
		if strings.Contains(string(body), "select the Europe timezone") {
			close(waiting)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			value = map[string]string{"slot": "timezone", "value": "Europe", "evidence": "select the Europe timezone"}
		}
		content, _ := json.Marshal(value)
		json.NewEncoder(w).Encode(map[string]any{"usage": map[string]int{"prompt_tokens": 19, "completion_tokens": 8},
			"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": string(content)}}}})
	}))
	defer provider.Close()
	model, err := memory.NewModel(provider.URL+"/v1/chat/completions", "fixture", "")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(New(service, model, slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer server.Close()
	server.Client().Timeout = 4 * time.Second
	request := func(method, path, token string, body any, want int) []byte {
		t.Helper()
		var payload io.Reader
		if body != nil {
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			payload = bytes.NewReader(encoded)
		}
		req, err := http.NewRequestWithContext(ctx, method, server.URL+path, payload)
		if err != nil {
			t.Fatal(err)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != want || res.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s %s: code=%d, response=%s", method, path, res.StatusCode, data)
		}
		return data
	}
	if page := request("GET", "/", "", nil, 200); !strings.Contains(string(page), "访问工作台") {
		t.Fatal("workbench missing")
	}
	request("GET", "/console.js", "", nil, 200)
	request("GET", "/healthz", "", nil, 200)
	request("GET", "/api/session", "", nil, 403)
	request("GET", "/api/session", owner, nil, 200)
	query := memory.Recall{Subject: "riley", Purpose: "support", Slot: "notifications"}
	request("POST", "/api/recall", app, query, 403)
	request("POST", "/api/consent", owner, memory.Consent{App: "assistant", Purpose: "support", Read: true, Write: true, Auto: true, Expires: time.Now().Add(time.Hour)}, 200)
	source := memory.Source{Subject: "riley", Purpose: "support", Session: "web-session", External: "web-source", Kind: "user",
		Occurred: time.Now().Add(-time.Minute), Statement: "Remember my notifications as sms."}
	request("POST", "/api/source", app, source, 200)
	changed := source
	changed.Statement = "Remember my notifications as parcel."
	receive := func(in memory.Source, replayed bool) string {
		t.Helper()
		var receipt struct {
			ID       string `json:"conflict_id"`
			Replayed bool   `json:"replayed"`
		}
		jsonBody := request("POST", "/api/source", app, in, 409)
		if err := json.Unmarshal(jsonBody, &receipt); err != nil || receipt.ID == "" || receipt.Replayed != replayed {
			t.Fatalf("unexpected conflict receipt: %s (%v)", jsonBody, err)
		}
		return receipt.ID
	}
	id := receive(changed, false)
	if receive(changed, true) != id {
		t.Fatal("conflict replay changed the receipt")
	}
	for i := range 50 {
		variant := changed
		variant.Statement = fmt.Sprintf("Remember my notifications as delivery option %d.", i)
		receive(variant, false)
	}
	var page memory.SourceConflictPage
	readPage := func(token, suffix string) memory.SourceConflictPage {
		t.Helper()
		var result memory.SourceConflictPage
		body := request("GET", "/api/source-conflicts"+suffix, token, nil, 200)
		if err := json.Unmarshal(body, &result); err != nil || strings.Contains(string(body), "parcel") {
			t.Fatalf("invalid or sensitive conflict queue: %s (%v)", body, err)
		}
		return result
	}
	page = readPage(owner, "")
	if len(page.Items) != 50 || page.NextAfter == 0 || page.Items[0].External != source.External ||
		page.Items[0].App != "assistant" || page.Items[0].Session != source.Session {
		t.Fatalf("wrong conflict page: %+v", page)
	}
	last := readPage(owner, fmt.Sprintf("?after=%d", page.NextAfter))
	if len(last.Items) != 1 || last.NextAfter != 0 || last.Items[0].ID <= page.NextAfter {
		t.Fatalf("wrong conflict page continuation: %+v", last)
	}
	request("GET", "/api/source-conflicts", app, nil, 403)
	dismissal := map[string]string{"id": id}
	for _, token := range []string{otherUser, otherTenant} {
		if len(readPage(token, "").Items) != 0 {
			t.Fatal("another owner's conflict queue was disclosed")
		}
		request("POST", "/api/source-conflicts/dismiss", token, dismissal, 404)
	}
	request("POST", "/api/source-conflicts/dismiss", owner, dismissal, 200)
	request("POST", "/api/source-conflicts/dismiss", owner, dismissal, 200)
	remaining := readPage(owner, "")
	if receive(changed, true) != id || len(remaining.Items) != 50 || remaining.Items[0].ID == page.Items[0].ID {
		t.Fatal("dismissed conflict was re-opened")
	}
	if facts := request("POST", "/api/recall", app, query, 200); !strings.Contains(string(facts), "web-source") {
		t.Fatalf("recall lost provenance: %s", facts)
	}
	var share memory.Share
	body := request("POST", "/api/share", owner, map[string]any{"slot": "notifications", "purpose": "support", "minutes": 15}, 200)
	if err := json.Unmarshal(body, &share); err != nil {
		t.Fatal(err)
	}
	request("GET", "/share", "", nil, 200)
	request("POST", "/api/shared", "", map[string]string{"token": share.Token}, 200)
	modelConsent := memory.Consent{App: "assistant", Purpose: "preferences", Read: true, Write: true, Model: true, Expires: time.Now().Add(time.Hour)}
	request("POST", "/api/consent", owner, modelConsent, 200)
	readJob := func(body []byte) memory.ModelReceipt {
		t.Helper()
		var result memory.ModelReceipt
		if err := json.Unmarshal(body, &result); err != nil || result.ID == "" {
			t.Fatalf("invalid job receipt: %s (%v)", body, err)
		}
		return result
	}
	startJob := func(external, statement string) map[string]string {
		t.Helper()
		in := memory.Source{Subject: source.Subject, Purpose: modelConsent.Purpose, Session: "model-fixture", External: external,
			Kind: "user", Occurred: time.Now().Add(-time.Second), Statement: statement}
		response := request("POST", "/api/source", app, in, 200)
		var submission memory.Submission
		if err := json.Unmarshal(response, &submission); err != nil || submission.Source == "" {
			t.Fatalf("missing model source: %s (%v)", response, err)
		}
		return map[string]string{"subject": in.Subject, "purpose": in.Purpose, "source": submission.Source}
	}
	processJob := func() {
		t.Helper()
		if worked, err := service.ProcessExtraction(ctx, model); err != nil || !worked {
			t.Fatalf("job did not run: %v", err)
		}
	}
	modelInput := startJob("language-case", "I usually use French in these settings.")
	job := readJob(request("POST", "/api/extract/jobs", app, modelInput, 200))
	statusPath := "/api/extract/jobs?id=" + job.ID
	if job.Status != "queued" || readJob(request("POST", "/api/extract/jobs", app, modelInput, 200)).ID != job.ID {
		t.Fatal("initial job did not deduplicate")
	}
	request("GET", statusPath, otherUser, nil, 404)
	request("GET", statusPath, otherTenant, nil, 404)
	processJob()
	outcome := readJob(request("GET", statusPath, owner, nil, 200))
	if outcome.Status != "pending" || outcome.Attempt != 1 || outcome.PromptTokens != 19 || outcome.Candidate == "" {
		t.Fatalf("missing candidate or cost trace: %+v", outcome)
	}
	pending := request("GET", "/api/pending", owner, nil, 200)
	if !strings.Contains(string(pending), `"evidence":"use French"`) || !strings.Contains(string(pending), `"confidence":"evidence-matched"`) {
		t.Fatalf("pending model evidence was not the verified excerpt: %s", pending)
	}
	manual := memory.Source{Subject: source.Subject, Purpose: modelConsent.Purpose, Session: "model-fixture", External: "batch-units",
		Kind: "user", Occurred: time.Now().Add(-time.Second), Statement: "Remember my units as metric."}
	var units memory.Submission
	if err := json.Unmarshal(request("POST", "/api/source", app, manual, 200), &units); err != nil || units.Candidate == "" {
		t.Fatalf("missing rule candidate: %+v (%v)", units, err)
	}
	batch := []map[string]any{
		{"candidate": outcome.Candidate, "expected_version": 1, "accept": true},
		{"candidate": units.Candidate, "expected_version": 0, "accept": false},
		{"candidate": strings.Repeat("0", 32), "expected_version": 0, "accept": false},
	}
	request("POST", "/api/review/batch", app, batch, 403)
	var results []reviewOutcome
	if err := json.Unmarshal(request("POST", "/api/review/batch", owner, batch, 200), &results); err != nil ||
		len(results) != 3 || results[0].Error != "conflict" || results[1].Status != "rejected" || results[2].Error != "missing" {
		t.Fatalf("incorrect partial batch result: %+v (%v)", results, err)
	}
	request("POST", "/api/review", owner, map[string]any{"candidate": outcome.Candidate, "expected_version": 0, "accept": true}, 200)
	modelFacts := request("POST", "/api/recall", app, memory.Recall{Subject: source.Subject, Purpose: modelConsent.Purpose, Slot: "language"}, 200)
	if !strings.Contains(string(modelFacts), `"evidence":"use French"`) {
		t.Fatalf("review lost exact evidence: %s", modelFacts)
	}

	retryInput := startJob("retry-case", "I prefer rare fixture measurements as my default.")
	retryJob := readJob(request("POST", "/api/extract/jobs", app, retryInput, 200))
	processJob()
	retryPath := "/api/extract/jobs?id=" + retryJob.ID
	if failed := readJob(request("GET", retryPath, app, nil, 200)); failed.Status != "failed" || failed.Attempt != 1 {
		t.Fatalf("failed outcome was not persisted: %+v", failed)
	}
	if resumed := readJob(request("POST", "/api/extract/jobs", app, retryInput, 200)); resumed.ID != retryJob.ID || resumed.Status != "queued" {
		t.Fatalf("retry changed the job: %+v", resumed)
	}
	if _, err := db.ExecContext(ctx, "UPDATE model_jobs SET status='running',attempt=2,leased_until=UTC_TIMESTAMP(6) WHERE id=?", retryJob.ID); err != nil {
		t.Fatal(err)
	}
	processJob()
	if resumed := readJob(request("GET", retryPath, app, nil, 200)); resumed.Status != "no-proposal" || resumed.Attempt != 3 || retries.Load() != 2 {
		t.Fatalf("expired lease did not resume safely: %+v", resumed)
	}

	blockedInput := startJob("revoke-in-flight", "I normally select the Europe timezone.")
	blocked := readJob(request("POST", "/api/extract/jobs", app, blockedInput, 200))
	processed := make(chan error, 1)
	go func() {
		_, err := service.ProcessExtraction(ctx, model)
		processed <- err
	}()
	select {
	case <-waiting:
	case <-time.After(4 * time.Second):
		t.Fatal("model never received the running task")
	}
	blockedPath := "/api/extract/jobs?id=" + blocked.ID
	if running := readJob(request("GET", blockedPath, app, nil, 200)); running.Status != "running" {
		t.Fatalf("unexpected in-flight status: %+v", running)
	}
	request("POST", "/api/consent/revoke", owner, map[string]string{"app": "assistant", "purpose": modelConsent.Purpose, "key": "model-revoke"}, 200)
	request("GET", blockedPath, app, nil, 404)
	request("POST", "/api/consent", owner, modelConsent, 200)
	close(release)
	if err := <-processed; err != nil {
		t.Fatal(err)
	}
	if discarded := readJob(request("GET", blockedPath, owner, nil, 200)); discarded.Status != "discarded" || discarded.Candidate != "" {
		t.Fatalf("revoked in-flight inference created a proposal: %+v", discarded)
	}
	request("POST", "/api/forget", owner, map[string]any{"key": "missing-confirmation"}, 400)
	body = request("POST", "/api/forget", owner, map[string]any{"slot": "notifications", "key": "forget-web"}, 200)
	var forgotten memory.Receipt
	if err := json.Unmarshal(body, &forgotten); err != nil || forgotten.ID == "" {
		t.Fatalf("missing forgetting receipt: %s (%v)", body, err)
	}
	progress := "/api/forget?id=" + forgotten.ID
	request("GET", progress, otherUser, nil, 404)
	request("GET", progress, otherTenant, nil, 404)
	request("GET", progress, app, nil, 403)
	if state := request("GET", progress, owner, nil, 200); !strings.Contains(string(state), `"status":"applied"`) {
		t.Fatalf("forgetting was not applied: %s", state)
	}
	if len(readPage(owner, "").Items) != 0 {
		t.Fatal("forgetting left source conflicts")
	}
	request("POST", "/api/shared", "", map[string]string{"token": share.Token}, 404)
	request("POST", "/api/source", app, source, 410)
	request("POST", "/api/forget", owner, map[string]any{"key": "forget-account", "all": true}, 200)
	request("GET", statusPath, owner, nil, 404)
	request("POST", "/api/recall", app, query, 403)
	t.Log("HTTP conflicts, isolation, share, forgetting, asynchronous extraction, retry and revocation checked")
}
