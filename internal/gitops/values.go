package gitops

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// valuesKey is a dotted path of plain Helm values keys, such as
// "resources" or "api.resources".
var valuesKey = regexp.MustCompile(`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$`)

// ValidateValuesKey rejects keys UpdateValues cannot address.
func ValidateValuesKey(key string) error {
	if len(key) > 256 || !valuesKey.MatchString(key) {
		return fmt.Errorf("values key %q must be dot-separated letters, digits, '-' and '_'", key)
	}
	return nil
}

// UpdateValues sets the requests and limits at a dotted key of a Helm values
// file, such as "resources" or "api.resources". The mapping that holds the
// key must exist. A missing, null, or empty ("{}") resources value becomes a
// block; an existing block is edited in place. Only the affected values
// change, so comments and formatting elsewhere stay as they were.
func UpdateValues(content []byte, key string, desired Resources) ([]byte, error) {
	if err := ValidateValuesKey(key); err != nil {
		return nil, err
	}
	var document yaml.Node
	if err := yaml.NewDecoder(bytes.NewReader(content)).Decode(&document); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse values: %w", err)
	}
	if len(document.Content) == 0 {
		return nil, ErrTargetNotFound
	}
	segments := strings.Split(key, ".")
	parent := document.Content[0]
	for _, segment := range segments[:len(segments)-1] {
		if _, parent = mappingValue(parent, segment); parent == nil {
			return nil, ErrTargetNotFound
		}
	}
	if parent.Kind != yaml.MappingNode || parent.Style&yaml.FlowStyle != 0 || len(parent.Content) == 0 {
		return nil, ErrUnsupportedLayout
	}
	lines := strings.SplitAfter(string(content), "\n")
	name := segments[len(segments)-1]
	keyNode, resources := mappingValue(parent, name)
	values := desiredValues(desired)
	switch {
	case keyNode == nil:
		// Add the key after the last entry of its mapping.
		indent := indentOf(lines, parent.Content[0].Line)
		text := indent + name + ":\n" + resourceBlock(indent+"  ", values)
		return apply(lines, []edit{{line: lastLine(parent) + 1, column: 1, text: text}}), nil
	case resources.Kind == yaml.MappingNode && len(resources.Content) == 0,
		resources.Kind == yaml.ScalarNode && resources.Tag == "!!null":
		// "resources: {}", "resources: null", "resources: ~", or a bare
		// "resources:" becomes a block under the key.
		raw := lines[keyNode.Line-1]
		body := strings.TrimRight(raw, "\r\n")
		colon := strings.Index(body[min(len(body), keyNode.Column-1):], ":")
		if resources.Line != keyNode.Line || colon < 0 {
			return nil, ErrUnsupportedLayout
		}
		ending := raw[len(body):]
		if ending == "" {
			ending = "\n"
		}
		comment := ""
		if index := strings.Index(body, " #"); index >= 0 {
			for index > 0 && body[index-1] == ' ' {
				index--
			}
			comment = body[index:]
		}
		indent := indentOf(lines, keyNode.Line)
		lines[keyNode.Line-1] = body[:keyNode.Column-1+colon+1] + comment + ending
		return apply(lines, []edit{{line: keyNode.Line + 1, column: 1, text: resourceBlock(indent+"  ", values)}}), nil
	case resources.Kind != yaml.MappingNode:
		return nil, ErrUnsupportedLayout
	}
	// An existing block mapping: edit it like a container's resources.
	holder := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{{Kind: yaml.ScalarNode, Value: "resources", Line: keyNode.Line, Column: keyNode.Column}, resources}}
	edits, err := resourceEdits(lines, holder, desired)
	if err != nil {
		return nil, err
	}
	if len(edits) == 0 {
		return nil, ErrNoChange
	}
	return apply(lines, edits), nil
}
