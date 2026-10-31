package cli

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aisidev/tt/model"
)

type sshTarget struct {
	user, host string
	port       uint16
	deployer   *deployer
}

func redactDeploymentOutput(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if at := strings.Index(line, "TT_API_KEY="); at >= 0 {
			lines[i] = line[:at] + "TT_API_KEY=<redacted>"
		}
	}
	return strings.Join(lines, "\n")
}
func (t sshTarget) exec(ctx context.Context, script string) (string, error) {
	command := exec.Command("ssh", "-o", "StrictHostKeyChecking=accept-new", "-o", "ConnectTimeout=10", "-p", strconv.Itoa(int(t.port)), t.user+"@"+t.host, "sh -s")
	command.Stdin = strings.NewReader(script)
	out, err := t.deployer.execute(ctx, command, 900*time.Second)
	if err != nil {
		return "", fmt.Errorf("SSH deployment timed out or failed; inspect service state before retrying: %w", err)
	}
	if !out.Success() {
		return "", fmt.Errorf("ssh command failed on %s: %s %s", t.host, redactDeploymentOutput(string(out.Stdout)), redactDeploymentOutput(string(out.Stderr)))
	}
	return strings.ToValidUTF8(string(out.Stdout), "\ufffd"), nil
}
func (t sshTarget) copy(ctx context.Context, local, remote string) error {
	command := exec.Command("scp", "-o", "StrictHostKeyChecking=accept-new", "-o", "ConnectTimeout=10", "-P", strconv.Itoa(int(t.port)), local, t.user+"@"+t.host+":"+remote)
	out, err := t.deployer.execute(ctx, command, 0)
	if err != nil {
		return fmt.Errorf("scp failed: %w", err)
	}
	if !out.Success() {
		return fmt.Errorf("scp to %s failed: %s", t.host, out.Stderr)
	}
	digest, err := fileDigest(local, false)
	if err != nil {
		return err
	}
	_, err = t.exec(ctx, fmt.Sprintf("printf '%%s  %%s\\n' '%s' '%s' | sha256sum -c -", digest, remote))
	return err
}
func (t sshTarget) staging(ctx context.Context) (string, error) {
	value, err := t.exec(ctx, "umask 077; mktemp -d /tmp/ttstack-deploy.XXXXXXXXXX")
	if err != nil {
		return "", err
	}
	path := strings.TrimSpace(value)
	if err := validateDeployPath(path, true); err != nil {
		return "", err
	}
	const prefix = "/tmp/ttstack-deploy."
	if !strings.HasPrefix(path, prefix) || strings.Contains(strings.TrimPrefix(path, prefix), "/") {
		return "", fmt.Errorf("invalid remote staging directory")
	}
	return path, nil
}
func (d *deployer) distributed(ctx context.Context, path string) error {
	content, err := readUTF8(path, "read config")
	if err != nil {
		return err
	}
	cfg, err := parseDeployConfig(content)
	if err != nil {
		return fmt.Errorf("parse deploy.toml: %w", err)
	}
	if err := validateDeployConfig(cfg); err != nil {
		return err
	}
	for _, agent := range cfg.Agents {
		if _, err := parseDisk(agent.DiskTotal); err != nil {
			return err
		}
		if _, err := model.ParseStorage(agent.Storage); err != nil {
			return err
		}
	}
	release, prefix, user := cfg.General.ReleaseDir, cfg.General.Prefix, cfg.General.User
	home := "/home/" + user
	sidecar := path + ".api-key"
	var key string
	if cfg.General.APIKey != nil {
		key, err = parseDeploymentKey(*cfg.General.APIKey)
		if err != nil {
			return err
		}
		if err := saveConfigFile(sidecar, key+"\n"); err != nil {
			return fmt.Errorf("API key staging file: %w", err)
		}
	} else if fileExists(sidecar) {
		content, err := readUTF8(sidecar, "read deployment key")
		if err != nil {
			return err
		}
		key, err = parseDeploymentKey(content)
		if err != nil {
			return err
		}
	} else {
		var target sshTarget
		if controller := cfg.Controller; controller != nil {
			target = sshTarget{controller.SSHUser, controller.Host, controller.SSHPort, d}
		} else if len(cfg.Agents) > 0 {
			agent := cfg.Agents[0]
			target = sshTarget{agent.SSHUser, agent.Host, agent.SSHPort, d}
		} else {
			return fmt.Errorf("deploy config has no controller or agents")
		}
		// Reusing a key while agent and controller disagree would afterwards
		// cement the split by writing the chosen side into every new role, so
		// the probe refuses instead of silently preferring the controller.
		script := fmt.Sprintf("sudo sh -c 'ctl=%s/etc/tt-ctl.env; agt=%s/etc/tt-agent.env; if [ -f \"$ctl\" ] && [ -f \"$agt\" ] && ! cmp -s \"$ctl\" \"$agt\"; then echo \"existing agent/controller keys differ; put the intended shared key in deploy.toml general.api_key\" >&2; exit 1; fi; if [ -f \"$ctl\" ]; then cat \"$ctl\"; elif [ -f \"$agt\" ]; then cat \"$agt\"; fi'", prefix, prefix)
		previous, err := target.exec(ctx, script)
		if err != nil {
			return err
		}
		if strings.TrimSpace(previous) == "" {
			key = generateAPIKey()
		} else {
			key, err = parseDeploymentKey(previous)
			if err != nil {
				return err
			}
		}
		if err := saveSecret(sidecar, key); err != nil {
			return err
		}
	}
	// Authority and its activation sidecars belong to external operator control.
	// Deployment observes an existing private file, never initializes or replaces it.
	if controller := cfg.Controller; controller != nil && controller.AuthorityFile != nil {
		if err := d.checkRemoteAuthority(ctx, sshTarget{controller.SSHUser, controller.Host, controller.SSHPort, d}, *controller.AuthorityFile); err != nil {
			return err
		}
	}
	for _, agent := range cfg.Agents {
		if agent.AuthorityFile != nil {
			if err := d.checkRemoteAuthority(ctx, sshTarget{agent.SSHUser, agent.Host, agent.SSHPort, d}, *agent.AuthorityFile); err != nil {
				return err
			}
		}
	}
	environment := "TT_API_KEY=" + key + "\n"
	if controller := cfg.Controller; controller != nil {
		d.app.out("\n[deploy] === %s (controller) ===", controller.Host)
		target := sshTarget{controller.SSHUser, controller.Host, controller.SSHPort, d}
		tmp, err := target.staging(ctx)
		if err != nil {
			return err
		}
		for _, name := range []string{"tt-ctl", "tt"} {
			binary := filepath.Join(release, name)
			if !fileExists(binary) {
				return fmt.Errorf("%s not found (run 'make release')", binary)
			}
			if err := target.copy(ctx, filepath.Join(release, name), tmp+"/"+name); err != nil {
				return err
			}
			d.app.out("[deploy] uploaded %s", name)
		}
		dataDir := home + "/ctl"
		if controller.DataDir != nil {
			dataDir = *controller.DataDir
		}
		command := fmt.Sprintf("%s/bin/tt-ctl --listen %s --data-dir %s", prefix, controller.Listen, dataDir)
		if controller.AuthorityFile != nil {
			command += " --authority-file " + *controller.AuthorityFile
		}

		// The unprivileged controller must own its explicitly configured state
		// directory before systemd starts it. Existing contents are not rewritten.
		var ownedDirs []string
		if dataDir != home+"/ctl" {
			ownedDirs = []string{dataDir}
		}
		script := remoteSetupScript("tt-ctl", command, user, home, prefix, tmp, []string{"tt-ctl", "tt"}, []string{dataDir}, controller.Host, &envFile{prefix + "/etc/tt-ctl.env", environment}, ownedDirs...)
		result, deployErr := target.exec(ctx, script)
		_, cleanupErr := target.exec(ctx, "rm -rf -- "+tmp)
		if deployErr != nil {
			return deployErr
		}
		if cleanupErr != nil {
			// The service is installed and running; a leftover staging
			// directory must not downgrade the outcome to failure.
			d.app.diagnostic("[deploy] warning: staging cleanup on %s failed: %v", controller.Host, cleanupErr)
		}
		fmt.Fprint(d.app.stdout, result)
	}
	for index, agent := range cfg.Agents {
		d.app.out("\n[deploy] === %s (agent %d) ===", agent.Host, index)
		target := sshTarget{agent.SSHUser, agent.Host, agent.SSHPort, d}
		tmp, err := target.staging(ctx)
		if err != nil {
			return err
		}
		agentRelease := release
		if agent.ReleaseDir != nil {
			agentRelease = *agent.ReleaseDir
		}
		binary := filepath.Join(agentRelease, "tt-agent")
		if !fileExists(binary) {
			return fmt.Errorf("%s not found (run 'make release')", binary)
		}
		if err := target.copy(ctx, binary, tmp+"/tt-agent"); err != nil {
			return err
		}
		d.app.out("[deploy] uploaded tt-agent")
		imageDir, runtimeDir := home+"/images", home+"/runtime"
		if agent.ImageDir != nil {
			imageDir = *agent.ImageDir
		}
		if agent.RuntimeDir != nil {
			runtimeDir = *agent.RuntimeDir
		}
		disk, err := parseDisk(agent.DiskTotal)
		if err != nil {
			return err
		}
		command := fmt.Sprintf("%s/bin/tt-agent --listen %s --image-dir %s --runtime-dir %s --data-dir %s/data --storage %s --cpu-total %d --mem-total %d --disk-total %d", prefix, agent.Listen, imageDir, runtimeDir, home, agent.Storage, agent.CPUTotal, agent.MemTotal, disk)
		if agent.AuthorityFile != nil {
			command += " --authority-file " + *agent.AuthorityFile
		}
		if agent.HostID != nil {
			command += " --host-id " + *agent.HostID
		}
		if agent.ContainerRuntime != nil {
			command += " --container-runtime " + *agent.ContainerRuntime
		}
		if agent.EnableDiskBackup {
			command += " --enable-disk-backup"
		}
		dirs := []string{}
		if agent.Storage == "file" {
			dirs = []string{imageDir, runtimeDir}
		}
		script := remoteSetupScript("tt-agent", command, user, home, prefix, tmp, []string{"tt-agent"}, dirs, agent.Host, &envFile{prefix + "/etc/tt-agent.env", environment})
		result, deployErr := target.exec(ctx, script)
		_, cleanupErr := target.exec(ctx, "rm -rf -- "+tmp)
		if deployErr != nil {
			return deployErr
		}
		if cleanupErr != nil {
			d.app.diagnostic("[deploy] warning: staging cleanup on %s failed: %v", agent.Host, cleanupErr)
		}
		fmt.Fprint(d.app.stdout, result)
	}
	d.app.out("\n[deploy] distributed deployment complete")
	d.app.out("[deploy] API key stored in %s (0600). Configure the CLI using TT_API_KEY.", sidecar)
	if controller := cfg.Controller; controller != nil {
		parts := strings.Split(controller.Listen, ":")
		port := parts[len(parts)-1]
		d.app.out("[deploy] Run with TT_API_KEY set: tt config %s:%s", controller.Host, port)
	}
	return nil
}

func (d *deployer) checkRemoteAuthority(ctx context.Context, target sshTarget, path string) error {
	// Paths have already passed strict shell/unit validation. readlink/type and
	// mode checks establish only a selected input, never serving activation.
	script := "sudo test -f " + path + " && sudo test ! -L " + path + " && sudo sh -c 'mode=$(stat -c %a " + path + "); test \"$mode\" = 600 || test \"$mode\" = 400'"
	_, err := target.exec(ctx, script)
	return err
}
