package ctl

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aisidev/tt/internal/auth"
	"github.com/aisidev/tt/internal/command"
	"github.com/aisidev/tt/internal/runcmd"
)

func serveController(ctx context.Context, cfg Config, stderr io.Writer) int {
	var key *string
	if cfg.keyPresent {
		key = &cfg.APIKey
		if _, err := auth.ParseAPIKey(*key); err != nil {
			fmt.Fprintf(stderr, "Invalid API key: %v\n", err)
			return 1
		}
	}
	if cfg.AuthorityFile != "" && key == nil {
		fmt.Fprintln(stderr, "receiver authority requires bearer authentication")
		return 1
	}
	if err := os.MkdirAll(cfg.DataDir, 0777); err != nil {
		fmt.Fprintf(stderr, "Failed to create data dir %s: %v\n", cfg.DataDir, err)
		return 1
	}
	lock, err := runcmd.LockState(filepath.Join(cfg.DataDir, "service.lock"))
	if err != nil {
		fmt.Fprintf(stderr, "Cannot lock state: %v\n", err)
		return 1
	}
	defer lock.Close()
	db, err := OpenDB(ctx, filepath.Join(cfg.DataDir, "ctl.db"))
	if err != nil {
		fmt.Fprintf(stderr, "Failed to open database: %v\n", err)
		return 1
	}
	defer db.Close()
	db.stderr = stderr
	state := newServer(db, key)
	if err = state.commandJournal.SetAuthority(cfg.AuthorityFile); err != nil {
		fmt.Fprintln(stderr, "cannot load receiver authority")
		return 1
	}
	if cfg.EnrollmentDirectory != "" {
		if cfg.AuthorityFile == "" || deploymentStoreWithin(cfg.EnrollmentDirectory, cfg.DataDir) {
			fmt.Fprintln(stderr, "enrollment grants require an external authority and independent directory")
			return 1
		}
		store := &command.EnrollmentStore{Directory: cfg.EnrollmentDirectory}
		if store.Validate() != nil {
			fmt.Fprintln(stderr, "enrollment grant evidence unavailable")
			return 1
		}
		state.enrollmentStore = store
	}
	work, stopWork := context.WithCancel(context.Background())
	state.workCtx = work
	defer stopWork()
	state.stderr = stderr
	reader, err := state.clientFactory(5 * time.Second)
	if err != nil {
		fmt.Fprintf(stderr, "cannot initialize agent client: %v\n", err)
		return 1
	}
	if key != nil {
		fmt.Fprintln(stderr, "API key authentication enabled")
	} else {
		fmt.Fprintln(stderr, "WARNING: no --api-key set, all API endpoints are unauthenticated!")
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		fmt.Fprintf(stderr, "Failed to bind %s: %v\n", cfg.Listen, err)
		return 1
	}
	fmt.Fprintf(stderr, "tt-ctl listening on %s\n", cfg.Listen)
	background, stop := context.WithCancel(context.Background())
	var loops sync.WaitGroup
	loops.Add(2)
	go func() {
		defer loops.Done()
		for {
			timer := time.NewTimer(60 * time.Second)
			select {
			case <-background.Done():
				timer.Stop()
				return
			case <-timer.C:
				if state.commandJournal.Authority == nil {
					state.ExpireOnce(background)
				}
			}
		}
	}()
	go func() {
		defer loops.Done()
		for {
			if background.Err() != nil {
				return
			}
			if state.commandJournal.MutationsAllowed() {
				state.RefreshAllHosts(background, reader)
			}
			timer := time.NewTimer(15 * time.Second)
			select {
			case <-background.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	server := &http.Server{Handler: state.Handler(), ReadHeaderTimeout: 10 * time.Second} // see compatibility §7
	serving := make(chan error, 1)
	go func() { serving <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
		fmt.Fprintln(stderr, "received shutdown signal")
		_ = server.Shutdown(context.Background())
		<-serving
	case err := <-serving:
		_ = server.Shutdown(context.Background())
		if err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(stderr, "Server error: %v\n", err)
		}
	}
	// Graceful HTTP shutdown drains accepted foreground requests. Detached
	// creation and expiry work then stops, matching the Rust runtime lifetime;
	// its durable plans remain available for reconciliation after restart.
	stop()
	stopWork()
	loops.Wait()
	state.workers.Wait()
	fmt.Fprintln(stderr, "tt-ctl shutting down")
	return 0
}

func deploymentStoreWithin(path, root string) bool {
	return path == root || strings.HasPrefix(filepath.Clean(path), strings.TrimSuffix(filepath.Clean(root), "/")+"/")
}
