package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/axin2024/agent-memory/internal/memory"
	"github.com/axin2024/agent-memory/internal/web"
)

var version = "dev"
var revision = "unknown"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] == "help" {
		fmt.Println("agent-memory version | keygen | journal-init | migrate | provision TENANT ACTOR ROLE HOURS | recover | serve")
		return nil
	}
	command := args[0]
	args = args[1:]
	if command == "version" {
		fmt.Printf("agent-memory %s %s\n", version, revision)
		return nil
	}
	path := os.Getenv("MEMORY_JOURNAL")
	keyPath := os.Getenv("MEMORY_KEY_FILE")
	if command == "keygen" {
		return memory.WriteKey(keyPath)
	}
	if command == "journal-init" {
		key, err := memory.ReadKey(keyPath)
		if err != nil {
			return err
		}
		return memory.InitJournal(path, key)
	}
	if command != "migrate" && command != "provision" && command != "recover" && command != "serve" {
		return errors.New("unknown command")
	}
	db, err := memory.Connect(os.Getenv("MEMORY_MYSQL_DSN"))
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if command == "migrate" {
		return memory.Migrate(ctx, db)
	}
	key, err := memory.ReadKey(keyPath)
	if err != nil {
		return err
	}
	service, err := memory.Open(ctx, db, path, key)
	if err != nil {
		return err
	}
	defer service.Close()
	switch command {
	case "recover":
		fmt.Println("journal verified and applied")
		return nil
	case "provision":
		if len(args) != 4 {
			return memory.ErrInput
		}
		ttl, err := time.ParseDuration(args[3] + "h")
		if err != nil {
			return memory.ErrInput
		}
		token, err := service.Provision(ctx, args[0], args[1], args[2], ttl)
		if err != nil {
			return err
		}
		fmt.Println(token)
		return nil
	default:
		cancel()
		return serve(args, service)
	}
}

func serve(args []string, service *memory.Service) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	address := flags.String("listen", "127.0.0.1:8105", "listen address")
	cert := flags.String("tls-cert", "", "TLS PEM certificate")
	key := flags.String("tls-key", "", "TLS PEM key")
	if err := flags.Parse(args); err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(*address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	local := ip != nil && ip.IsLoopback()
	if (*cert == "") != (*key == "") || !local && *cert == "" {
		return errors.New("non-loopback listeners require a TLS certificate and key")
	}
	var model *memory.Model
	if modelURL := strings.TrimSpace(os.Getenv("MEMORY_MODEL_URL")); modelURL != "" {
		model, err = memory.NewModel(modelURL, os.Getenv("MEMORY_MODEL_NAME"), os.Getenv("MEMORY_MODEL_TOKEN"))
		if err != nil {
			return err
		}
	}
	var embedding *memory.TeamEmbedder
	if target := strings.TrimSpace(os.Getenv("MEMORY_TEAM_EMBED_URL")); target != "" {
		embedding, err = memory.NewTeamEmbedder(target, os.Getenv("MEMORY_TEAM_EMBED_NAME"),
			os.Getenv("MEMORY_TEAM_EMBED_TOKEN"), os.Getenv("MEMORY_TEAM_EMBED_WEIGHTS_SHA256"))
		if err != nil {
			return err
		}
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	server := &http.Server{Addr: *address, Handler: web.New(service, model, logger, embedding), ReadHeaderTimeout: 4 * time.Second,
		ReadTimeout: 12 * time.Second, WriteTimeout: 25 * time.Second, IdleTimeout: 45 * time.Second, MaxHeaderBytes: 16 * 1024}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if model != nil {
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			runModelWorker(ctx, service, model, logger)
		}()
		defer func() {
			cancel()
			<-stopped
		}()
	}
	exited := make(chan error, 1)
	go func() {
		if *cert != "" {
			exited <- server.ListenAndServeTLS(*cert, *key)
			return
		}
		exited <- server.ListenAndServe()
	}()
	select {
	case err = <-exited:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		return server.Shutdown(shutdown)
	}
}

func runModelWorker(ctx context.Context, service *memory.Service, model *memory.Model, logger *slog.Logger) {
	poll := time.NewTicker(500 * time.Millisecond)
	defer poll.Stop()
	for ctx.Err() == nil {
		worked, err := service.ProcessExtraction(ctx, model)
		if err != nil && ctx.Err() == nil {
			logger.Error("model job processing failed", "error_type", fmt.Sprintf("%T", err))
		}
		if err == nil && worked {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
		}
	}
}
