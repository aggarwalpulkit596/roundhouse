// Package scheduler decides where work runs across many nodes. The engine
// package runs one node; this is the piece a multi-node control plane puts
// in front of many engines.
//
// Two problems, two classic algorithms:
//
//   - Placing replicas of a service: filter nodes that fit, then score them.
//     Bin-packing (Kubernetes' MostAllocated) fills nodes up so whole machines
//     can be drained or never bought; spreading (LeastAllocated) keeps
//     headroom everywhere. Anti-affinity pushes replicas of one service onto
//     different nodes and zones, because two replicas on one host are one
//     failure away from zero.
//
//   - Routing builds to builders: rendezvous (highest-random-weight)
//     hashing sends the same repository to the same few builders, so their
//     layer caches stay warm, and when a builder disappears only the
//     repositories it owned move. Railway's builder describes the same idea
//     as "1-of-3 on the ring": hash to three candidates, pick the least
//     loaded.
package scheduler

import (
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strings"
)

// Node is a machine's capacity and current reservations.
type Node struct {
	Name          string
	Zone          string
	CPUs          float64
	MemoryMB      int
	AllocatedCPU  float64
	AllocatedMB   int
	Unschedulable bool
	// Replicas of each service already on the node.
	Services map[string]int
}

// Request asks for replicas of one service.
type Request struct {
	Service  string
	CPU      float64
	MemoryMB int
	Replicas int
}

// Strategy selects the scoring function.
type Strategy string

const (
	BinPack Strategy = "binpack"
	Spread  Strategy = "spread"
)

// Assignment places one replica.
type Assignment struct {
	Replica int
	Node    string
	Score   float64
}

// Decision is a full placement with the reasoning, so it can be explained.
type Decision struct {
	Assignments []Assignment
	// Rejected lists why nodes were filtered out (for the first replica).
	Rejected map[string]string
}

// Place assigns every replica, one at a time, updating reservations as it
// goes so later replicas see earlier ones.
func Place(nodes []Node, req Request, strat Strategy) (Decision, error) {
	if req.Replicas <= 0 {
		req.Replicas = 1
	}
	work := make([]Node, len(nodes))
	for i, n := range nodes {
		work[i] = n
		work[i].Services = map[string]int{}
		for k, v := range n.Services {
			work[i].Services[k] = v
		}
	}
	d := Decision{Rejected: map[string]string{}}
	for r := 0; r < req.Replicas; r++ {
		best, bestScore := -1, math.Inf(-1)
		for i := range work {
			n := &work[i]
			if why := fits(n, req); why != "" {
				if r == 0 {
					d.Rejected[n.Name] = why
				}
				continue
			}
			s := score(n, req, work, strat)
			if s > bestScore || (s == bestScore && best >= 0 && n.Name < work[best].Name) {
				best, bestScore = i, s
			}
		}
		if best < 0 {
			return d, fmt.Errorf("replica %d of %s does not fit on any node (%s)", r, req.Service, summarize(d.Rejected))
		}
		n := &work[best]
		n.AllocatedCPU += req.CPU
		n.AllocatedMB += req.MemoryMB
		n.Services[req.Service]++
		d.Assignments = append(d.Assignments, Assignment{Replica: r, Node: n.Name, Score: round(bestScore)})
	}
	return d, nil
}

// fits is the filter phase: hard constraints only.
func fits(n *Node, req Request) string {
	switch {
	case n.Unschedulable:
		return "cordoned"
	case n.CPUs-n.AllocatedCPU < req.CPU:
		return fmt.Sprintf("needs %.2f CPU, %.2f free", req.CPU, n.CPUs-n.AllocatedCPU)
	case n.MemoryMB-n.AllocatedMB < req.MemoryMB:
		return fmt.Sprintf("needs %d MB, %d free", req.MemoryMB, n.MemoryMB-n.AllocatedMB)
	}
	return ""
}

// score is the ranking phase: soft preferences, higher is better.
func score(n *Node, req Request, all []Node, strat Strategy) float64 {
	cpu := safeDiv(n.AllocatedCPU+req.CPU, n.CPUs)
	mem := safeDiv(float64(n.AllocatedMB+req.MemoryMB), float64(n.MemoryMB))
	util := (cpu + mem) / 2
	s := util
	if strat == Spread {
		s = 1 - util
	}
	// Anti-affinity: each existing replica of this service on the node, and
	// in the node's zone, costs more than any utilization difference.
	s -= 1.0 * float64(n.Services[req.Service])
	if n.Zone != "" {
		inZone := 0
		for _, o := range all {
			if o.Zone == n.Zone {
				inZone += o.Services[req.Service]
			}
		}
		s -= 0.5 * float64(inZone)
	}
	return s
}

func safeDiv(a, b float64) float64 {
	if b <= 0 {
		return 1
	}
	return a / b
}

func round(f float64) float64 { return math.Round(f*1000) / 1000 }

func summarize(m map[string]string) string {
	var parts []string
	for k, v := range m {
		parts = append(parts, k+": "+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

// ---------------------------------------------------------------------------
// rendezvous hashing

// Rank orders nodes by highest random weight for key: hash(node, key). Every
// caller computes the same order with no shared state, and removing a node
// only changes the answer for keys whose top choice it was.
func Rank(key string, nodes []string) []string {
	type w struct {
		node string
		h    uint64
	}
	ws := make([]w, len(nodes))
	for i, n := range nodes {
		h := fnv.New64a()
		h.Write([]byte(n))
		h.Write([]byte{0})
		h.Write([]byte(key))
		ws[i] = w{n, mix(h.Sum64())}
	}
	sort.Slice(ws, func(i, j int) bool {
		if ws[i].h != ws[j].h {
			return ws[i].h > ws[j].h
		}
		return ws[i].node < ws[j].node
	})
	out := make([]string, len(ws))
	for i := range ws {
		out[i] = ws[i].node
	}
	return out
}

// mix is a 64-bit finalizer (splitmix64) that spreads FNV's output.
func mix(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

// Builder is a build node and its current load (running builds).
type Builder struct {
	Name string
	Load int
}

// PickBuilder chooses among the key's top `candidates` builders by
// rendezvous rank, taking the least loaded (ties go to the higher-ranked,
// warmest cache).
func PickBuilder(key string, builders []Builder, candidates int) (string, []string, error) {
	if len(builders) == 0 {
		return "", nil, errors.New("no builders")
	}
	if candidates <= 0 {
		candidates = 3
	}
	names := make([]string, len(builders))
	load := map[string]int{}
	for i, b := range builders {
		names[i] = b.Name
		load[b.Name] = b.Load
	}
	ranked := Rank(key, names)
	if len(ranked) > candidates {
		ranked = ranked[:candidates]
	}
	best := ranked[0]
	for _, n := range ranked[1:] {
		if load[n] < load[best] {
			best = n
		}
	}
	return best, ranked, nil
}
