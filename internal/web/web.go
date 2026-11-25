package web

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/axin2024/agent-memory/internal/memory"
)

//go:embed ui/*
var assets embed.FS

type Endpoint struct {
	service *memory.Service
	model   *memory.Model
	log     *slog.Logger
}

func New(service *memory.Service, model *memory.Model, logger *slog.Logger) http.Handler {
	e := Endpoint{service, model, logger}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", static("index.html", "text/html"))
	mux.HandleFunc("GET /share", static("share.html", "text/html"))
	mux.HandleFunc("GET /console.css", static("console.css", "text/css"))
	mux.HandleFunc("GET /console.js", static("console.js", "text/javascript"))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		e.reply(w, r, map[string]string{"status": "ready"}, service.Check(r.Context()))
	})
	mux.HandleFunc("POST /api/shared", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Token string `json:"token"`
		}
		if err := read(w, r, &input); err != nil {
			e.reply(w, r, nil, err)
			return
		}
		fact, err := service.ReadShare(r.Context(), input.Token)
		e.reply(w, r, fact, err)
	})
	mux.HandleFunc("/api/", e.api)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		mux.ServeHTTP(w, r)
	})
}

func static(file, kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := assets.ReadFile("ui/" + file)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", kind+"; charset=utf-8")
		w.Write(body)
	}
}

func read(w http.ResponseWriter, r *http.Request, target any) error {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return memory.ErrInput
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024))
	d.DisallowUnknownFields()
	if err = d.Decode(target); err != nil {
		return memory.ErrInput
	}
	if d.Decode(new(any)) != io.EOF {
		return memory.ErrInput
	}
	return nil
}

func (e Endpoint) reply(w http.ResponseWriter, r *http.Request, body any, err error) {
	status := http.StatusOK
	if err != nil {
		var code string
		switch {
		case errors.Is(err, memory.ErrInput):
			status, code = http.StatusBadRequest, "invalid-input"
		case errors.Is(err, memory.ErrDenied):
			status, code = http.StatusForbidden, "access-denied"
		case errors.Is(err, memory.ErrMissing):
			status, code = http.StatusNotFound, "missing"
		case errors.Is(err, memory.ErrConflict):
			status, code = http.StatusConflict, "conflict"
		case errors.Is(err, memory.ErrForgotten):
			status, code = http.StatusGone, "forgotten"
		case errors.Is(err, memory.ErrRecovery):
			status, code = http.StatusServiceUnavailable, "recovery-required"
		case errors.Is(err, memory.ErrProvider):
			status, code = http.StatusBadGateway, "model-unavailable"
		default:
			status, code = http.StatusInternalServerError, "internal"
			e.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "error_type", fmt.Sprintf("%T", err))
		}
		body = map[string]string{"error": code}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func input[T any](w http.ResponseWriter, r *http.Request, fn func(T) (any, error)) (any, error) {
	var b T
	if err := read(w, r, &b); err != nil {
		return nil, err
	}
	return fn(b)
}

type operation struct {
	All     bool   `json:"all"`
	Key     string `json:"key"`
	Slot    string `json:"slot"`
	App     string `json:"app"`
	Purpose string `json:"purpose"`
	Token   string `json:"token"`
	Minutes int    `json:"minutes"`
}
type extractionRequest struct {
	Subject string `json:"subject"`
	Purpose string `json:"purpose"`
	Source  string `json:"source"`
}
type reviewRequest struct {
	Candidate string `json:"candidate"`
	Version   uint64 `json:"expected_version"`
	Accept    *bool  `json:"accept"`
}
type policyRequest struct {
	SourceDays int `json:"source_days"`
	FactDays   int `json:"fact_days"`
}

func (e Endpoint) api(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		e.reply(w, r, nil, memory.ErrDenied)
		return
	}
	ctx := r.Context()
	who, err := e.service.Authenticate(ctx, token)
	if err != nil {
		e.reply(w, r, nil, err)
		return
	}
	s := e.service
	var body any
	done := map[string]bool{"applied": true}
	switch r.Method + " " + r.URL.Path {
	case "GET /api/session":
		body = who
	case "GET /api/pending":
		body, err = s.Pending(ctx, who)
	case "GET /api/export":
		body, err = s.Export(ctx, who)
	case "GET /api/audit":
		var after uint64
		if raw := r.URL.Query().Get("after"); raw != "" {
			after, err = strconv.ParseUint(raw, 10, 64)
			if err != nil {
				e.reply(w, r, nil, memory.ErrInput)
				return
			}
		}
		body, err = s.Audit(ctx, who, after)
	case "POST /api/consent":
		body, err = input(w, r, func(in memory.Consent) (any, error) { return done, s.SetConsent(ctx, who, in) })
	case "POST /api/consent/revoke":
		body, err = input(w, r, func(in operation) (any, error) { return s.RevokeConsent(ctx, who, in.App, in.Purpose, in.Key) })
	case "POST /api/source":
		body, err = input(w, r, func(in memory.Source) (any, error) { return s.Ingest(ctx, who, in) })
	case "POST /api/extract":
		if e.model == nil {
			err = memory.ErrProvider
			break
		}
		body, err = input(w, r, func(in extractionRequest) (any, error) {
			return s.Enrich(ctx, who, in.Subject, in.Purpose, in.Source, e.model)
		})
	case "POST /api/review":
		body, err = input(w, r, func(in reviewRequest) (any, error) {
			if in.Accept == nil {
				return nil, memory.ErrInput
			}
			status, err := s.Review(ctx, who, in.Candidate, in.Version, *in.Accept)
			return map[string]string{"status": status}, err
		})
	case "POST /api/edit":
		body, err = input(w, r, func(in memory.Edit) (any, error) {
			version, err := s.Edit(ctx, who, in)
			return map[string]uint64{"version": version}, err
		})
	case "POST /api/recall":
		body, err = input(w, r, func(in memory.Recall) (any, error) { return s.Recall(ctx, who, in) })
	case "POST /api/forget":
		body, err = input(w, r, func(in operation) (any, error) {
			if (in.Slot == "") != in.All {
				return nil, memory.ErrInput
			}
			return s.Forget(ctx, who, in.Slot, in.Key)
		})
	case "POST /api/share":
		body, err = input(w, r, func(in operation) (any, error) { return s.CreateShare(ctx, who, in.Slot, in.Purpose, in.Minutes) })
	case "POST /api/share/revoke":
		body, err = input(w, r, func(in operation) (any, error) { return s.RevokeShare(ctx, who, in.Token, in.Key) })
	case "PUT /api/policy":
		body, err = input(w, r, func(in policyRequest) (any, error) { return done, s.SetPolicy(ctx, who, in.SourceDays, in.FactDays) })
	case "POST /api/sweep":
		body, err = s.Sweep(ctx, who)
	default:
		err = memory.ErrMissing
	}
	e.reply(w, r, body, err)
}
