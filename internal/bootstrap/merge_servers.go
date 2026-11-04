package bootstrap

import (
	"fmt"
	"sort"

	"agentcfg/internal/syncer"
)

func mergeServerCandidates(plan *Plan) {
	servers := []ServerCandidate{}
	groups := map[string]int{}
	names := map[string][]int{}
	for _, item := range plan.Servers {
		if item.NativeName == "" {
			item.NativeName = item.Name
		}
		item.CanonicalSHA256 = syncer.ServerSHA256(item.Server)
		item.ID = "server:" + item.Name + ":" + item.CanonicalSHA256
		if index, ok := groups[item.ID]; ok {
			old := &servers[index]
			old.Harnesses = union(old.Harnesses, item.Harnesses)
			old.Sources = union(old.Sources, item.Sources)
			old.Origins = unionOrigins(old.Origins, item.Origins)
			old.Warnings = union(old.Warnings, item.Warnings)
			old.Selected = old.Selected && item.Selected
			old.Server.Harnesses = append([]string(nil), old.Harnesses...)
			old.Source, old.Server.Source = old.Sources[0], old.Sources[0]
		} else {
			groups[item.ID] = len(servers)
			names[item.Name] = append(names[item.Name], len(servers))
			servers = append(servers, item)
		}
	}
	for name, origins := range plan.inactiveOrigins {
		indices := names[name]
		if len(indices) == 0 {
			continue
		}
		// Bind one retirement proof to the placeholder, even if the full name
		// has several upstream variants. All variants retain its disable policy.
		index := indices[0]
		for _, other := range indices {
			if servers[other].CanonicalSHA256 < servers[index].CanonicalSHA256 {
				index = other
			}
		}
		candidate := &servers[index]
		candidate.Origins = unionOrigins(candidate.Origins, origins)
		for _, origin := range origins {
			candidate.Harnesses = union(candidate.Harnesses, []string{origin.Harness})
			candidate.Sources = union(candidate.Sources, []string{origin.Source})
		}
	}
	sort.Slice(servers, func(i, j int) bool { return servers[i].ID < servers[j].ID })
	assignServerNames(plan, servers)

	plan.Servers = servers
}

// Assign names by definition, independent of native file enumeration order.
func assignServerNames(plan *Plan, servers []ServerCandidate) {
	names, bases, reserved := map[string]int{}, map[string]int{}, map[string]bool{}
	baseFor := func(candidate ServerCandidate) string {
		var active []string
		for _, origin := range candidate.Origins {
			if !origin.Inactive {
				active = union(active, []string{origin.Harness})
			}
		}
		if len(active) == 0 {
			active = candidate.Harnesses
		}
		harness := "native"
		if len(active) != 0 {
			harness = active[0]
		}
		return candidate.NativeName + "-" + harness
	}
	for _, candidate := range servers {
		names[candidate.NativeName]++
		bases[baseFor(candidate)]++
		reserved[candidate.NativeName] = true
	}
	for i := range servers {
		candidate := &servers[i]
		if names[candidate.NativeName] < 2 {
			continue
		}
		base := baseFor(*candidate)
		unique := base
		if bases[base] > 1 || reserved[unique] {
			for length := 8; length <= len(candidate.CanonicalSHA256); length += 8 {
				unique = base + "-" + candidate.CanonicalSHA256[:length]
				if !reserved[unique] {
					break
				}
			}
			for suffix := 1; reserved[unique]; suffix++ {
				unique = fmt.Sprintf("%s-%s-%d", base, candidate.CanonicalSHA256, suffix)
			}
		}
		reserved[unique] = true
		candidate.Name, candidate.Server.Name = unique, unique
		candidate.Warnings = union(candidate.Warnings, []string{"distinct native definitions kept under separate canonical names"})
		plan.Warnings = append(plan.Warnings, Warning{Source: candidate.Source, Server: unique, Kind: WarningAdvisory, Message: fmt.Sprintf("distinct definitions of native MCP %q are preserved under separate names", candidate.NativeName)})
	}
}

// Availability keeps normal imports available to every selected harness, while
// honoring native disable flags and reserved harness-owned runtimes. No source
// provenance is interpreted as an implicit restriction for ordinary servers.
func (p *Plan) Availability(candidate ServerCandidate) []string {
	active, inactive := map[string]bool{}, map[string]bool{}
	for _, origin := range candidate.Origins {
		if origin.Inactive {
			inactive[origin.Harness] = true
		} else {
			active[origin.Harness] = true
		}
	}
	for _, warning := range p.Warnings {
		if warning.Server == candidate.NativeName && (warning.Kind == WarningInactive || warning.Kind == WarningNativeRuntime) {
			inactive[warning.Harness] = true
		}
	}
	if len(active) == 0 && len(inactive) != 0 {
		return []string{"none"}
	}
	for harness := range active {
		delete(inactive, harness)
	}
	if len(inactive) == 0 {
		return nil
	}
	var allowed []string
	for _, target := range targets {
		if !inactive[target.name] {
			allowed = append(allowed, target.name)
		}
	}
	return allowed
}
