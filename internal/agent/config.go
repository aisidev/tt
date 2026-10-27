// Package agent implements the TTstack host agent.
package agent

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/aisidev/tt/internal/commandline"
	"github.com/aisidev/tt/internal/engine"
	"github.com/aisidev/tt/model"
)

//go:embed help.txt
var help string

type Config struct {
	AuthorityFile                                                         string
	EnableDiskBackup                                                      bool
	ContainerRuntime, SSHPublicAddress, SSHIngressNetNS, SSHIngressTarget string
	PortStart, PortEnd                                                    uint
	Listen, ImageDir, RuntimeDir, DataDir, Storage                        string
	CPUTotal, MemTotal, DiskTotal                                         uint
	HostID, APIKey                                                        string
	StopGrace                                                             int
	present                                                               map[string]bool
}

func parseConfig(args []string, stdout, stderr io.Writer) (Config, bool, int) {
	var cfg Config
	fs := flag.NewFlagSet("tt-agent", flag.ContinueOnError)
	fs.BoolVar(&cfg.EnableDiskBackup, "enable-disk-backup", false, "Enable explicit disk backup admission")
	fs.IntVar(&cfg.StopGrace, "stop-grace", -1, "Graceful-stop grace seconds: -1 = engine defaults (QEMU 10, Firecracker 30), 0 = force kill immediately")
	fs.StringVar(&cfg.ContainerRuntime, "container-runtime", os.Getenv("TT_CONTAINER_RUNTIME"), "Container runtime for this inventory")
	fs.StringVar(&cfg.SSHPublicAddress, "ssh-public-address", "", "Client-reachable IPv4 address")
	fs.StringVar(&cfg.SSHIngressNetNS, "ssh-ingress-netns", "", "Outer network namespace")
	fs.StringVar(&cfg.SSHIngressTarget, "ssh-ingress-target", "", "Agent namespace address")
	commandline.UintVar(fs, &cfg.PortStart, "port-start", 20000, 16, "First TCP host port")
	commandline.UintVar(fs, &cfg.PortEnd, "port-end", 65535, 16, "Last TCP host port")
	fs.StringVar(&cfg.Listen, "listen", "0.0.0.0:9100", "Listen address for the HTTP API")
	fs.StringVar(&cfg.ImageDir, "image-dir", "/home/ttstack/images", "Base image directory or dataset")
	fs.StringVar(&cfg.RuntimeDir, "runtime-dir", "/home/ttstack/runtime", "Runtime clone directory or dataset")
	fs.StringVar(&cfg.AuthorityFile, "authority-file", "", "Private deployment authority file outside database restore scope")
	fs.StringVar(&cfg.DataDir, "data-dir", "/home/ttstack/data", "Directory for persistent state")
	fs.StringVar(&cfg.Storage, "storage", "file", "Storage backend")
	commandline.UintVar(fs, &cfg.CPUTotal, "cpu-total", 0, 32, "CPU capacity; 0 = auto-detect")
	commandline.UintVar(fs, &cfg.MemTotal, "mem-total", 0, 32, "Memory capacity MiB; 0 = auto-detect")
	commandline.UintVar(fs, &cfg.DiskTotal, "disk-total", 204800, 32, "Disk scheduling budget MiB")
	fs.StringVar(&cfg.HostID, "host-id", "", "Unique host identifier")
	fs.StringVar(&cfg.APIKey, "api-key", os.Getenv("TT_API_KEY"), "API key for authentication")
	fs.SetOutput(stderr)
	var showHelp, version bool
	fs.BoolVar(&showHelp, "help", false, "Print help")
	fs.BoolVar(&showHelp, "h", false, "Print help")
	fs.BoolVar(&version, "version", false, "Print version")
	fs.BoolVar(&version, "V", false, "Print version")
	fs.Usage = func() { fmt.Fprint(stderr, help) }
	if err := commandline.CheckFlags(fs, args); err != nil {
		fmt.Fprintln(stderr, err)
		return cfg, false, 2
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return cfg, false, 0
		}
		return cfg, false, 2
	}
	if showHelp {
		fmt.Fprint(stdout, help)
		return cfg, false, 0
	}
	if version {
		fmt.Fprintf(stdout, "tt-agent %s\n", commandline.Version)
		return cfg, false, 0
	}
	if len(fs.Args()) > 0 {
		fmt.Fprintf(stderr, "error: unexpected argument '%s' found\n", fs.Args()[0])
		return cfg, false, 2
	}
	cfg.present = map[string]bool{}
	fs.Visit(func(f *flag.Flag) { cfg.present[f.Name] = true })
	if _, ok := os.LookupEnv("TT_CONTAINER_RUNTIME"); ok {
		cfg.present["container-runtime"] = true
	}
	if _, ok := os.LookupEnv("TT_API_KEY"); ok {
		cfg.present["api-key"] = true
	}
	if _, err := model.ParseStorage(cfg.Storage); err != nil {
		fmt.Fprintln(stderr, err)
		return cfg, false, 2
	}
	if cfg.present["container-runtime"] {
		if _, err := engine.ParseContainerRuntime(cfg.ContainerRuntime); err != nil {
			fmt.Fprintln(stderr, err)
			return cfg, false, 2
		}
	}
	if cfg.PortStart > math.MaxUint16 || cfg.PortEnd > math.MaxUint16 || cfg.CPUTotal > math.MaxUint32 || cfg.MemTotal > math.MaxUint32 || cfg.DiskTotal > math.MaxUint32 {
		fmt.Fprintln(stderr, "error: numeric argument out of range")
		return cfg, false, 2
	}
	if cfg.StopGrace < -1 || cfg.StopGrace > 300 {
		fmt.Fprintln(stderr, "error: --stop-grace must be within -1..300 seconds")
		return cfg, false, 2
	}
	for _, field := range []struct{ name, value string }{{"ssh-public-address", cfg.SSHPublicAddress}, {"ssh-ingress-target", cfg.SSHIngressTarget}} {
		if cfg.present[field.name] {
			address, err := netip.ParseAddr(field.value)
			if err != nil || !address.Is4() {
				fmt.Fprintf(stderr, "error: invalid value for --%s: invalid IPv4 address\n", field.name)
				return cfg, false, 2
			}
		}
	}
	if cfg.present["ssh-ingress-netns"] && (!cfg.present["ssh-public-address"] || !cfg.present["ssh-ingress-target"]) || cfg.present["ssh-ingress-target"] && !cfg.present["ssh-ingress-netns"] {
		fmt.Fprintln(stderr, "error: SSH ingress requires --ssh-public-address, --ssh-ingress-netns and --ssh-ingress-target together")
		return cfg, false, 2
	}
	return cfg, true, 0
}

func Run(args []string, stdout, stderr io.Writer) int {
	cfg, proceed, code := parseConfig(args, stdout, stderr)
	if !proceed {
		return code
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return serveAgent(ctx, cfg, stderr)
}
func (c Config) EffectiveCPU() uint32 {
	if c.CPUTotal != 0 {
		return uint32(c.CPUTotal)
	}
	return availableCPU()
}
func (c Config) EffectiveMem(ctx context.Context) uint32 {
	if c.MemTotal != 0 {
		return uint32(c.MemTotal)
	}
	if memory, ok := totalMemory(ctx); ok {
		return memory
	}
	return 8192
}
func parseMemTotal(content string) (uint32, bool) {
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return 0, false
			}
			kb, err := strconv.ParseUint(strings.TrimPrefix(fields[1], "+"), 10, 64)
			return uint32(kb / 1024), err == nil
		}
	}
	return 0, false
}
