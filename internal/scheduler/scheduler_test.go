package scheduler

import (
	"fmt"
	"testing"
)

func nodes() []Node {
	return []Node{
		{Name: "a", Zone: "z1", CPUs: 8, MemoryMB: 16384, AllocatedCPU: 6, AllocatedMB: 12000},
		{Name: "b", Zone: "z1", CPUs: 8, MemoryMB: 16384, AllocatedCPU: 1, AllocatedMB: 2000},
		{Name: "c", Zone: "z2", CPUs: 8, MemoryMB: 16384, AllocatedCPU: 2, AllocatedMB: 4000},
		{Name: "d", Zone: "z2", CPUs: 2, MemoryMB: 1024},
	}
}

func TestBinPackPrefersFullestNodeThatFits(t *testing.T) {
	d, err := Place(nodes(), Request{Service: "web", CPU: 1, MemoryMB: 1024, Replicas: 1}, BinPack)
	if err != nil {
		t.Fatal(err)
	}
	if d.Assignments[0].Node != "a" {
		t.Fatalf("binpack chose %s, want the most allocated node a", d.Assignments[0].Node)
	}
}

func TestSpreadPrefersLowestUtilizationAfterPlacement(t *testing.T) {
	// Utilization is relative: 0.5 CPU is 25% of the small idle node d but
	// only ~19% of the big, lightly used node b, so spreading picks b.
	d, err := Place(nodes(), Request{Service: "web", CPU: 0.5, MemoryMB: 256, Replicas: 1}, Spread)
	if err != nil {
		t.Fatal(err)
	}
	if d.Assignments[0].Node != "b" {
		t.Fatalf("spread chose %s, want b", d.Assignments[0].Node)
	}
}

func TestReplicasSpreadAcrossNodesAndZones(t *testing.T) {
	d, err := Place(nodes(), Request{Service: "web", CPU: 0.5, MemoryMB: 256, Replicas: 3}, BinPack)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	zones := map[string]int{}
	zoneOf := map[string]string{"a": "z1", "b": "z1", "c": "z2", "d": "z2"}
	for _, a := range d.Assignments {
		if seen[a.Node] {
			t.Fatalf("two replicas on %s while other nodes had room: %+v", a.Node, d.Assignments)
		}
		seen[a.Node] = true
		zones[zoneOf[a.Node]]++
	}
	if zones["z1"] == 0 || zones["z2"] == 0 {
		t.Fatalf("replicas should cover both zones: %+v", d.Assignments)
	}
}

func TestFilterExplainsRejections(t *testing.T) {
	ns := nodes()
	ns[1].Unschedulable = true
	d, err := Place(ns, Request{Service: "db", CPU: 4, MemoryMB: 8000, Replicas: 1}, BinPack)
	if err != nil {
		t.Fatal(err)
	}
	if d.Assignments[0].Node != "c" {
		t.Fatalf("placed on %s", d.Assignments[0].Node)
	}
	if d.Rejected["b"] != "cordoned" || d.Rejected["d"] == "" || d.Rejected["a"] == "" {
		t.Fatalf("rejections not explained: %v", d.Rejected)
	}
	if _, err := Place(ns, Request{Service: "huge", CPU: 64, Replicas: 1}, BinPack); err == nil {
		t.Fatal("an impossible request must fail")
	}
}

func TestRendezvousIsStableAndMinimallyDisruptive(t *testing.T) {
	all := []string{"b1", "b2", "b3", "b4", "b5"}
	without := []string{"b1", "b2", "b4", "b5"} // b3 removed
	moved, owned := 0, 0
	for i := 0; i < 2000; i++ {
		key := fmt.Sprintf("github.com/org/repo-%d", i)
		before := Rank(key, all)[0]
		after := Rank(key, without)[0]
		if before == "b3" {
			owned++
			continue
		}
		if before != after {
			moved++
		}
	}
	if moved != 0 {
		t.Fatalf("%d keys not owned by the removed builder changed owner", moved)
	}
	// Roughly a fifth of keys should have belonged to each builder.
	if owned < 300 || owned > 500 {
		t.Fatalf("b3 owned %d/2000 keys; distribution is skewed", owned)
	}
}

func TestPickBuilderBalancesWithinCandidates(t *testing.T) {
	bs := []Builder{{"b1", 0}, {"b2", 0}, {"b3", 0}, {"b4", 0}, {"b5", 0}}
	key := "github.com/org/api"
	first, cands, _ := PickBuilder(key, bs, 3)
	if first != cands[0] {
		t.Fatal("with equal load the warmest (top-ranked) builder wins")
	}
	for i := range bs {
		if bs[i].Name == cands[0] {
			bs[i].Load = 10
		}
	}
	second, _, _ := PickBuilder(key, bs, 3)
	if second == first || (second != cands[1] && second != cands[2]) {
		t.Fatalf("a busy top builder should shed to another candidate, got %s (candidates %v)", second, cands)
	}
}
