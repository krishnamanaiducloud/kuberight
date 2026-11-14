package httpapi

// Scheduling constraints that decide which nodes a pod may use. Node-impact
// estimates honor taints and tolerations, nodeSelector, and required node
// affinity. Preferred affinity, pod affinity and anti-affinity, topology
// spread, and PodDisruptionBudgets are not modeled.

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

// SchedulingConstraints are a pod template's node placement rules.
// RequiredTerms are ORed; the requirements inside one term are ANDed.
type SchedulingConstraints struct {
	NodeSelector  map[string]string   `json:"nodeSelector,omitempty"`
	Tolerations   []Toleration        `json:"tolerations,omitempty"`
	RequiredTerms [][]NodeRequirement `json:"requiredTerms,omitempty"`
}

// Empty reports whether the pod can use any schedulable node.
func (constraints SchedulingConstraints) Empty() bool {
	return len(constraints.NodeSelector) == 0 && len(constraints.Tolerations) == 0 && len(constraints.RequiredTerms) == 0
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

// allows reports whether a pod with these constraints may run on the node.
func (constraints SchedulingConstraints) allows(node NodeCapacity) bool {
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
