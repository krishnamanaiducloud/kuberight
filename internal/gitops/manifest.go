// Package gitops proposes approved recommendations as pull requests that
// edit the workload's manifest in Git, so GitOps controllers apply them.
package gitops

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Target identifies the container whose resources a manifest edit sets.
type Target struct {
	Namespace, Kind, Workload, Container string
}

// Resources are the values to write. A zero limit leaves the manifest's
// limit as it is; RemoveCPULimit deletes it.
type Resources struct {
	CPURequestMilli, CPULimitMilli       int64
	MemoryRequestBytes, MemoryLimitBytes int64
	RemoveCPULimit                       bool
}

var (
	// ErrTargetNotFound means the file does not define the workload's container.
	ErrTargetNotFound = errors.New("the manifest does not define the workload's container")
	// ErrNoChange means the manifest already has the recommended values.
	ErrNoChange = errors.New("the manifest already has the recommended resources")
	// ErrUnsupportedLayout means the YAML cannot be edited without rewriting it.
	ErrUnsupportedLayout = errors.New("the manifest's resources use a layout KubeRight cannot edit in place")
)

// FormatCPU renders millicores as a Kubernetes quantity.
func FormatCPU(milli int64) string {
	if milli%1000 == 0 {
		return strconv.FormatInt(milli/1000, 10)
	}
	return strconv.FormatInt(milli, 10) + "m"
}

// FormatMemory renders bytes as the largest exact binary-suffix quantity.
func FormatMemory(bytes int64) string {
	for _, unit := range []struct {
		suffix string
		size   int64
	}{{"Gi", 1 << 30}, {"Mi", 1 << 20}, {"Ki", 1 << 10}} {
		if bytes >= unit.size && bytes%unit.size == 0 {
			return strconv.FormatInt(bytes/unit.size, 10) + unit.suffix
		}
	}
	return strconv.FormatInt(bytes, 10)
}

// edit replaces or inserts text in the original file, so comments,
// ordering, quoting, and every other document stay exactly as they were.
type edit struct {
	line, column int // 1-based start of the replaced text
	length       int // bytes to replace; 0 inserts
	text         string
	deleteLine   bool
	order        int // generation order, kept for edits at the same position
}

// UpdateManifest sets the container's requests and limits in a multi-
// document YAML file. Only the affected values change.
func UpdateManifest(content []byte, target Target, desired Resources) ([]byte, error) {
	lines := strings.SplitAfter(string(content), "\n")
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	for {
		var document yaml.Node
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			return nil, ErrTargetNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("parse manifest: %w", err)
		}
		if len(document.Content) == 0 {
			continue
		}
		root := document.Content[0]
		container := findContainer(root, target)
		if container == nil {
			continue
		}
		edits, err := resourceEdits(lines, container, desired)
		if err != nil {
			return nil, err
		}
		if len(edits) == 0 {
			return nil, ErrNoChange
		}
		return apply(lines, edits), nil
	}
}

func mappingValue(node *yaml.Node, key string) (*yaml.Node, *yaml.Node) {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil, nil
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			return node.Content[index], node.Content[index+1]
		}
	}
	return nil, nil
}

func path(node *yaml.Node, keys ...string) *yaml.Node {
	for _, key := range keys {
		_, node = mappingValue(node, key)
	}
	return node
}

func findContainer(root *yaml.Node, target Target) *yaml.Node {
	if kind := path(root, "kind"); kind == nil || kind.Value != target.Kind {
		return nil
	}
	if name := path(root, "metadata", "name"); name == nil || name.Value != target.Workload {
		return nil
	}
	if namespace := path(root, "metadata", "namespace"); namespace != nil && namespace.Value != "" && namespace.Value != target.Namespace {
		return nil
	}
	podSpec := path(root, "spec", "template", "spec")
	if target.Kind == "CronJob" {
		podSpec = path(root, "spec", "jobTemplate", "spec", "template", "spec")
	}
	for _, list := range []string{"containers", "initContainers"} {
		containers := path(podSpec, list)
		if containers == nil || containers.Kind != yaml.SequenceNode {
			continue
		}
		for _, container := range containers.Content {
			if name := path(container, "name"); name != nil && name.Value == target.Container {
				return container
			}
		}
	}
	return nil
}

// lastLine is the last source line a node's content occupies.
func lastLine(node *yaml.Node) int {
	last := node.Line
	for _, child := range node.Content {
		last = max(last, lastLine(child))
	}
	return last
}

func indentOf(lines []string, line int) string {
	text := lines[line-1]
	return text[:len(text)-len(strings.TrimLeft(text, " "))]
}

// scalarLength is the length of a scalar's source text at its position.
func scalarLength(lines []string, node *yaml.Node) (int, bool) {
	text := strings.TrimRight(lines[node.Line-1], "\r\n")
	if node.Column-1 >= len(text) {
		return 0, false
	}
	rest := text[node.Column-1:]
	switch node.Style {
	case yaml.DoubleQuotedStyle, yaml.SingleQuotedStyle:
		quote := rest[0]
		for index := 1; index < len(rest); index++ {
			if rest[index] == '\\' && quote == '"' {
				index++
				continue
			}
			if rest[index] == quote {
				return index + 1, true
			}
		}
		return 0, false
	case 0, yaml.TaggedStyle:
		end := len(rest)
		if comment := strings.Index(rest, " #"); comment >= 0 {
			end = comment
		}
		if comma := strings.IndexAny(rest[:end], ",}]"); comma >= 0 && node.Style == 0 && strings.ContainsAny(text[:node.Column-1], "{[") {
			end = comma
		}
		return len(strings.TrimRight(rest[:end], " ")), true
	}
	return 0, false
}

func quoted(node *yaml.Node, value string) string {
	switch node.Style {
	case yaml.DoubleQuotedStyle:
		return `"` + value + `"`
	case yaml.SingleQuotedStyle:
		return "'" + value + "'"
	}
	return value
}

// value is one resource quantity to write.
type value struct{ section, key, text string }

func desiredValues(desired Resources) []value {
	values := []value{{"requests", "cpu", FormatCPU(desired.CPURequestMilli)}, {"requests", "memory", FormatMemory(desired.MemoryRequestBytes)}}
	if desired.CPULimitMilli > 0 {
		values = append(values, value{"limits", "cpu", FormatCPU(desired.CPULimitMilli)})
	}
	if desired.MemoryLimitBytes > 0 {
		values = append(values, value{"limits", "memory", FormatMemory(desired.MemoryLimitBytes)})
	}
	return values
}

// resourceBlock renders a complete resources mapping body, each line
// starting with indent.
func resourceBlock(indent string, values []value) string {
	block := ""
	for _, section := range []string{"requests", "limits"} {
		entries := ""
		for _, item := range values {
			if item.section == section {
				entries += indent + "  " + item.key + ": " + item.text + "\n"
			}
		}
		if entries != "" {
			block += indent + section + ":\n" + entries
		}
	}
	return block
}

func resourceEdits(lines []string, container *yaml.Node, desired Resources) ([]edit, error) {
	values := desiredValues(desired)
	edits := []edit{}
	resourcesKey, resources := mappingValue(container, "resources")
	if resources != nil && resources.Kind != yaml.MappingNode {
		return nil, ErrUnsupportedLayout
	}
	if resources != nil && resources.Style&yaml.FlowStyle != 0 {
		return nil, ErrUnsupportedLayout
	}
	if resources == nil {
		// Insert a complete resources block after the container's last line.
		nameKey, _ := mappingValue(container, "name")
		if nameKey == nil || container.Style&yaml.FlowStyle != 0 {
			return nil, ErrUnsupportedLayout
		}
		indent := strings.Repeat(" ", nameKey.Column-1)
		block := indent + "resources:\n" + resourceBlock(indent+"  ", values)
		return []edit{{line: lastLine(container) + 1, column: 1, text: block}}, nil
	}
	for _, section := range []string{"requests", "limits"} {
		sectionKey, mapping := mappingValue(resources, section)
		pending := []value{}
		for _, item := range values {
			if item.section != section {
				continue
			}
			key, node := mappingValue(mapping, item.key)
			if node == nil {
				pending = append(pending, item)
				continue
			}
			if node.Kind != yaml.ScalarNode {
				return nil, ErrUnsupportedLayout
			}
			if node.Value == item.text {
				continue
			}
			length, ok := scalarLength(lines, node)
			if !ok {
				return nil, ErrUnsupportedLayout
			}
			_ = key
			edits = append(edits, edit{line: node.Line, column: node.Column, length: length, text: quoted(node, item.text)})
		}
		if len(pending) == 0 {
			continue
		}
		if mapping != nil && (mapping.Kind != yaml.MappingNode || mapping.Style&yaml.FlowStyle != 0) {
			return nil, ErrUnsupportedLayout
		}
		var indent string
		var after int
		if mapping != nil && len(mapping.Content) > 0 {
			indent, after = indentOf(lines, mapping.Content[0].Line), lastLine(mapping)
		} else {
			if sectionKey != nil {
				return nil, ErrUnsupportedLayout // an empty "requests:" value
			}
			// The section is missing: add it under resources.
			childIndent := indentOf(lines, resourcesKey.Line) + "  "
			if len(resources.Content) > 0 {
				childIndent = indentOf(lines, resources.Content[0].Line)
			}
			text := childIndent + section + ":\n"
			for _, item := range pending {
				text += childIndent + "  " + item.key + ": " + item.text + "\n"
			}
			edits = append(edits, edit{line: lastLine(resources) + 1, column: 1, text: text})
			continue
		}
		text := ""
		for _, item := range pending {
			text += indent + item.key + ": " + item.text + "\n"
		}
		edits = append(edits, edit{line: after + 1, column: 1, text: text})
	}
	if desired.RemoveCPULimit {
		if key, node := mappingValue(path(resources, "limits"), "cpu"); node != nil {
			if lastLine(node) != key.Line {
				return nil, ErrUnsupportedLayout
			}
			edits = append(edits, edit{line: key.Line, deleteLine: true})
		}
	}
	return edits, nil
}

func apply(lines []string, edits []edit) []byte {
	for index := range edits {
		edits[index].order = index
	}
	sort.SliceStable(edits, func(left, right int) bool {
		if edits[left].line != edits[right].line {
			return edits[left].line > edits[right].line
		}
		if edits[left].column != edits[right].column {
			return edits[left].column > edits[right].column
		}
		// Insertions at one position are applied last-first so they end up in order.
		return edits[left].order > edits[right].order
	})
	for _, change := range edits {
		index := change.line - 1
		switch {
		case change.deleteLine:
			lines = append(lines[:index], lines[index+1:]...)
		case change.length == 0:
			if index > 0 && index <= len(lines) && !strings.HasSuffix(lines[index-1], "\n") {
				lines[index-1] += "\n"
			}
			if index > len(lines) {
				index = len(lines)
			}
			lines = append(lines[:index], append([]string{change.text}, lines[index:]...)...)
		default:
			text := lines[index]
			start := change.column - 1
			lines[index] = text[:start] + change.text + text[start+change.length:]
		}
	}
	return []byte(strings.Join(lines, ""))
}
