// Package cli implements the TTstack command-line client.
package cli

import (
	"embed"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aisidev/tt/internal/commandline"
)

//go:embed help/*.txt
var helpFiles embed.FS

func helpText(path string) string {
	name := strings.ReplaceAll(path, " ", "-")
	if name == "" {
		name = "root"
	}
	data, err := helpFiles.ReadFile("help/" + name + ".txt")
	if err != nil {
		return ""
	}
	return string(data)
}
func Run(args []string, stdout, stderr io.Writer) int {
	return newApplication(stdout, stderr).run(args)
}
func (a *application) run(args []string) int {
	p, err := parse(args)
	if err != nil {
		if errors.Is(err, errSubcommand) {
			fmt.Fprint(a.stderr, helpText(p.path))
			return 2
		}
		a.diagnostic("%v", err)
		for _, line := range strings.Split(helpText(p.path), "\n") {
			if strings.HasPrefix(line, "Usage:") {
				a.diagnostic("\n%s\n\nFor more information, try '--help'.", line)
				break
			}
		}
		return 2
	}
	if p.help {
		fmt.Fprint(a.stdout, helpText(p.path))
		return 0
	}
	if p.version {
		a.out("tt %s", commandline.Version)
		return 0
	}
	if p.path == "config" {
		path, err := a.configPath()
		if err == nil {
			err = saveConfig(path, p.argument, p.key)
		}
		if err != nil {
			a.diagnostic("Failed to save config: %v", err)
			return 1
		}
		a.out("Controller set to: %s", p.argument)
		if p.key != nil {
			a.out("API key saved.")
		}
		return 0
	}
	if strings.HasPrefix(p.path, "deploy ") {
		deploy := newDeployer(a)
		role := strings.TrimPrefix(p.path, "deploy ")
		if role == "dist" {
			err = deploy.distributed(a.ctx, p.argument)
		} else {
			err = deploy.local(a.ctx, role, p.value("release-dir"))
		}
	} else if p.path == "image recipes" {
		a.listRecipes()
		return 0
	} else if p.path == "image create" {
		builder := newImageBuilder(a)
		if p.argument == "all" {
			var engine *string
			if p.has("engine") {
				value := p.value("engine")
				engine = &value
			}
			err = builder.createAll(a.ctx, p.value("image-dir"), engine)
		} else {
			if p.has("engine") {
				err = fmt.Errorf("--engine only applies to 'image create all'")
			} else {
				err = builder.create(a.ctx, p.argument, p.value("image-dir"))
			}
		}
	} else {
		var saved *cliConfig
		if path, pathErr := a.configPath(); pathErr == nil {
			saved = loadConfig(path)
		}
		address, key, resolveErr := resolveConfig(p.server, p.key, saved)
		if resolveErr != nil {
			a.diagnostic("%v", resolveErr)
			return 1
		}
		client, clientErr := newClient(address, key)
		if clientErr != nil {
			a.diagnostic("Cannot initialize client: %v", clientErr)
			return 1
		}
		err = a.networkCommand(client, p)
	}
	if err != nil {
		a.diagnostic("Error: %v", err)
		return 1
	}
	return 0
}
