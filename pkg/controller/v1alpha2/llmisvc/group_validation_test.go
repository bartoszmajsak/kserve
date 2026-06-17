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
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	v1alpha2 "github.com/kserve/kserve/pkg/apis/serving/v1alpha2"
)

func TestValidateGroupModelName(t *testing.T) {
	tests := []struct {
		name              string
		members           []v1alpha2.LLMInferenceService
		wantCanonical     string
		wantMismatchNames []string
	}{
		{
			name: "matching model names",
			members: []v1alpha2.LLMInferenceService{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "v1", Namespace: "ns"},
					Spec:       v1alpha2.LLMInferenceServiceSpec{Model: v1alpha2.LLMModelSpec{Name: ptr.To("llama-70b")}},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "v2", Namespace: "ns"},
					Spec:       v1alpha2.LLMInferenceServiceSpec{Model: v1alpha2.LLMModelSpec{Name: ptr.To("llama-70b")}},
				},
			},
			wantCanonical:     "llama-70b",
			wantMismatchNames: nil,
		},
		{
			name: "one mismatched member - outlier identified by strict majority",
			members: []v1alpha2.LLMInferenceService{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "v1", Namespace: "ns"},
					Spec:       v1alpha2.LLMInferenceServiceSpec{Model: v1alpha2.LLMModelSpec{Name: ptr.To("llama-70b")}},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "v2", Namespace: "ns"},
					Spec:       v1alpha2.LLMInferenceServiceSpec{Model: v1alpha2.LLMModelSpec{Name: ptr.To("llama-70b")}},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "v3-bad", Namespace: "ns"},
					Spec:       v1alpha2.LLMInferenceServiceSpec{Model: v1alpha2.LLMModelSpec{Name: ptr.To("llama-8b")}},
				},
			},
			wantCanonical:     "llama-70b",
			wantMismatchNames: []string{"v3-bad"},
		},
		{
			name: "single member - no mismatch possible",
			members: []v1alpha2.LLMInferenceService{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "v1", Namespace: "ns"},
					Spec:       v1alpha2.LLMInferenceServiceSpec{Model: v1alpha2.LLMModelSpec{Name: ptr.To("llama-70b")}},
				},
			},
			wantCanonical:     "llama-70b",
			wantMismatchNames: nil,
		},
		{
			name: "two members with different names - no strict majority, all mismatched",
			members: []v1alpha2.LLMInferenceService{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "v1", Namespace: "ns"},
					Spec:       v1alpha2.LLMInferenceServiceSpec{Model: v1alpha2.LLMModelSpec{Name: ptr.To("model-b")}},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "v2", Namespace: "ns"},
					Spec:       v1alpha2.LLMInferenceServiceSpec{Model: v1alpha2.LLMModelSpec{Name: ptr.To("model-a")}},
				},
			},
			wantCanonical:     "",
			wantMismatchNames: []string{"v1", "v2"},
		},
		{
			name: "2-2 split - no strict majority, all mismatched",
			members: []v1alpha2.LLMInferenceService{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "v1", Namespace: "ns"},
					Spec:       v1alpha2.LLMInferenceServiceSpec{Model: v1alpha2.LLMModelSpec{Name: ptr.To("llama-70b")}},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "v2", Namespace: "ns"},
					Spec:       v1alpha2.LLMInferenceServiceSpec{Model: v1alpha2.LLMModelSpec{Name: ptr.To("llama-70b")}},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "v3", Namespace: "ns"},
					Spec:       v1alpha2.LLMInferenceServiceSpec{Model: v1alpha2.LLMModelSpec{Name: ptr.To("llama-8b")}},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "v4", Namespace: "ns"},
					Spec:       v1alpha2.LLMInferenceServiceSpec{Model: v1alpha2.LLMModelSpec{Name: ptr.To("llama-8b")}},
				},
			},
			wantCanonical:     "",
			wantMismatchNames: []string{"v1", "v2", "v3", "v4"},
		},
		{
			name: "3-way split - no strict majority, all mismatched",
			members: []v1alpha2.LLMInferenceService{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "v1", Namespace: "ns"},
					Spec:       v1alpha2.LLMInferenceServiceSpec{Model: v1alpha2.LLMModelSpec{Name: ptr.To("llama-70b")}},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "v2", Namespace: "ns"},
					Spec:       v1alpha2.LLMInferenceServiceSpec{Model: v1alpha2.LLMModelSpec{Name: ptr.To("llama-8b")}},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "v3", Namespace: "ns"},
					Spec:       v1alpha2.LLMInferenceServiceSpec{Model: v1alpha2.LLMModelSpec{Name: ptr.To("mistral-7b")}},
				},
			},
			wantCanonical:     "",
			wantMismatchNames: []string{"v1", "v2", "v3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			canonical, mismatched := validateGroupModelName(tt.members)
			assert.Equal(t, tt.wantCanonical, canonical)
			var names []string
			for _, mm := range mismatched {
				names = append(names, mm.member.Name)
			}
			assert.Equal(t, tt.wantMismatchNames, names)
		})
	}
}

func TestIsSelfMismatched(t *testing.T) {
	mismatched := []groupModelMismatch{
		{member: types.NamespacedName{Namespace: "ns", Name: "v3-bad"}, memberModel: "wrong-model"},
	}
	self := &v1alpha2.LLMInferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "v1", Namespace: "ns"},
	}
	assert.False(t, isSelfMismatched(self, mismatched))

	selfBad := &v1alpha2.LLMInferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "v3-bad", Namespace: "ns"},
	}
	assert.True(t, isSelfMismatched(selfBad, mismatched))
	assert.False(t, isSelfMismatched(self, nil))
}

