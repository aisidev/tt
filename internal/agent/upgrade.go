package agent

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/aisidev/tt/internal/commandline"
	"io"
)

const upgradeHelp = "Copy stopped-agent v5 metadata to a new v6 database without replacing the source\n\nUsage: upgrade-agent-v5 --source <SOURCE> --output <OUTPUT>\n\nOptions:\n      --source <SOURCE>\n      --output <OUTPUT>\n  -h, --help             Print help\n"

func RunUpgrade(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("upgrade-agent-v5", flag.ContinueOnError)
	fs.SetOutput(stderr)
	source := fs.String("source", "", "Source database")
	output := fs.String("output", "", "New destination database")
	fs.Usage = func() { fmt.Fprint(stdout, upgradeHelp) }
	var showHelp bool
	fs.BoolVar(&showHelp, "h", false, "Print help")
	fs.BoolVar(&showHelp, "help", false, "Print help")
	if err := commandline.CheckFlags(fs, args); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if showHelp {
		fmt.Fprint(stdout, upgradeHelp)
		return 0
	}
	present := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { present[f.Name] = true })
	if !present["source"] || !present["output"] {
		fmt.Fprintln(stderr, "error: the following required arguments were not provided:")
		for _, name := range []string{"source", "output"} {
			if !present[name] {
				label := "SOURCE"
				if name == "output" {
					label = "OUTPUT"
				}
				fmt.Fprintf(stderr, "  --%s <%s>\n", name, label)
			}
		}
		fmt.Fprint(stderr, "\nUsage: upgrade-agent-v5 --source <SOURCE> --output <OUTPUT>\n\nFor more information, try '--help'.\n")
		return 2
	}
	if len(fs.Args()) != 0 {
		fmt.Fprintf(stderr, "error: unexpected argument '%s' found\n\nUsage: upgrade-agent-v5 --source <SOURCE> --output <OUTPUT>\n\nFor more information, try '--help'.\n", fs.Args()[0])
		return 2
	}
	count, err := ConvertV5(context.Background(), *source, *output)
	if err != nil {
		fmt.Fprintf(stderr, "Offline conversion failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Converted %d VM records to a new v6 database; source and runtime disks were not changed\n", count)
	return 0
}
