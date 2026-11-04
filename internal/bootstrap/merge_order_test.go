package bootstrap

import (
	"reflect"
	"testing"

	"agentcfg/internal/config"
)

func TestCanonicalMCPNamesDoNotDependOnFileEnumeration(t *testing.T) {
	var first map[string]string
	for _, order := range [][]string{{"one", "two", "three"}, {"three", "one", "two"}, {"two", "three", "one"}} {
		plan := &Plan{}
		for _, command := range order {
			plan.Servers = append(plan.Servers, ServerCandidate{Name: "same", NativeName: "same", Server: config.Server{Command: command}, Harnesses: []string{"opencode"}, Origins: []Origin{{Harness: "opencode", Source: command}}, Selected: true})
		}
		mergeServerCandidates(plan)
		actual, seen := map[string]string{}, map[string]bool{}
		for _, candidate := range plan.Servers {
			if candidate.Name == "same" || seen[candidate.Name] {
				t.Fatal("canonical name collision")
			}
			seen[candidate.Name] = true
			actual[candidate.Server.Command] = candidate.Name
		}
		if first == nil {
			first = actual
		} else if !reflect.DeepEqual(first, actual) {
			t.Fatalf("discovery order changed canonical names: %v vs %v", first, actual)
		}
	}
}
