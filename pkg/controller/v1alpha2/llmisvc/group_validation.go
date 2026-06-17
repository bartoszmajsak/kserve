/*
Copyright 2026 The KServe Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package llmisvc

import (
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	v1alpha2 "github.com/kserve/kserve/pkg/apis/serving/v1alpha2"
)

// groupModelMismatch describes a member whose model.name differs from the group.
type groupModelMismatch struct {
	member      types.NamespacedName
	memberModel string
}

// validateGroupModelName determines which members (if any) have a model.name that
// differs from the group. Returns the canonical model name and a list of
// mismatched members. An empty mismatch list means all members agree.
//
// A model name is canonical when it has a strict majority (more than half).
// When no strict majority exists (e.g., a 2-2 split), all members are
// considered mismatched since the group is ambiguous.
//
// During a rolling model update (e.g., migrating from llama-8b to llama-70b),
// the canonical model flips when the new model reaches majority. This is
// intentional - the lagging member becomes the outlier.
func validateGroupModelName(
	members []v1alpha2.LLMInferenceService,
) (canonicalModel string, mismatched []groupModelMismatch) {
	counts := make(map[string]int)
	for i := range members {
		name := ptr.Deref(members[i].Spec.Model.Name, members[i].Name)
		counts[name]++
	}

	// Strict majority: more than half. Using count*2 > total avoids
	// integer division truncation (e.g., a 2-2 split gives 2*2=4, 4>4
	// is false - correctly rejecting the tie).
	for name, count := range counts {
		if count*2 > len(members) {
			canonicalModel = name
			break
		}
	}

	if canonicalModel == "" {
		for i := range members {
			mismatched = append(mismatched, groupModelMismatch{
				member:      types.NamespacedName{Namespace: members[i].Namespace, Name: members[i].Name},
				memberModel: ptr.Deref(members[i].Spec.Model.Name, members[i].Name),
			})
		}
		return "", mismatched
	}

	for i := range members {
		m := ptr.Deref(members[i].Spec.Model.Name, members[i].Name)
		if m != canonicalModel {
			mismatched = append(mismatched, groupModelMismatch{
				member:      types.NamespacedName{Namespace: members[i].Namespace, Name: members[i].Name},
				memberModel: m,
			})
		}
	}

	return canonicalModel, mismatched
}

func isSelfMismatched(self *v1alpha2.LLMInferenceService, mismatched []groupModelMismatch) bool {
	for _, mm := range mismatched {
		if mm.member.Name == self.Name && mm.member.Namespace == self.Namespace {
			return true
		}
	}
	return false
}

func ambiguousModelMessage(members []v1alpha2.LLMInferenceService) string {
	counts := make(map[string]int)
	for i := range members {
		counts[ptr.Deref(members[i].Spec.Model.Name, members[i].Name)]++
	}
	parts := make([]string, 0, len(counts))
	for name, count := range counts {
		parts = append(parts, fmt.Sprintf("%q (%d members)", name, count))
	}
	slices.Sort(parts)
	return fmt.Sprintf("group has no model name majority: found %s; all members must serve the same model",
		strings.Join(parts, ", "))
}
