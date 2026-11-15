package httpapi

// Scheduling constraints that decide which nodes a pod may use. Node-impact
// estimates honor taints and tolerations, nodeSelector, required node
// affinity, required pod affinity and anti-affinity, DoNotSchedule topology
// spread, and PodDisruptionBudgets that allow no voluntary evictions.
// Preferences (preferred affinity and ScheduleAnyway spread) are not modeled.

// NodeTaint is a node taint that can repel pods.
type NodeTaint struct {
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Effect string `json:"effect"`
}

// Toleration mirrors a pod toleration.
type Toleration struct {
	Key      string `json:"key,omitempty"`
	Operator string `json:"operator,omitempty"`
	Value    string `json:"value,omitempty"`
	Effect   string `json:"effect,omitempty"`
}

// NodeRequirement is one required node-affinity match expression.
type NodeRequirement struct {
	Key      string   `json:"key"`
	Operator string   `json:"operator"`
	Values   []string `json:"values,omitempty"`
}

// LabelSelector mirrors a Kubernetes label selector. Its expressions use the
// In, NotIn, Exists, and DoesNotExist operators.
type LabelSelector struct {
	MatchLabels      map[string]string `json:"matchLabels,omitempty"`
	MatchExpressions []NodeRequirement `json:"matchExpressions,omitempty"`
}

// PodAffinityTerm is one required pod affinity or anti-affinity term. A nil
// Selector matches no pods. Without Namespaces or AllNamespaces the term
// selects pods in the pod's own namespace.
type PodAffinityTerm struct {
	Selector      *LabelSelector `json:"selector,omitempty"`
	Namespaces    []string       `json:"namespaces,omitempty"`
	AllNamespaces bool           `json:"allNamespaces,omitempty"`
	TopologyKey   string         `json:"topologyKey"`
}

// TopologySpread is a topology spread constraint with whenUnsatisfiable
// DoNotSchedule. It counts pods in the pod's own namespace.
type TopologySpread struct {
	MaxSkew     int            `json:"maxSkew"`
	TopologyKey string         `json:"topologyKey"`
	Selector    *LabelSelector `json:"selector,omitempty"`
}

// SchedulingConstraints are a pod template's placement rules.
// RequiredTerms are ORed; the requirements inside one term are ANDed.
type SchedulingConstraints struct {
	NodeSelector    map[string]string   `json:"nodeSelector,omitempty"`
	Tolerations     []Toleration        `json:"tolerations,omitempty"`
	RequiredTerms   [][]NodeRequirement `json:"requiredTerms,omitempty"`
	PodAffinity     []PodAffinityTerm   `json:"podAffinity,omitempty"`
	PodAntiAffinity []PodAffinityTerm   `json:"podAntiAffinity,omitempty"`
	TopologySpread  []TopologySpread    `json:"topologySpread,omitempty"`
	// Labels are the pod template labels that other pods' rules select.
	Labels map[string]string `json:"labels,omitempty"`
}

// Empty reports whether the pod can use any schedulable node and has no
// labels that another pod's rules could select.
func (constraints SchedulingConstraints) Empty() bool {
	return len(constraints.NodeSelector) == 0 && len(constraints.Tolerations) == 0 && len(constraints.RequiredTerms) == 0 &&
		len(constraints.PodAffinity) == 0 && len(constraints.PodAntiAffinity) == 0 && len(constraints.TopologySpread) == 0 && len(constraints.Labels) == 0
}

// matches reports whether the selector selects a pod with these labels.
func (selector *LabelSelector) matches(labels map[string]string) bool {
	if selector == nil {
		return false
	}
	for key, value := range selector.MatchLabels {
		if current, present := labels[key]; !present || current != value {
			return false
		}
	}
	for _, requirement := range selector.MatchExpressions {
		if !requirement.matches(labels) {
			return false
		}
	}
	return true
}

// selects reports whether a term owned by a pod in ownerNamespace selects a
// pod in namespace with these labels.
func (term PodAffinityTerm) selects(ownerNamespace, namespace string, labels map[string]string) bool {
	switch {
	case term.AllNamespaces:
	case len(term.Namespaces) == 0:
		if namespace != ownerNamespace {
			return false
		}
	default:
		found := false
		for _, candidate := range term.Namespaces {
			found = found || candidate == namespace
		}
		if !found {
			return false
		}
	}
	return term.Selector.matches(labels)
}

func (toleration Toleration) tolerates(taint NodeTaint) bool {
	if toleration.Effect != "" && toleration.Effect != taint.Effect {
		return false
	}
	if toleration.Operator == "Exists" {
		return toleration.Key == "" || toleration.Key == taint.Key
	}
	return toleration.Key == taint.Key && toleration.Value == taint.Value
}

func (requirement NodeRequirement) matches(labels map[string]string) bool {
	value, present := labels[requirement.Key]
	in := func() bool {
		for _, candidate := range requirement.Values {
			if candidate == value {
				return true
			}
		}
		return false
	}
	switch requirement.Operator {
	case "In":
		return present && in()
	case "NotIn":
		return !present || !in()
	case "Exists":
		return present
	case "DoesNotExist":
		return !present
	default:
		// Gt and Lt are rare; treat them as unknown and therefore unsatisfied
		// so the estimate never places a pod where the scheduler would not.
		return false
	}
}

// allows reports whether a pod with these constraints may run on the node,
// considering node rules only; pod rules depend on the other pods placed.
func (constraints SchedulingConstraints) allows(node NodeCapacity) bool {
	return constraints.tolerates(node) && constraints.selectsNode(node)
}

func (constraints SchedulingConstraints) tolerates(node NodeCapacity) bool {
	for _, taint := range node.Taints {
		if taint.Effect != "NoSchedule" && taint.Effect != "NoExecute" {
			continue
		}
		tolerated := false
		for _, toleration := range constraints.Tolerations {
			if toleration.tolerates(taint) {
				tolerated = true
				break
			}
		}
		if !tolerated {
			return false
		}
	}
	return true
}

// selectsNode applies nodeSelector and required node affinity.
func (constraints SchedulingConstraints) selectsNode(node NodeCapacity) bool {
	for key, value := range constraints.NodeSelector {
		if node.Labels[key] != value {
			return false
		}
	}
	if len(constraints.RequiredTerms) == 0 {
		return true
	}
	for _, term := range constraints.RequiredTerms {
		matched := true
		for _, requirement := range term {
			if !requirement.matches(node.Labels) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}
