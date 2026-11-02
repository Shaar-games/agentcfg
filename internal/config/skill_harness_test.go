package config

import (
	"reflect"
	"testing"
)

func TestSkillHarnessMetadata(t *testing.T) {
	for _, tc := range []struct {
		metadata string
		want     []string
		invalid  bool
	}{
		{"", nil, false},
		{"metadata:\n  harness: codex,claude-code,cursor\n", []string{"codex", "claude-code", "cursor"}, false},
		{"metadata:\n  harness: 'codex, codex, cursor'\n", []string{"codex", "cursor"}, false},
		{"metadata:\n  harness: [codex, cursor]\n", nil, true},
		{"metadata:\n  harness: unknown\n", nil, true},
		{"metadata:\n  harness: codex,\n", nil, true},
	} {
		data := []byte("---\nname: review\ndescription: Review code.\n" + tc.metadata + "---\n")
		_, validationErr := validateSkill(data, "review")
		got, err := skillHarnesses(data)
		if tc.invalid {
			if validationErr == nil && err == nil {
				t.Errorf("expected rejection: %q", tc.metadata)
			}
		} else if validationErr != nil || err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("metadata %q: got %v, errors %v/%v", tc.metadata, got, validationErr, err)
		}
	}
}
