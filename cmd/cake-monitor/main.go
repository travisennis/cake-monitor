// Command cake-monitor records background events and delivers them to cake.
package main

import (
	"os"
	"path/filepath"

	"github.com/travisennis/cake-monitor/internal/cli"
	"github.com/travisennis/cake-monitor/internal/toolbox"
)

func main() {
	args := os.Args[1:]
	// The detached watcher is re-executed under a toolbox call, so its
	// environment may carry TOOLBOX_ACTION. Dispatch it before anything else.
	if len(args) > 0 && args[0] == "__watch" {
		os.Exit(cli.Run(args, os.Stdin, os.Stdout, os.Stderr))
	}
	if name, ok := toolbox.Name(filepath.Base(os.Args[0])); ok {
		os.Exit(cli.RunTool(name, os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(cli.Run(args, os.Stdin, os.Stdout, os.Stderr))
}
