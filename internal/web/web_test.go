package web

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	server := httptest.NewServer(New(service, nil, slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer server.Close()
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
	request("POST", "/api/forget", owner, map[string]any{"key": "missing-confirmation"}, 400)
	request("POST", "/api/forget", owner, map[string]any{"slot": "notifications", "key": "forget-web"}, 200)
	request("POST", "/api/shared", "", map[string]string{"token": share.Token}, 404)
	request("POST", "/api/source", app, source, 410)
	request("POST", "/api/forget", owner, map[string]any{"key": "forget-account", "all": true}, 200)
	request("POST", "/api/recall", app, query, 403)
	t.Log("HTTP workbench, independent application grant, source provenance, share, forgetting and replay checked")
}
