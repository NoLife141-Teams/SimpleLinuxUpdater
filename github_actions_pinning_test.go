package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
)

// Reuse the YAML parser already in go.mod to distinguish real uses keys from
// comments and shell script text, including quoted keys and flow mappings.
type actionPinningVisitor struct {
	lines  []string
	errors []string
}

var actionCommitPattern = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
var actionVersionCommentPattern = regexp.MustCompile(`#\s*v[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?(?:\s|$)`)

func (v *actionPinningVisitor) Visit(node ast.Node) ast.Visitor {
	entry, ok := node.(*ast.MappingValueNode)
	if !ok {
		return v
	}
	key, ok := entry.Key.(*ast.StringNode)
	if !ok || key.Value != "uses" {
		return v
	}
	line := entry.Key.GetToken().Position.Line
	value, ok := entry.Value.(*ast.StringNode)
	if !ok {
		v.errors = append(v.errors, fmt.Sprintf("line %d: uses must be a literal action reference", line))
		return v
	}
	if strings.HasPrefix(value.Value, "./") {
		return v
	}
	action, ref, found := strings.Cut(value.Value, "@")
	if !found || action == "" || !actionCommitPattern.MatchString(ref) {
		v.errors = append(v.errors, fmt.Sprintf("line %d: %q must use a full 40-character commit SHA", line, value.Value))
	} else if !actionVersionCommentPattern.MatchString(v.lines[line-1]) {
		v.errors = append(v.errors, fmt.Sprintf("line %d: %q needs a version comment (# vX.Y.Z)", line, value.Value))
	}
	return v
}

func workflowActionPinningErrors(source string) []string {
	file, err := parser.ParseBytes([]byte(source), 0)
	if err != nil {
		return []string{fmt.Sprintf("invalid workflow YAML: %v", err)}
	}
	visitor := &actionPinningVisitor{lines: strings.Split(source, "\n")}
	for _, doc := range file.Docs {
		ast.Walk(visitor, doc)
	}
	return visitor.errors
}

func TestGitHubWorkflowsPinExternalActions(t *testing.T) {
	entries, err := os.ReadDir(".github/workflows")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		ext := filepath.Ext(entry.Name())
		if entry.IsDir() || (ext != ".yml" && ext != ".yaml") {
			continue
		}
		count++
		t.Run(entry.Name(), func(t *testing.T) {
			source := readWorkflowForTest(t, filepath.Join(".github/workflows", entry.Name()))
			for _, problem := range workflowActionPinningErrors(source) {
				t.Error(problem)
			}
		})
	}
	if count == 0 {
		t.Fatal("no workflows found")
	}
}

func TestWorkflowActionPinningPolicy(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	tests := []struct {
		name, source string
		valid        bool
	}{
		{"major tag", "- uses: actions/checkout@v7", false},
		{"main", "uses: owner/action@main", false},
		{"master", "uses: owner/action@master", false},
		{"semver tag", "uses: owner/action@v1.2.3", false},
		{"missing ref", "uses: owner/action", false},
		{"short SHA", "uses: owner/action@0123456 # v1.2.3", false},
		{"long SHA", "uses: owner/action@" + sha + "0 # v1.2.3", false},
		{"non hex SHA", "uses: owner/action@z" + sha[1:] + " # v1.2.3", false},
		{"missing comment", "uses: owner/action@" + sha, false},
		{"major-only comment", "uses: owner/action@" + sha + " # v1", false},
		{"full SHA", "- uses: owner/action@" + sha + " # v1.2.3", true},
		{"quoted key and value", "'uses': 'owner/action@" + sha + "' # v1.2.3", true},
		{"flow mutable", "steps: [{uses: owner/action@main}]", false},
		{"reusable workflow mutable", "jobs:\n  shared:\n    uses: owner/repo/.github/workflows/test.yaml@main", false},
		{"reusable workflow pinned", "jobs:\n  shared:\n    uses: owner/repo/.github/workflows/test.yaml@" + sha + " # v1.2.3", true},
		{"local", "- uses: ./local-action", true},
		{"local workflow", "uses: ./.github/workflows/shared.yaml", true},
		{"script and comment", "# uses: owner/action@main\nrun: |\n  uses: owner/action@main", true},
		{"non literal", "uses: [owner/action@main]", false},
		{"invalid YAML", "uses: [", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			problems := workflowActionPinningErrors(tt.source)
			if (len(problems) == 0) != tt.valid {
				t.Fatalf("valid = %v, want %v; problems: %v", len(problems) == 0, tt.valid, problems)
			}
		})
	}
}
