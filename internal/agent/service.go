package agent

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/aisidev/tt/internal/auth"
	"github.com/aisidev/tt/internal/command"
	"github.com/aisidev/tt/internal/engine"
	"github.com/aisidev/tt/internal/runcmd"
	"github.com/aisidev/tt/model"
)

func serveAgent(ctx context.Context, cfg Config, stderr io.Writer) int {
	var key *string
	if cfg.present["api-key"] {
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
	path := filepath.Join(cfg.DataDir, "agent.db")
	var requestedHost *string
	if cfg.present["host-id"] {
		requestedHost = &cfg.HostID
	}
	host, err := ResolveHostID(ctx, path, requestedHost)
	if err != nil {
		fmt.Fprintf(stderr, "Failed to resolve host_id: %v\n", err)
		return 1
	}
	resource := model.Resource{CPUTotal: cfg.EffectiveCPU(), MemTotal: cfg.EffectiveMem(ctx), DiskTotal: uint32(cfg.DiskTotal)}
	backend, err := model.ParseStorage(cfg.Storage)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	rt, err := newRuntime(ctx, host, backend, cfg.ImageDir, cfg.RuntimeDir, path, resource, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "Failed to initialize runtime: %v\n", err)
		return 1
	}
	defer rt.Close()
	var requestedRuntime *engine.ContainerRuntime
	if cfg.present["container-runtime"] {
		selected, err := engine.ParseContainerRuntime(cfg.ContainerRuntime)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		requestedRuntime = &selected
	}
	if err := rt.ConfigureContainerRuntime(ctx, requestedRuntime); err != nil {
		fmt.Fprintf(stderr, "Cannot bind container runtime: %v\n", err)
		return 1
	}
	var ingress *SshIngress
	if cfg.present["ssh-public-address"] {
		address, err := netip.ParseAddr(cfg.SSHPublicAddress)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		ingress = &SshIngress{PublicAddress: address}
		if cfg.present["ssh-ingress-netns"] {
			ingress.Namespace = &cfg.SSHIngressNetNS
		}
		if cfg.present["ssh-ingress-target"] {
			target, err := netip.ParseAddr(cfg.SSHIngressTarget)
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			ingress.Target = &target
		}
	}
	if err := rt.ConfigureNetwork(ctx, ingress, uint16(cfg.PortStart), uint16(cfg.PortEnd)); err != nil {
		fmt.Fprintf(stderr, "Invalid agent network configuration: %v\n", err)
		return 1
	}
	engine.SetStopGrace(cfg.StopGrace)
	rt.ConfigureBackup(ctx, cfg.EnableDiskBackup)
	if cfg.AuthorityFile != "" {
		if !cfg.present["host-id"] {
			fmt.Fprintln(stderr, "epoch receiver requires an explicit stable host identity")
			return 1
		}
		authority, err := command.OpenAuthority(cfg.AuthorityFile)
		if err != nil {
			fmt.Fprintln(stderr, "cannot load receiver authority")
			return 1
		}
		state, err := authority.State()
		if err != nil {
			fmt.Fprintln(stderr, "cannot load receiver authority")
			return 1
		}
		parent, domainLock, err := engine.OpenProcessDomain(state.ID, host)
		if err != nil {
			fmt.Fprintln(stderr, "cannot establish exclusive payload cgroup domain")
			return 1
		}
		defer domainLock.Close()
		rt.processDomain = parent
		rt.engineFactory = func(kind model.Engine, container *engine.ContainerRuntime) (engine.Engine, error) {
			base, err := engine.New(kind, container)
			if err != nil {
				return nil, err
			}
			return engine.WithProcessDomain(base, parent)
		}
		rt.processEpochOnly = true
		rt.engines = slices.DeleteFunc(append([]model.Engine{}, rt.engines...), func(value model.Engine) bool { return value != model.Qemu && value != model.Firecracker })
	}
	server := newServer(rt, path)
	if err = server.commandJournal.SetAuthority(cfg.AuthorityFile); err != nil {
		fmt.Fprintln(stderr, "cannot load receiver authority")
		return 1
	}
	if err = server.loadQuarantine(ctx); err != nil {
		fmt.Fprintln(stderr, "cannot restore external quarantine reservations")
		return 1
	}
	if key != nil {
		fmt.Fprintln(stderr, "API key authentication enabled")
	} else {
		fmt.Fprintln(stderr, "WARNING: no --api-key set, all agent endpoints are unauthenticated!")
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		fmt.Fprintf(stderr, "Failed to bind %s: %v\n", cfg.Listen, err)
		return 1
	}
	fmt.Fprintf(stderr, "tt-agent [%s] listening on %s\n", host, cfg.Listen)
	background, stopBackground := context.WithCancel(context.Background())
	var loops sync.WaitGroup
	loop := func(work func(context.Context)) {
		loops.Add(1)
		go func() {
			defer loops.Done()
			for {
				if background.Err() != nil {
					return
				}
				work(background)
				timer := time.NewTimer(15 * time.Second)
				select {
				case <-background.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		}()
	}
	loop(server.RefreshImages)
	loop(func(loopCtx context.Context) {
		if !server.commandJournal.MutationsAllowed() {
			return
		}
		if err := server.ReconcileOnce(loopCtx); err != nil {
			fmt.Fprintf(stderr, "[agent] reconciliation failed: %v\n", err)
		}
	})
	httpServer := &http.Server{Handler: server.Handler(key), ReadHeaderTimeout: 10 * time.Second} // see compatibility §7
	serving := make(chan error, 1)
	go func() { serving <- httpServer.Serve(listener) }()
	select {
	case <-ctx.Done():
		fmt.Fprintln(stderr, "received shutdown signal")
		stopBackground()
		_ = httpServer.Shutdown(context.Background())
		<-serving
	case err := <-serving:
		stopBackground()
		_ = httpServer.Shutdown(context.Background())
		if err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(stderr, "Server error: %v\n", err)
		}
	}
	loops.Wait()
	fmt.Fprintln(stderr, "tt-agent shutting down")
	return 0
}
