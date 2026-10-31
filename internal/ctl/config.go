// Package ctl implements the TTstack controller.
package ctl

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/aisidev/tt/internal/commandline"
)

//go:embed help.txt
var help string

type Config struct {
	EnrollmentDirectory string
	AuthorityFile       string
	Listen              string
	DataDir             string
	APIKey              string
	keyPresent          bool
}

func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "enrollment-token" {
		return enrollmentTokenCLI(args[1:], stdout, stderr)
	}
	var cfg Config
	fs := flag.NewFlagSet("tt-ctl", flag.ContinueOnError)
	fs.StringVar(&cfg.Listen, "listen", "0.0.0.0:9200", "Listen address for the HTTP API")
	fs.StringVar(&cfg.EnrollmentDirectory, "enrollment-grants", "", "Private generated enrollment grant directory outside database restore scope")
	fs.StringVar(&cfg.AuthorityFile, "authority-file", "", "Private deployment authority file outside database restore scope")
	fs.StringVar(&cfg.DataDir, "data-dir", "/home/ttstack/ctl", "Directory for persistent state")
	fs.StringVar(&cfg.APIKey, "api-key", os.Getenv("TT_API_KEY"), "API key for authentication")
	fs.SetOutput(stderr)
	var showHelp, version bool
	fs.BoolVar(&showHelp, "h", false, "Print help")
	fs.BoolVar(&showHelp, "help", false, "Print help")
	fs.BoolVar(&version, "V", false, "Print version")
	fs.BoolVar(&version, "version", false, "Print version")
	fs.Usage = func() { fmt.Fprint(stderr, help) }
	if err := commandline.CheckFlags(fs, args); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if showHelp {
		fmt.Fprint(stdout, help)
		return 0
	}
	if version {
		fmt.Fprintf(stdout, "tt-ctl %s\n", commandline.Version)
		return 0
	}
	if len(fs.Args()) > 0 {
		fmt.Fprintf(stderr, "error: unexpected argument '%s' found\n", fs.Args()[0])
		return 2
	}
	_, cfg.keyPresent = os.LookupEnv("TT_API_KEY")
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "api-key" {
			cfg.keyPresent = true
		}
	})
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return serveController(ctx, cfg, stderr)
}
