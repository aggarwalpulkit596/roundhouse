// Command rh is Roundhouse: a container engine and deployment platform built
// from Linux primitives, for learning how platforms like Railway work.
//
// One binary plays every role, selected by argv[1]:
//
//	rh <command>        the CLI
//	rh daemon           the provisioning engine (API + reconciler)
//	rh registry         an OCI distribution registry
//	rh __init / __exec  container init and exec helpers (internal)
//	rh __shim           per-container supervisor (internal)
package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/container"
	_ "github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/nsenter"
	"github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/runtime"
)

type command struct {
	summary string
	run     func(args []string) error
	group   string
}

var commands = map[string]command{}

func register(name, group, summary string, run func([]string) error) {
	commands[name] = command{summary: summary, run: run, group: group}
}

func main() {
	if len(os.Args) > 1 {
		// Internal re-exec entry points must be dispatched before anything
		// else runs: they execute inside a half-built container.
		switch os.Args[1] {
		case runtime.InitArg:
			runtime.Init()
			os.Exit(1) // Init only returns on failure it could not report
		case runtime.ExecArg:
			runtime.ExecMain()
			return
		case container.ShimArg:
			if len(os.Args) != 4 {
				fmt.Fprintln(os.Stderr, "usage: rh __shim ROOT ID")
				os.Exit(2)
			}
			container.ShimMain(os.Args[2], os.Args[3])
			return
		}
	}
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		usage()
		return
	}
	name := os.Args[1]
	cmd, ok := commands[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "rh: unknown command %q\n\n", name)
		usage()
		os.Exit(2)
	}
	if err := cmd.run(os.Args[2:]); err != nil {
		if e, ok := err.(exitError); ok {
			os.Exit(int(e))
		}
		fmt.Fprintln(os.Stderr, "rh:", err)
		os.Exit(1)
	}
}

// exitError propagates a container's exit status as rh's own.
type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func usage() {
	fmt.Println(`Roundhouse — a container engine and deploy platform built from Linux primitives.

Usage: rh <command> [flags]`)
	groups := map[string][]string{}
	for n, c := range commands {
		groups[c.group] = append(groups[c.group], n)
	}
	for _, g := range []string{"Containers", "Images", "Build & ship", "Platform"} {
		names := groups[g]
		sort.Strings(names)
		fmt.Printf("\n%s:\n", g)
		for _, n := range names {
			fmt.Printf("  %-10s %s\n", n, commands[n].summary)
		}
	}
	fmt.Println(`
Environment:
  RH_ROOT     state directory (default /var/lib/roundhouse)
  RH_HOST     daemon address for platform commands (default unix:///run/roundhouse.sock)

Run 'rh <command> -h' for command flags.`)
}

func stateRoot() string {
	if r := os.Getenv("RH_ROOT"); r != "" {
		return r
	}
	return "/var/lib/roundhouse"
}

func manager() (*container.Manager, error) {
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("this command needs root (namespaces, mounts and cgroups are privileged)")
	}
	return container.NewManager(stateRoot())
}

// stringList is a repeatable flag (-e A=1 -e B=2).
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }
