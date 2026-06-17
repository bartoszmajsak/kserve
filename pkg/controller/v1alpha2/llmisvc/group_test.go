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
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	v1alpha2 "github.com/kserve/kserve/pkg/apis/serving/v1alpha2"
	"github.com/kserve/kserve/pkg/constants"
)

func TestRouterSpecHasGroup(t *testing.T) {
	tests := []struct {
		name   string
		llmSvc *v1alpha2.LLMInferenceService
		want   bool
	}{
		{
			name:   "nil router",
			llmSvc: &v1alpha2.LLMInferenceService{},
			want:   false,
		},
		{
			name: "nil route",
			llmSvc: &v1alpha2.LLMInferenceService{
				Spec: v1alpha2.LLMInferenceServiceSpec{
					Router: &v1alpha2.RouterSpec{},
				},
			},
			want: false,
		},
		{
			name: "no group",
			llmSvc: &v1alpha2.LLMInferenceService{
				Spec: v1alpha2.LLMInferenceServiceSpec{
					Router: &v1alpha2.RouterSpec{
						Route: &v1alpha2.GatewayRoutesSpec{
							Weight: ptr.To(int32(1)),
						},
					},
				},
			},
			want: false,
		},
		{
			name: "group set",
			llmSvc: &v1alpha2.LLMInferenceService{
				Spec: v1alpha2.LLMInferenceServiceSpec{
					Router: &v1alpha2.RouterSpec{
						Route: &v1alpha2.GatewayRoutesSpec{
							Group:  ptr.To("llama-70b"),
							Weight: ptr.To(int32(9)),
						},
					},
				},
			},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.llmSvc.Spec.Router.HasGroup())
		})
	}
}

func TestFilterActiveMembers(t *testing.T) {
	now := metav1.Now()

	t.Run("excludes terminating members", func(t *testing.T) {
		m := memberSvc("v1", "llama-70b", 9, false, now)
		m.DeletionTimestamp = &now
		members := []v1alpha2.LLMInferenceService{
			m,
			memberSvc("v2", "llama-70b", 1, false, now),
		}
		active := filterActiveMembers(members)
		require.Len(t, active, 1)
		assert.Equal(t, "v2", active[0].Name)
	})

	t.Run("keeps all active members", func(t *testing.T) {
		members := []v1alpha2.LLMInferenceService{
			memberSvc("v1", "llama-70b", 9, false, now),
			memberSvc("v2", "llama-70b", 1, false, now),
		}
		active := filterActiveMembers(members)
		assert.Len(t, active, 2)
	})
}

// TestResolveGroupMembers and TestResolveBackendRef were removed - these
// functions now use combineBaseRefsConfig (requiring a reconciler + cache).
// Behavior is covered by envtest integration tests in controller_int_group_test.go.

func TestIsGroupRoute(t *testing.T) {
	tests := []struct {
		name  string
		route *gwapiv1.HTTPRoute
		want  bool
	}{
		{
			name:  "nil route",
			route: nil,
			want:  false,
		},
		{
			name:  "no labels",
			route: &gwapiv1.HTTPRoute{},
			want:  false,
		},
		{
			name: "no group label",
			route: &gwapiv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"other": "label"},
				},
			},
			want: false,
		},
		{
			name: "has group label",
			route: &gwapiv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						constants.LLMRoutingGroupLabelKey: "llama-70b",
					},
				},
			},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isGroupRoute(tt.route))
		})
	}
}

func TestUpdateGroupStatus(t *testing.T) {
	t.Run("populates group status from resolved members", func(t *testing.T) {
		llmSvc := &v1alpha2.LLMInferenceService{
			Spec: v1alpha2.LLMInferenceServiceSpec{
				Router: &v1alpha2.RouterSpec{
					Route: &v1alpha2.GatewayRoutesSpec{
						Group: ptr.To("llama-70b"),
					},
				},
			},
		}

		resolved := []resolvedMember{
			{name: "v1", weight: 9, backendRef: gwapiv1.BackendObjectReference{Name: "v1-pool"}},
			{name: "v2", weight: 1, backendRef: gwapiv1.BackendObjectReference{Name: "v2-pool"}},
		}

		updateGroupStatus(llmSvc, resolved)

		require.NotNil(t, llmSvc.Status.Router)
		require.NotNil(t, llmSvc.Status.Router.Group)
		assert.Equal(t, "llama-70b", llmSvc.Status.Router.Group.Name)
		require.Len(t, llmSvc.Status.Router.Group.Members, 2)
		assert.Equal(t, "v1", llmSvc.Status.Router.Group.Members[0].Name)
		assert.Equal(t, int32(9), llmSvc.Status.Router.Group.Members[0].Weight)
		assert.Equal(t, "v2", llmSvc.Status.Router.Group.Members[1].Name)
		assert.Equal(t, int32(1), llmSvc.Status.Router.Group.Members[1].Weight)
	})

	t.Run("creates RouterStatus if nil", func(t *testing.T) {
		llmSvc := &v1alpha2.LLMInferenceService{
			Spec: v1alpha2.LLMInferenceServiceSpec{
				Router: &v1alpha2.RouterSpec{
					Route: &v1alpha2.GatewayRoutesSpec{
						Group: ptr.To("g"),
					},
				},
			},
		}

		updateGroupStatus(llmSvc, []resolvedMember{{name: "v1", weight: 1}})

		require.NotNil(t, llmSvc.Status.Router)
		assert.Equal(t, "g", llmSvc.Status.Router.Group.Name)
	})
}

func TestRewriteRulesForGroup(t *testing.T) {
	t.Run("replaces controller-managed backendRefs with group members", func(t *testing.T) {
		llmSvc := &v1alpha2.LLMInferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "my-svc"},
		}
		route := &gwapiv1.HTTPRoute{
			Spec: gwapiv1.HTTPRouteSpec{
				Rules: []gwapiv1.HTTPRouteRule{
					{
						BackendRefs: []gwapiv1.HTTPBackendRef{
							{BackendRef: gwapiv1.BackendRef{
								BackendObjectReference: gwapiv1.BackendObjectReference{
									Kind: ptr.To(gwapiv1.Kind("InferencePool")),
									Name: "my-svc-inference-pool",
								},
							}},
						},
					},
				},
			},
		}

		members := []resolvedMember{
			{name: "v1", weight: 9, backendRef: gwapiv1.BackendObjectReference{Name: "v1-pool"}},
			{name: "v2", weight: 1, backendRef: gwapiv1.BackendObjectReference{Name: "v2-pool"}},
		}

		rewriteRulesForGroup(route, llmSvc, members)

		require.Len(t, route.Spec.Rules[0].BackendRefs, 2)
		assert.Equal(t, gwapiv1.ObjectName("v1-pool"), route.Spec.Rules[0].BackendRefs[0].Name)
		assert.Equal(t, int32(9), *route.Spec.Rules[0].BackendRefs[0].Weight)
		assert.Equal(t, gwapiv1.ObjectName("v2-pool"), route.Spec.Rules[0].BackendRefs[1].Name)
		assert.Equal(t, int32(1), *route.Spec.Rules[0].BackendRefs[1].Weight)
	})

	t.Run("skips rules with only custom backendRefs", func(t *testing.T) {
		llmSvc := &v1alpha2.LLMInferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "my-svc"},
		}
		route := &gwapiv1.HTTPRoute{
			Spec: gwapiv1.HTTPRouteSpec{
				Rules: []gwapiv1.HTTPRouteRule{
					{BackendRefs: []gwapiv1.HTTPBackendRef{{BackendRef: gwapiv1.BackendRef{
						BackendObjectReference: gwapiv1.BackendObjectReference{
							Kind: ptr.To(gwapiv1.Kind("InferencePool")),
							Name: "my-svc-inference-pool",
						},
					}}}},
					{BackendRefs: []gwapiv1.HTTPBackendRef{{BackendRef: gwapiv1.BackendRef{
						BackendObjectReference: gwapiv1.BackendObjectReference{
							Kind: ptr.To(gwapiv1.Kind("Service")),
							Name: "user-custom-svc",
						},
					}}}},
				},
			},
		}

		members := []resolvedMember{
			{name: "v1", weight: 5, backendRef: gwapiv1.BackendObjectReference{Name: "v1-pool"}},
		}

		rewriteRulesForGroup(route, llmSvc, members)

		require.Len(t, route.Spec.Rules[0].BackendRefs, 1, "controller-managed rule should be rewritten")
		assert.Equal(t, gwapiv1.ObjectName("v1-pool"), route.Spec.Rules[0].BackendRefs[0].Name)

		require.Len(t, route.Spec.Rules[1].BackendRefs, 1, "custom rule should be untouched")
		assert.Equal(t, gwapiv1.ObjectName("user-custom-svc"), route.Spec.Rules[1].BackendRefs[0].Name)
	})
}

func TestIsExpectedBackendRef(t *testing.T) {
	svc := &v1alpha2.LLMInferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "my-svc"},
	}
	svcWithScheduler := &v1alpha2.LLMInferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "my-svc"},
		Spec: v1alpha2.LLMInferenceServiceSpec{
			Router: &v1alpha2.RouterSpec{
				Scheduler: &v1alpha2.SchedulerSpec{
					Pool: &v1alpha2.InferencePoolSpec{
						Ref: &corev1.LocalObjectReference{Name: "custom-pool"},
					},
				},
			},
		},
	}

	tests := []struct {
		name string
		svc  *v1alpha2.LLMInferenceService
		ref  gwapiv1.BackendRef
		want bool
	}{
		{
			name: "default InferencePool matches",
			svc:  svc,
			ref:  gwapiv1.BackendRef{BackendObjectReference: gwapiv1.BackendObjectReference{Kind: ptr.To(gwapiv1.Kind("InferencePool")), Name: "my-svc-inference-pool"}},
			want: true,
		},
		{
			name: "workload Service matches",
			svc:  svc,
			ref:  gwapiv1.BackendRef{BackendObjectReference: gwapiv1.BackendObjectReference{Kind: ptr.To(gwapiv1.Kind("Service")), Name: gwapiv1.ObjectName(workloadServiceName(svc))}},
			want: true,
		},
		{
			name: "custom pool ref matches",
			svc:  svcWithScheduler,
			ref:  gwapiv1.BackendRef{BackendObjectReference: gwapiv1.BackendObjectReference{Kind: ptr.To(gwapiv1.Kind("InferencePool")), Name: "custom-pool"}},
			want: true,
		},
		{
			name: "user InferencePool does not match",
			svc:  svc,
			ref:  gwapiv1.BackendRef{BackendObjectReference: gwapiv1.BackendObjectReference{Kind: ptr.To(gwapiv1.Kind("InferencePool")), Name: "user-custom-pool"}},
			want: false,
		},
		{
			name: "user Service does not match",
			svc:  svc,
			ref:  gwapiv1.BackendRef{BackendObjectReference: gwapiv1.BackendObjectReference{Kind: ptr.To(gwapiv1.Kind("Service")), Name: "user-custom-svc"}},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isExpectedBackendRef(tt.svc, tt.ref))
		})
	}
}

func TestInferencePoolAPIGroupFromRoute(t *testing.T) {
	tests := []struct {
		name  string
		route *gwapiv1.HTTPRoute
		want  string
	}{
		{
			name: "v1 backendRef",
			route: &gwapiv1.HTTPRoute{
				Spec: gwapiv1.HTTPRouteSpec{Rules: []gwapiv1.HTTPRouteRule{{BackendRefs: []gwapiv1.HTTPBackendRef{{BackendRef: gwapiv1.BackendRef{BackendObjectReference: gwapiv1.BackendObjectReference{
					Kind: ptr.To(gwapiv1.Kind("InferencePool")), Group: ptr.To(gwapiv1.Group(constants.InferencePoolV1APIGroupName)),
				}}}}}}},
			},
			want: constants.InferencePoolV1APIGroupName,
		},
		{
			name: "migration annotation",
			route: &gwapiv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{AnnotationInferencePoolMigrated: "v1"}},
			},
			want: constants.InferencePoolV1APIGroupName,
		},
		{
			name:  "empty route returns empty",
			route: &gwapiv1.HTTPRoute{},
			want:  "",
		},
		{
			name: "Service-only route returns empty",
			route: &gwapiv1.HTTPRoute{
				Spec: gwapiv1.HTTPRouteSpec{Rules: []gwapiv1.HTTPRouteRule{{BackendRefs: []gwapiv1.HTTPBackendRef{{BackendRef: gwapiv1.BackendRef{BackendObjectReference: gwapiv1.BackendObjectReference{
					Kind: ptr.To(gwapiv1.Kind("Service")), Name: "my-svc",
				}}}}}}},
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, inferencePoolAPIGroupFromRoute(tt.route))
		})
	}
}

func TestTrafficFieldsChanged(t *testing.T) {
	now := metav1.Now()
	tests := []struct {
		name string
		old  *v1alpha2.LLMInferenceService
		new  *v1alpha2.LLMInferenceService
		want bool
	}{
		{
			name: "no traffic fields on either",
			old:  &v1alpha2.LLMInferenceService{},
			new:  &v1alpha2.LLMInferenceService{},
			want: false,
		},
		{
			name: "group added",
			old:  &v1alpha2.LLMInferenceService{},
			new:  &v1alpha2.LLMInferenceService{Spec: v1alpha2.LLMInferenceServiceSpec{Router: &v1alpha2.RouterSpec{Route: &v1alpha2.GatewayRoutesSpec{Group: ptr.To("g")}}}},
			want: true,
		},
		{
			name: "group removed",
			old:  &v1alpha2.LLMInferenceService{Spec: v1alpha2.LLMInferenceServiceSpec{Router: &v1alpha2.RouterSpec{Route: &v1alpha2.GatewayRoutesSpec{Group: ptr.To("g")}}}},
			new:  &v1alpha2.LLMInferenceService{},
			want: true,
		},
		{
			name: "weight changed",
			old:  &v1alpha2.LLMInferenceService{Spec: v1alpha2.LLMInferenceServiceSpec{Router: &v1alpha2.RouterSpec{Route: &v1alpha2.GatewayRoutesSpec{Group: ptr.To("g"), Weight: ptr.To(int32(9))}}}},
			new:  &v1alpha2.LLMInferenceService{Spec: v1alpha2.LLMInferenceServiceSpec{Router: &v1alpha2.RouterSpec{Route: &v1alpha2.GatewayRoutesSpec{Group: ptr.To("g"), Weight: ptr.To(int32(5))}}}},
			want: true,
		},
		{
			name: "force-stop changed",
			old:  func() *v1alpha2.LLMInferenceService { s := memberSvc("v1", "g", 9, false, now); return &s }(),
			new:  func() *v1alpha2.LLMInferenceService { s := memberSvc("v1", "g", 9, true, now); return &s }(),
			want: true,
		},
		{
			name: "model name changed on grouped member",
			old:  func() *v1alpha2.LLMInferenceService { s := memberSvc("v1", "g", 9, false, now); return &s }(),
			new: func() *v1alpha2.LLMInferenceService {
				s := memberSvc("v1", "g", 9, false, now)
				s.Spec.Model.Name = ptr.To("different")
				return &s
			}(),
			want: true,
		},
		{
			name: "baseRefs changed on grouped member",
			old:  func() *v1alpha2.LLMInferenceService { s := memberSvc("v1", "g", 9, false, now); return &s }(),
			new: func() *v1alpha2.LLMInferenceService {
				s := memberSvc("v1", "g", 9, false, now)
				s.Spec.BaseRefs = []corev1.LocalObjectReference{{Name: "new-config"}}
				return &s
			}(),
			want: true,
		},
		{
			name: "scheduler added on grouped member",
			old: func() *v1alpha2.LLMInferenceService {
				s := memberSvc("v1", "g", 9, false, now)
				s.Spec.Router.Scheduler = nil
				return &s
			}(),
			new:  func() *v1alpha2.LLMInferenceService { s := memberSvc("v1", "g", 9, false, now); return &s }(),
			want: true,
		},
		{
			name: "scheduler removed on grouped member",
			old:  func() *v1alpha2.LLMInferenceService { s := memberSvc("v1", "g", 9, false, now); return &s }(),
			new: func() *v1alpha2.LLMInferenceService {
				s := memberSvc("v1", "g", 9, false, now)
				s.Spec.Router.Scheduler = nil
				return &s
			}(),
			want: true,
		},
		{
			name: "scheduler pool ref changed on grouped member",
			old: func() *v1alpha2.LLMInferenceService {
				s := memberSvc("v1", "g", 9, false, now)
				s.Spec.Router.Scheduler.Pool = &v1alpha2.InferencePoolSpec{
					Ref: &corev1.LocalObjectReference{Name: "old-pool"},
				}
				return &s
			}(),
			new: func() *v1alpha2.LLMInferenceService {
				s := memberSvc("v1", "g", 9, false, now)
				s.Spec.Router.Scheduler.Pool = &v1alpha2.InferencePoolSpec{
					Ref: &corev1.LocalObjectReference{Name: "new-pool"},
				}
				return &s
			}(),
			want: true,
		},
		{
			name: "scheduler pool ref added on grouped member",
			old:  func() *v1alpha2.LLMInferenceService { s := memberSvc("v1", "g", 9, false, now); return &s }(),
			new: func() *v1alpha2.LLMInferenceService {
				s := memberSvc("v1", "g", 9, false, now)
				s.Spec.Router.Scheduler.Pool = &v1alpha2.InferencePoolSpec{
					Ref: &corev1.LocalObjectReference{Name: "custom-pool"},
				}
				return &s
			}(),
			want: true,
		},
		{
			name: "non-traffic change on grouped member",
			old:  func() *v1alpha2.LLMInferenceService { s := memberSvc("v1", "g", 9, false, now); return &s }(),
			new:  func() *v1alpha2.LLMInferenceService { s := memberSvc("v1", "g", 9, false, now); return &s }(),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, trafficFieldsChanged(tt.old, tt.new))
		})
	}
}

// memberSvc creates a minimal LLMInferenceService for group testing.
func memberSvc(name, group string, weight int32, stopped bool, ts metav1.Time) v1alpha2.LLMInferenceService {
	svc := v1alpha2.LLMInferenceService{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "default",
			CreationTimestamp: ts,
		},
		Spec: v1alpha2.LLMInferenceServiceSpec{
			Model: v1alpha2.LLMModelSpec{
				Name: ptr.To("llama-70b"),
			},
			Router: &v1alpha2.RouterSpec{
				Route: &v1alpha2.GatewayRoutesSpec{
					Group:  ptr.To(group),
					Weight: ptr.To(weight),
				},
				Scheduler: &v1alpha2.SchedulerSpec{},
			},
		},
	}
	if stopped {
		svc.Annotations = map[string]string{
			constants.StopAnnotationKey: "true",
		}
	}
	return svc
}
