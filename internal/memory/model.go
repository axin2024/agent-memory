package memory

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

var ErrProvider = errors.New("model output unavailable")
var modelSlots = []string{"notifications", "language", "timezone", "units", "none"}

type Model struct {
	target string
	name   string
	token  string
	client *http.Client
}

type extraction struct {
	Slot     string `json:"slot"`
	Value    string `json:"value"`
	Evidence string `json:"evidence"`
}

type ModelReceipt struct {
	Source           string `json:"source"`
	Candidate        string `json:"candidate,omitempty"`
	Status           string `json:"status"`
	Model            string `json:"model"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	DurationMillis   int64  `json:"duration_ms"`
	ResponseSHA      string `json:"response_sha256,omitempty"`
}

func NewModel(target, name, token string) (*Model, error) {
	endpoint, err := url.Parse(target)
	if err != nil || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" ||
		!strings.HasSuffix(endpoint.Path, "/v1/chat/completions") || !textOK(name, 128) {
		return nil, ErrInput
	}
	hostname := net.ParseIP(endpoint.Hostname())
	if endpoint.Scheme != "https" && (endpoint.Scheme != "http" || hostname == nil || !hostname.IsLoopback()) {
		return nil, ErrInput
	}
	if strings.ContainsAny(token, "\r\n\x00") {
		return nil, ErrInput
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &Model{target, name, token, &http.Client{Timeout: 8 * time.Second, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (m *Model) extract(ctx context.Context, text string) (extraction, ModelReceipt, error) {
	start := time.Now()
	receipt := ModelReceipt{Model: m.name}
	system := "Extract only an explicit first-person continuing preference. Choose notifications, language, timezone, or units. " +
		"Copy its short value and exact evidence from the input. For quotations, instructions, one-time requests, or uncertainty choose none and empty strings."
	request := map[string]any{"model": m.name, "temperature": 0, "max_tokens": 140, "stream": false,
		"messages": []map[string]string{{"role": "system", "content": system}, {"role": "user", "content": text}},
		"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{
			"name": "preference", "strict": true, "schema": map[string]any{"type": "object", "additionalProperties": false,
				"required": []string{"slot", "value", "evidence"}, "properties": map[string]any{
					"slot": map[string]any{"type": "string", "enum": modelSlots}, "value": map[string]string{"type": "string"}, "evidence": map[string]string{"type": "string"},
				}}}},
	}
	payload, _ := json.Marshal(request)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.target, bytes.NewReader(payload))
	if err != nil {
		return extraction{}, receipt, ErrProvider
	}
	req.Header.Set("Content-Type", "application/json")
	if m.token != "" {
		req.Header.Set("Authorization", "Bearer "+m.token)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return extraction{}, receipt, ErrProvider
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 65_537))
	if err != nil || len(data) > 65_536 || resp.StatusCode != 200 {
		return extraction{}, receipt, ErrProvider
	}
	receipt.DurationMillis = time.Since(start).Milliseconds()
	receipt.ResponseSHA = hash(string(data))
	var result struct {
		Usage struct {
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
		} `json:"usage"`
		Choices []struct {
			Finish  string `json:"finish_reason"`
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &result) != nil || len(result.Choices) != 1 || result.Choices[0].Finish != "stop" ||
		result.Usage.Prompt < 0 || result.Usage.Completion < 0 {
		return extraction{}, receipt, ErrProvider
	}
	receipt.PromptTokens = result.Usage.Prompt
	receipt.CompletionTokens = result.Usage.Completion
	var value extraction
	decode := json.NewDecoder(strings.NewReader(result.Choices[0].Message.Content))
	decode.DisallowUnknownFields()
	if decode.Decode(&value) != nil || decode.Decode(new(any)) != io.EOF || !slices.Contains(modelSlots, value.Slot) {
		return extraction{}, receipt, ErrProvider
	}
	if value.Slot == "none" {
		return extraction{}, receipt, nil
	}
	if !textOK(value.Value, 240) || !textOK(value.Evidence, 750) || !strings.Contains(text, value.Evidence) ||
		!strings.Contains(value.Evidence, value.Value) || sensitive.MatchString(value.Value) || attribution.MatchString(value.Evidence) {
		return extraction{}, receipt, nil
	}
	return value, receipt, nil
}

func (s *Service) Enrich(ctx context.Context, who Identity, subject, purpose, source string, model *Model) (ModelReceipt, error) {
	if len(source) != 64 || model == nil {
		return ModelReceipt{}, ErrInput
	}
	if err := s.lock(ctx); err != nil {
		return ModelReceipt{}, err
	}
	defer s.mu.Unlock()
	var receipt ModelReceipt
	err := s.tx(ctx, func(tx *sql.Tx) error {
		scope, c, err := s.grants(ctx, tx, who, subject, purpose, true)
		if err != nil {
			return err
		}
		if !c.Model || who.Role != "app" {
			return ErrDenied
		}
		p, err := evidence(ctx, tx, scope, source)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrMissing
		}
		if err != nil {
			return err
		}
		if p.App != who.Actor || p.Purpose != purpose || p.Evidence == "" || !p.Expires.After(time.Now().UTC()) {
			return ErrDenied
		}
		receipt.Source, receipt.Model = source, model.name
		err = tx.QueryRowContext(ctx, "SELECT candidate,status FROM candidates WHERE scope_hash=? AND source_hash=?", scope, source).
			Scan(&receipt.Candidate, &receipt.Status)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		instruction := strings.ToLower(p.Evidence)
		if !(strings.HasPrefix(instruction, "i ") || strings.HasPrefix(instruction, "my ") || strings.HasPrefix(instruction, "我")) ||
			attribution.MatchString(instruction) || sensitive.MatchString(instruction) {
			return ErrInput
		}
		value, result, err := model.extract(ctx, p.Evidence)
		if err != nil {
			return err
		}
		receipt = result
		receipt.Source = source
		receipt.Status = "no-proposal"
		if value.Slot != "" {
			receipt.Candidate, receipt.Status, err = s.addCandidate(ctx, tx, who.Tenant, scope, source, value.Slot, value.Value, "model", false)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "UPDATE sources SET slot=?,slot_hash=? WHERE scope_hash=? AND source_hash=?",
				value.Slot, s.slot(scope, value.Slot), scope, source); err != nil {
				return err
			}
		}
		return event(ctx, tx, who.Tenant, scope, "model-extract", source, receipt.Status, receipt.DurationMillis*1000)
	})
	return receipt, err
}
