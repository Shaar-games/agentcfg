package bootstrap

import (
	"sort"
)

func mergeCandidates(plan *Plan) {
	mergeServerCandidates(plan)

	skills := []SkillCandidate{}
	groups, names := map[string]int{}, map[string][]int{}
	for _, item := range plan.Skills {
		item.ID = "skill:" + item.Name + ":" + item.Fingerprint
		if index, ok := groups[item.ID]; ok {
			old := &skills[index]
			old.Harnesses = union(old.Harnesses, item.Harnesses)
			old.Sources = union(old.Sources, item.Sources)
			old.Origins = unionOrigins(old.Origins, item.Origins)
			old.Warnings = union(old.Warnings, item.Warnings)
			old.Selected = old.Selected && item.Selected
			old.Source = old.Sources[0]
		} else {
			groups[item.ID] = len(skills)
			names[item.Name] = append(names[item.Name], len(skills))
			skills = append(skills, item)
		}
	}
	for _, name := range keys(names) {
		if len(names[name]) < 2 {
			continue
		}
		for _, index := range names[name] {
			skills[index].Selected = false
			skills[index].Warnings = union(skills[index].Warnings, []string{"different skill trees share this name; select at most one"})
		}
		plan.warnSkill("", skills[names[name][0]].Source, "conflicting skill trees were left unselected")
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].ID < skills[j].ID })
	plan.Skills = skills
}

func union(a, b []string) []string {
	result := append(append([]string(nil), a...), b...)
	sort.Strings(result)
	out := result[:0]
	for _, value := range result {
		if len(out) == 0 || out[len(out)-1] != value {
			out = append(out, value)
		}
	}
	return out
}

func unionOrigins(a, b []Origin) []Origin {
	result := append(append([]Origin(nil), a...), b...)
	sort.Slice(result, func(i, j int) bool {
		if result[i].Harness != result[j].Harness {
			return result[i].Harness < result[j].Harness
		}
		return result[i].Source < result[j].Source
	})
	out := result[:0]
	for _, origin := range result {
		if len(out) == 0 || out[len(out)-1] != origin {
			out = append(out, origin)
		}
	}
	return out
}
