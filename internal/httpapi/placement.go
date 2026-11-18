package httpapi

import (
	"math"
	"sort"
)

const hostnameTopologyKey = "kubernetes.io/hostname"

type podDemand struct {
	cpuMilli, memoryBytes int64
	// workload ("namespace/kind/name") groups replicas and names the owner
	// for unplaced-pod reporting; constraints restrict where the pod runs.
	workload    string
	namespace   string
	constraints SchedulingConstraints
	// pinnedNode is the node a PodDisruptionBudget keeps this pod on.
	pinnedNode string
}

type packingResult struct {
	NodesNeeded    int   `json:"nodesNeeded"`
	UnplacedPods   int   `json:"unplacedPods"`
	CPURequested   int64 `json:"cpuRequestedMilli"`
	MemoryRequests int64 `json:"memoryRequestedBytes"`
	// NoEligibleNodePods could not use any schedulable node because of
	// taints, nodeSelector, or required node affinity, whatever their size.
	NoEligibleNodePods int `json:"noEligibleNodePods"`
	// PodRulePods had room on an eligible node but were kept off it by pod
	// affinity, anti-affinity, or topology spread.
	PodRulePods int `json:"podRulePods"`
	// PinnedPods stay on their current nodes because a PodDisruptionBudget
	// allows no voluntary evictions.
	PinnedPods int `json:"pinnedPods"`
	// UnplacedWorkloads names up to 20 workloads with unplaced pods.
	UnplacedWorkloads []string `json:"unplacedWorkloads"`
	// nodesByGroup counts used nodes by nodeGroupKey.
	nodesByGroup map[string]int
}

// selection is a pod rule resolved to the workloads its selector matches.
type selection struct {
	key     string
	matched []int
}

type affinityRule struct {
	selection
	// self is set when the rule matches its own workload, which lets the
	// first matching pod start anywhere, as the scheduler does.
	self bool
}

type spreadRule struct {
	selection
	maxSkew int
	// perNode caps matching pods per node for the hostname key.
	perNode int
	// domains are the other keys' eligible domain values.
	domains []string
}

type workloadRules struct {
	namespace   string
	constraints SchedulingConstraints
	replicas    int
	anti        []selection
	// antiBy holds other workloads' anti-affinity rules that select this one;
	// required anti-affinity is symmetric.
	antiBy   []selection
	affinity []affinityRule
	spread   []spreadRule
	// dependent is set when a required affinity rule needs another
	// workload's pods, so this workload is placed after the others.
	dependent bool
}

// placement tracks which workloads' pods occupy each topology domain.
type placement struct {
	workloads []workloadRules
	keys      []string
	counts    map[string]map[int]int
	totals    []int
}

func domainValue(node NodeCapacity, key string) (string, bool) {
	if value, found := node.Labels[key]; found {
		return value, true
	}
	if key == hostnameTopologyKey {
		return node.Name, true
	}
	return "", false
}

func newPlacement(nodes []NodeCapacity, pods []podDemand) (*placement, []int) {
	state := &placement{counts: map[string]map[int]int{}}
	groups := make([]int, len(pods))
	positions := map[string]int{}
	for index, pod := range pods {
		position, found := positions[pod.workload]
		if !found || pod.workload == "" {
			position = len(state.workloads)
			positions[pod.workload] = position
			state.workloads = append(state.workloads, workloadRules{namespace: pod.namespace, constraints: pod.constraints})
		}
		state.workloads[position].replicas++
		groups[index] = position
	}
	state.totals = make([]int, len(state.workloads))
	byNamespace := map[string][]int{}
	for index, workload := range state.workloads {
		byNamespace[workload.namespace] = append(byNamespace[workload.namespace], index)
	}
	keys := map[string]bool{}
	resolve := func(owner int, term PodAffinityTerm) []int {
		candidates := byNamespace[state.workloads[owner].namespace]
		if term.AllNamespaces || len(term.Namespaces) > 0 {
			candidates = make([]int, len(state.workloads))
			for index := range candidates {
				candidates[index] = index
			}
		}
		matched := []int{}
		for _, candidate := range candidates {
			target := state.workloads[candidate]
			if term.selects(state.workloads[owner].namespace, target.namespace, target.constraints.Labels) {
				matched = append(matched, candidate)
			}
		}
		return matched
	}
	for owner := range state.workloads {
		constraints := state.workloads[owner].constraints
		for _, term := range constraints.PodAntiAffinity {
			matched := resolve(owner, term)
			keys[term.TopologyKey] = true
			state.workloads[owner].anti = append(state.workloads[owner].anti, selection{term.TopologyKey, matched})
			for _, target := range matched {
				if target != owner {
					state.workloads[target].antiBy = append(state.workloads[target].antiBy, selection{term.TopologyKey, []int{owner}})
				}
			}
		}
		for _, term := range constraints.PodAffinity {
			matched := resolve(owner, term)
			keys[term.TopologyKey] = true
			rule := affinityRule{selection: selection{term.TopologyKey, matched}}
			for _, target := range matched {
				rule.self = rule.self || target == owner
				state.workloads[owner].dependent = state.workloads[owner].dependent || target != owner
			}
			state.workloads[owner].affinity = append(state.workloads[owner].affinity, rule)
		}
		for _, constraint := range constraints.TopologySpread {
			term := PodAffinityTerm{Selector: constraint.Selector, TopologyKey: constraint.TopologyKey}
			rule := spreadRule{selection: selection{constraint.TopologyKey, resolve(owner, term)}, maxSkew: constraint.MaxSkew}
			keys[constraint.TopologyKey] = true
			eligible, domains := 0, map[string]bool{}
			for _, node := range nodes {
				// The default nodeAffinityPolicy honors node rules and the
				// default nodeTaintsPolicy ignores taints.
				if value, found := domainValue(node, constraint.TopologyKey); found && node.Schedulable && constraints.selectsNode(node) {
					eligible++
					if !domains[value] {
						domains[value] = true
						rule.domains = append(rule.domains, value)
					}
				}
			}
			if constraint.TopologyKey == hostnameTopologyKey {
				// Capping each node at maxSkew keeps any packing within the
				// skew, whichever nodes remain. Only when the replicas
				// outnumber maxSkew per eligible node must nodes hold more.
				replicas := 0
				for _, target := range rule.matched {
					replicas += state.workloads[target].replicas
				}
				rule.perNode = rule.maxSkew
				if eligible > 0 {
					rule.perNode = max(rule.maxSkew, (replicas+eligible-1)/eligible)
				}
				rule.domains = nil
			}
			state.workloads[owner].spread = append(state.workloads[owner].spread, rule)
		}
	}
	for key := range keys {
		state.keys = append(state.keys, key)
	}
	sort.Strings(state.keys)
	return state, groups
}

func (state *placement) count(key, value string, matched []int) int {
	domain, total := state.counts[key+"\x00"+value], 0
	for _, workload := range matched {
		total += domain[workload]
	}
	return total
}

// admits reports whether the workload's next pod may join the node given
// the pods already placed.
func (state *placement) admits(workload int, node NodeCapacity) bool {
	rules := &state.workloads[workload]
	for _, group := range [][]selection{rules.anti, rules.antiBy} {
		for _, rule := range group {
			if value, found := domainValue(node, rule.key); found && state.count(rule.key, value, rule.matched) > 0 {
				return false
			}
		}
	}
	for _, rule := range rules.affinity {
		value, found := domainValue(node, rule.key)
		if !found {
			return false
		}
		if state.count(rule.key, value, rule.matched) > 0 {
			continue
		}
		placed := 0
		for _, target := range rule.matched {
			placed += state.totals[target]
		}
		if !rule.self || placed > 0 {
			return false
		}
	}
	for _, rule := range rules.spread {
		value, found := domainValue(node, rule.key)
		if !found {
			return false
		}
		current := state.count(rule.key, value, rule.matched)
		if rule.perNode > 0 {
			if current+1 > rule.perNode {
				return false
			}
			continue
		}
		minimum := math.MaxInt
		for _, domain := range rule.domains {
			minimum = min(minimum, state.count(rule.key, domain, rule.matched))
		}
		if minimum == math.MaxInt {
			minimum = 0
		}
		if current+1-minimum > rule.maxSkew {
			return false
		}
	}
	return true
}

func (state *placement) record(workload int, node NodeCapacity) {
	state.totals[workload]++
	for _, key := range state.keys {
		if value, found := domainValue(node, key); found {
			domain := state.counts[key+"\x00"+value]
			if domain == nil {
				domain = map[int]int{}
				state.counts[key+"\x00"+value] = domain
			}
			domain[workload]++
		}
	}
}

// pack places pods with first-fit-decreasing onto nodes ordered largest
// first, after reserving DaemonSet pods on every node. Pods that a
// PodDisruptionBudget keeps in place are placed on their current nodes
// first. A pod only uses nodes whose taints it tolerates and whose labels
// satisfy its nodeSelector and required node affinity, and only where its
// required pod affinity, anti-affinity (in both directions), and
// DoNotSchedule topology spread allow. Pods whose affinity needs another
// workload's pods are placed last. It returns how many nodes receive at
// least one pod. Preferences are not modeled, so the result is an estimate,
// not a scheduler simulation.
func pack(nodes []NodeCapacity, daemon podDemand, pods []podDemand) packingResult {
	type bin struct {
		cpu, memory int64
		node        NodeCapacity
	}
	bins := make([]bin, 0, len(nodes))
	var maxCPU, maxMemory int64 = 1, 1
	for _, node := range nodes {
		if !node.Schedulable {
			continue
		}
		bins = append(bins, bin{node.CPUMilli - daemon.cpuMilli, node.MemoryBytes - daemon.memoryBytes, node})
		maxCPU, maxMemory = max(maxCPU, node.CPUMilli), max(maxMemory, node.MemoryBytes)
	}
	sort.SliceStable(bins, func(left, right int) bool {
		return float64(bins[left].cpu)/float64(maxCPU)+float64(bins[left].memory)/float64(maxMemory) >
			float64(bins[right].cpu)/float64(maxCPU)+float64(bins[right].memory)/float64(maxMemory)
	})
	state, groups := newPlacement(nodes, pods)
	order := make([]int, len(pods))
	for index := range order {
		order[index] = index
	}
	dominant := func(pod podDemand) float64 {
		return max(float64(pod.cpuMilli)/float64(maxCPU), float64(pod.memoryBytes)/float64(maxMemory))
	}
	sort.SliceStable(order, func(left, right int) bool {
		leftDependent, rightDependent := state.workloads[groups[order[left]]].dependent, state.workloads[groups[order[right]]].dependent
		if leftDependent != rightDependent {
			return rightDependent
		}
		return dominant(pods[order[left]]) > dominant(pods[order[right]])
	})
	result := packingResult{UnplacedWorkloads: []string{}, nodesByGroup: map[string]int{}}
	used := make([]bool, len(bins))
	place := func(index, target int) {
		bins[target].cpu -= pods[index].cpuMilli
		bins[target].memory -= pods[index].memoryBytes
		if !used[target] {
			used[target] = true
			result.NodesNeeded++
			result.nodesByGroup[nodeGroupKey(bins[target].node)]++
		}
		state.record(groups[index], bins[target].node)
	}
	byName := make(map[string]int, len(bins))
	for index := range bins {
		byName[bins[index].node.Name] = index
	}
	pinned := make([]bool, len(pods))
	for index, pod := range pods {
		result.CPURequested += pod.cpuMilli
		result.MemoryRequests += pod.memoryBytes
		// A pod on a cordoned or missing node is moved like any other.
		if target, found := byName[pod.pinnedNode]; found && pod.pinnedNode != "" {
			place(index, target)
			pinned[index] = true
			result.PinnedPods++
		}
	}
	unplaced := map[string]bool{}
	for _, index := range order {
		if pinned[index] {
			continue
		}
		pod := pods[index]
		placed, eligible, ruled := false, false, false
		for target := range bins {
			if !pod.constraints.allows(bins[target].node) {
				continue
			}
			eligible = true
			if bins[target].cpu < pod.cpuMilli || bins[target].memory < pod.memoryBytes {
				continue
			}
			if !state.admits(groups[index], bins[target].node) {
				ruled = true
				continue
			}
			place(index, target)
			placed = true
			break
		}
		if placed {
			continue
		}
		result.UnplacedPods++
		switch {
		case !eligible:
			result.NoEligibleNodePods++
		case ruled:
			result.PodRulePods++
		}
		if pod.workload != "" && !unplaced[pod.workload] && len(result.UnplacedWorkloads) < 20 {
			unplaced[pod.workload] = true
			result.UnplacedWorkloads = append(result.UnplacedWorkloads, pod.workload)
		}
	}
	return result
}
