package cli

import (
	"fmt"
	"math"
	"net/netip"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/aisidev/tt/internal/engine"
	"github.com/aisidev/tt/model"
)

type deployGeneral struct {
	Prefix, User, ReleaseDir string
	APIKey                   *string
}
type deployController struct {
	Host, SSHUser, Listen string
	SSHPort               uint16
	DataDir               *string
}
type deployAgent struct {
	EnableDiskBackup                                           bool
	Host, SSHUser, Listen, Storage, DiskTotal                  string
	SSHPort                                                    uint16
	ImageDir, RuntimeDir, HostID, ContainerRuntime, ReleaseDir *string
	CPUTotal, MemTotal                                         uint32
}
type deployConfig struct {
	General    deployGeneral
	Controller *deployController
	Agents     []deployAgent
}

func deployText(table map[string]any, key, fallback string, required bool) (string, error) {
	value, present := table[key]
	if !present {
		if required {
			return "", fmt.Errorf("missing field `%s`", key)
		}
		return fallback, nil
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("field %s must be a string", key)
	}
	return text, nil
}
func deployOptional(table map[string]any, key string) (*string, error) {
	if _, present := table[key]; !present {
		return nil, nil
	}
	text, err := deployText(table, key, "", false)
	if err != nil {
		return nil, err
	}
	return &text, nil
}
func deployUnsigned(table map[string]any, key string, fallback, max uint64) (uint64, error) {
	value, present := table[key]
	if !present {
		return fallback, nil
	}
	n, ok := value.(int64)
	if !ok || n < 0 || uint64(n) > max {
		return 0, fmt.Errorf("field %s must be an unsigned integer in range", key)
	}
	return uint64(n), nil
}
func parseDeployConfig(content string) (deployConfig, error) {
	var raw map[string]any
	if _, err := toml.Decode(content, &raw); err != nil {
		return deployConfig{}, err
	}
	cfg := deployConfig{General: deployGeneral{Prefix: "/opt/ttstack", User: "ttstack", ReleaseDir: "./target/release"}, Agents: []deployAgent{}}
	if value, present := raw["general"]; present {
		table, ok := value.(map[string]any)
		if !ok {
			return cfg, fmt.Errorf("general must be a table")
		}
		var err error
		if cfg.General.Prefix, err = deployText(table, "prefix", cfg.General.Prefix, false); err != nil {
			return cfg, err
		}
		if cfg.General.User, err = deployText(table, "user", cfg.General.User, false); err != nil {
			return cfg, err
		}
		if cfg.General.ReleaseDir, err = deployText(table, "release_dir", cfg.General.ReleaseDir, false); err != nil {
			return cfg, err
		}
		if cfg.General.APIKey, err = deployOptional(table, "api_key"); err != nil {
			return cfg, err
		}
	}
	if value, present := raw["controller"]; present {
		table, ok := value.(map[string]any)
		if !ok {
			return cfg, fmt.Errorf("controller must be a table")
		}
		controller := &deployController{}
		var err error
		if controller.Host, err = deployText(table, "host", "", true); err != nil {
			return cfg, err
		}
		if controller.SSHUser, err = deployText(table, "ssh_user", "root", false); err != nil {
			return cfg, err
		}
		if controller.Listen, err = deployText(table, "listen", "0.0.0.0:9200", false); err != nil {
			return cfg, err
		}
		port, err := deployUnsigned(table, "ssh_port", 22, math.MaxUint16)
		if err != nil {
			return cfg, err
		}
		controller.SSHPort = uint16(port)
		if controller.DataDir, err = deployOptional(table, "data_dir"); err != nil {
			return cfg, err
		}
		cfg.Controller = controller
	}
	if value, present := raw["agents"]; present {
		tables := []map[string]any{}
		switch value := value.(type) {
		case []map[string]any:
			tables = value
		case []any:
			for _, entry := range value {
				table, ok := entry.(map[string]any)
				if !ok {
					return cfg, fmt.Errorf("agents must be tables")
				}
				tables = append(tables, table)
			}
		default:
			return cfg, fmt.Errorf("agents must be an array")
		}
		for _, table := range tables {
			agent := deployAgent{}
			var err error
			if agent.Host, err = deployText(table, "host", "", true); err != nil {
				return cfg, err
			}
			if agent.SSHUser, err = deployText(table, "ssh_user", "root", false); err != nil {
				return cfg, err
			}
			if agent.Listen, err = deployText(table, "listen", "0.0.0.0:9100", false); err != nil {
				return cfg, err
			}
			if agent.Storage, err = deployText(table, "storage", "file", false); err != nil {
				return cfg, err
			}
			if agent.DiskTotal, err = deployText(table, "disk_total", "200G", false); err != nil {
				return cfg, err
			}
			port, err := deployUnsigned(table, "ssh_port", 22, math.MaxUint16)
			if err != nil {
				return cfg, err
			}
			agent.SSHPort = uint16(port)
			cpu, err := deployUnsigned(table, "cpu_total", 0, math.MaxUint32)
			if err != nil {
				return cfg, err
			}
			agent.CPUTotal = uint32(cpu)
			mem, err := deployUnsigned(table, "mem_total", 0, math.MaxUint32)
			if err != nil {
				return cfg, err
			}
			agent.MemTotal = uint32(mem)
			if value, present := table["enable_disk_backup"]; present {
				enabled, ok := value.(bool)
				if !ok {
					return cfg, fmt.Errorf("field enable_disk_backup must be a boolean")
				}
				agent.EnableDiskBackup = enabled
			}
			for _, field := range []struct {
				name string
				out  **string
			}{{"image_dir", &agent.ImageDir}, {"runtime_dir", &agent.RuntimeDir}, {"host_id", &agent.HostID}, {"container_runtime", &agent.ContainerRuntime}, {"release_dir", &agent.ReleaseDir}} {
				value, err := deployOptional(table, field.name)
				if err != nil {
					return cfg, err
				}
				*field.out = value
			}
			cfg.Agents = append(cfg.Agents, agent)
		}
	}
	return cfg, nil
}
func asciiAllowed(value, extra string) bool {
	for _, b := range []byte(value) {
		if !((b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || strings.IndexByte(extra, b) >= 0) {
			return false
		}
	}
	return true
}
func parseDisk(value string) (uint32, error) {
	value = strings.Trim(strings.TrimSpace(value), "\"")
	factor := uint64(1)
	if strings.HasSuffix(value, "g") || strings.HasSuffix(value, "G") {
		value = value[:len(value)-1]
		factor = 1024
	}
	number, err := strconv.ParseUint(strings.TrimPrefix(value, "+"), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid disk_total: %w", err)
	}
	if number == 0 || number > math.MaxUint32/factor {
		return 0, fmt.Errorf("disk_total must be positive and fit in MiB")
	}
	return uint32(number * factor), nil
}
func validateDeployPath(value string, absolute bool) error {
	invalid := value == "" || (absolute && !strings.HasPrefix(value, "/")) || !asciiAllowed(value, "/._-")
	for _, part := range strings.Split(value, "/") {
		invalid = invalid || part == ".."
	}
	if invalid {
		return fmt.Errorf("deployment paths must use letters, digits, '/', '.', '_' or '-' and no parent traversal; filesystem paths must be absolute")
	}
	return nil
}
func validateDeployConfig(cfg deployConfig) error {
	if err := validateDeployPath(cfg.General.Prefix, true); err != nil {
		return err
	}
	user := func(value string) error {
		if value == "" || strings.HasPrefix(value, "-") || !asciiAllowed(value, "_-") {
			return fmt.Errorf("invalid deployment user")
		}
		return nil
	}
	peer := func(host, name, listen string) error {
		if err := user(name); err != nil {
			return err
		}
		if host == "" || strings.HasPrefix(host, "-") || !asciiAllowed(host, ".:-[]") {
			return fmt.Errorf("invalid SSH host")
		}
		if _, err := netip.ParseAddrPort(listen); err != nil {
			return fmt.Errorf("listen must be an IP address and port: %w", err)
		}
		return nil
	}
	if err := user(cfg.General.User); err != nil {
		return err
	}
	if controller := cfg.Controller; controller != nil {
		if err := peer(controller.Host, controller.SSHUser, controller.Listen); err != nil {
			return err
		}
		if controller.DataDir != nil {
			if err := validateDeployPath(*controller.DataDir, true); err != nil {
				return err
			}
		}
	}
	for _, agent := range cfg.Agents {
		if err := peer(agent.Host, agent.SSHUser, agent.Listen); err != nil {
			return err
		}
		for _, path := range []*string{agent.ImageDir, agent.RuntimeDir} {
			if path == nil {
				continue
			}
			if err := validateDeployPath(*path, agent.Storage == "file"); err != nil {
				return err
			}
			if agent.Storage == "zvol" && strings.HasPrefix(*path, "/") {
				return fmt.Errorf("zvol storage requires dataset names")
			}
		}
		if agent.HostID != nil {
			if err := model.ValidateName(*agent.HostID, "host_id"); err != nil {
				return err
			}
		}
		if agent.ContainerRuntime != nil {
			if _, err := engine.ParseContainerRuntime(*agent.ContainerRuntime); err != nil {
				return err
			}
		}
	}
	return nil
}
