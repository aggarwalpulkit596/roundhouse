package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/api"
	"github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/engine"
	"github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/scheduler"
)

func init() {
	register("schedule", "Platform", "Explain where replicas or builds would be placed across nodes", cmdSchedule)
}

func cmdSchedule(args []string) error {
	fs := flag.NewFlagSet("schedule", flag.ExitOnError)
	nodeSpec := fs.String("nodes", "", "simulated nodes NAME:CPUS:MEMMB[:ALLOCCPU:ALLOCMB[:ZONE]],... ")
	agents := fs.String("agents", "", "real daemons to query, comma separated (unix:///run/roundhouse.sock,http://host:7070)")
	service := fs.String("service", "web", "service name (for anti-affinity)")
	cpu := fs.Float64("cpu", 0.5, "CPU per replica")
	mem := fs.Int("memory", 256, "MB per replica")
	replicas := fs.Int("replicas", 3, "replicas to place")
	strategy := fs.String("strategy", "binpack", "binpack or spread")
	buildKey := fs.String("build-key", "", "instead of replicas: route this build key (e.g. a repo URL) to a builder")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage: rh schedule [flags]

Examples:
  rh schedule --nodes a:8:16384:6:12000:z1,b:8:16384:1:2000:z1,c:8:16384:2:4000:z2 --replicas 3
  rh schedule --nodes a:8:16384,b:8:16384 --strategy spread
  rh schedule --agents unix:///run/roundhouse.sock --replicas 2
  rh schedule --nodes b1:1:1,b2:1:1,b3:1:1,b4:1:1,b5:1:1 --build-key github.com/acme/api`)
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	var nodes []scheduler.Node
	if *nodeSpec != "" {
		for _, part := range strings.Split(*nodeSpec, ",") {
			f := strings.Split(part, ":")
			if len(f) < 3 {
				return fmt.Errorf("bad node %q (want NAME:CPUS:MEMMB[:ALLOCCPU:ALLOCMB[:ZONE]])", part)
			}
			n := scheduler.Node{Name: f[0]}
			n.CPUs, _ = strconv.ParseFloat(f[1], 64)
			n.MemoryMB, _ = strconv.Atoi(f[2])
			if len(f) >= 5 {
				n.AllocatedCPU, _ = strconv.ParseFloat(f[3], 64)
				n.AllocatedMB, _ = strconv.Atoi(f[4])
			}
			if len(f) >= 6 {
				n.Zone = f[5]
			}
			nodes = append(nodes, n)
		}
	}
	for _, a := range strings.Split(*agents, ",") {
		if a == "" {
			continue
		}
		var info engine.NodeInfo
		if err := api.New(a).Do(context.Background(), http.MethodGet, "/v1/node", nil, &info); err != nil {
			return fmt.Errorf("%s: %w", a, err)
		}
		nodes = append(nodes, scheduler.Node{Name: info.Name, CPUs: float64(info.CPUs), MemoryMB: info.MemoryMB, AllocatedCPU: info.AllocatedCPU, AllocatedMB: info.AllocatedMB})
	}
	if len(nodes) == 0 {
		fs.Usage()
		return exitError(2)
	}

	if *buildKey != "" {
		var bs []scheduler.Builder
		for _, n := range nodes {
			bs = append(bs, scheduler.Builder{Name: n.Name, Load: int(n.AllocatedCPU)})
		}
		pick, cands, err := scheduler.PickBuilder(*buildKey, bs, 3)
		if err != nil {
			return err
		}
		names := make([]string, len(nodes))
		for i, n := range nodes {
			names[i] = n.Name
		}
		fmt.Printf("rendezvous order for %q: %s\n", *buildKey, strings.Join(scheduler.Rank(*buildKey, names), " > "))
		fmt.Printf("candidates (warm caches): %s\nchosen (least loaded candidate): %s\n", strings.Join(cands, ", "), pick)
		return nil
	}

	d, err := scheduler.Place(nodes, scheduler.Request{Service: *service, CPU: *cpu, MemoryMB: *mem, Replicas: *replicas}, scheduler.Strategy(*strategy))
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tZONE\tCPU (alloc/cap)\tMEMORY MB (alloc/cap)\tFILTER")
	for _, n := range nodes {
		why := d.Rejected[n.Name]
		if why == "" {
			why = "fits"
		}
		fmt.Fprintf(tw, "%s\t%s\t%.2f/%.0f\t%d/%d\t%s\n", n.Name, n.Zone, n.AllocatedCPU, n.CPUs, n.AllocatedMB, n.MemoryMB, why)
	}
	tw.Flush()
	fmt.Println()
	for _, a := range d.Assignments {
		fmt.Printf("replica %d -> %s (score %.3f)\n", a.Replica, a.Node, a.Score)
	}
	return err
}
