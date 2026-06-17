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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha2 "github.com/kserve/kserve/pkg/apis/serving/v1alpha2"
	"github.com/kserve/kserve/pkg/constants"
)

func TestGroupMemberEventHandler_Update_OldGroupFanout(t *testing.T) {
	v1Svc := groupedSvc("v1", "group-a")
	v2Svc := groupedSvc("v2", "group-a")
	// v3 is changing from group-a to group-b
	v3Old := groupedSvc("v3", "group-a")
	v3New := groupedSvc("v3", "group-b")

	fakeClient := fakeClientWithIndex(t, &v1Svc, &v2Svc, &v3New)
	h := &groupMemberEventHandler{reconciler: &LLMISVCReconciler{Client: fakeClient}}

	reqs := drainUpdateEvent(t, h, &v3Old, &v3New)

	names := requestNames(reqs)
	assert.Contains(t, names, "v1", "old group member v1 should be enqueued")
	assert.Contains(t, names, "v2", "old group member v2 should be enqueued")
	assert.NotContains(t, names, "v3", "self should not be enqueued")
}

func TestGroupMemberEventHandler_Update_GroupRemoved(t *testing.T) {
	v1Svc := groupedSvc("v1", "group-a")
	// v2 is leaving the group entirely
	v2Old := groupedSvc("v2", "group-a")
	v2New := v1alpha2.LLMInferenceService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "v2",
			Namespace: "default",
			Labels:    map[string]string{constants.LLMRoutingGroupLabelKey: "group-a"},
		},
		Spec: v1alpha2.LLMInferenceServiceSpec{
			Router: &v1alpha2.RouterSpec{
				Route: &v1alpha2.GatewayRoutesSpec{},
			},
		},
	}

	fakeClient := fakeClientWithIndex(t, &v1Svc, &v2New)
	h := &groupMemberEventHandler{reconciler: &LLMISVCReconciler{Client: fakeClient}}

	reqs := drainUpdateEvent(t, h, &v2Old, &v2New)

	names := requestNames(reqs)
	assert.Contains(t, names, "v1", "old group member v1 should be enqueued when v2 leaves")
}

func TestGroupMemberEventHandler_Update_WeightChange(t *testing.T) {
	v1Svc := groupedSvc("v1", "group-a")
	v2Old := groupedSvc("v2", "group-a")
	v2New := groupedSvc("v2", "group-a")
	v2New.Spec.Router.Route.Weight = ptr.To(int32(5))

	fakeClient := fakeClientWithIndex(t, &v1Svc, &v2New)
	h := &groupMemberEventHandler{reconciler: &LLMISVCReconciler{Client: fakeClient}}

	reqs := drainUpdateEvent(t, h, &v2Old, &v2New)

	names := requestNames(reqs)
	assert.Contains(t, names, "v1", "group member v1 should be enqueued on v2 weight change")
	assert.NotContains(t, names, "v2", "self should not be enqueued")
}

func TestGroupMemberEventHandler_Delete(t *testing.T) {
	v1Svc := groupedSvc("v1", "group-a")
	v2Svc := groupedSvc("v2", "group-a")

	fakeClient := fakeClientWithIndex(t, &v1Svc, &v2Svc)
	h := &groupMemberEventHandler{reconciler: &LLMISVCReconciler{Client: fakeClient}}

	q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer q.ShutDown()

	h.Delete(t.Context(), event.DeleteEvent{Object: &v2Svc}, q)

	reqs := drainQueue(q)
	names := requestNames(reqs)
	assert.Contains(t, names, "v1", "group member v1 should be enqueued on v2 delete")
	assert.NotContains(t, names, "v2", "deleted self should not be enqueued")
}

func TestGroupMemberEventHandler_NonGrouped_NoEnqueue(t *testing.T) {
	nonGrouped := v1alpha2.LLMInferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "solo", Namespace: "default"},
		Spec:       v1alpha2.LLMInferenceServiceSpec{},
	}

	fakeClient := fakeClientWithIndex(t, &nonGrouped)
	h := &groupMemberEventHandler{reconciler: &LLMISVCReconciler{Client: fakeClient}}

	q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer q.ShutDown()

	h.Create(t.Context(), event.CreateEvent{Object: &nonGrouped}, q)

	assert.Equal(t, 0, q.Len(), "non-grouped member should not enqueue anything")
}

func groupedSvc(name, group string) v1alpha2.LLMInferenceService {
	return v1alpha2.LLMInferenceService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels:    map[string]string{constants.LLMRoutingGroupLabelKey: group},
		},
		Spec: v1alpha2.LLMInferenceServiceSpec{
			Router: &v1alpha2.RouterSpec{
				Route: &v1alpha2.GatewayRoutesSpec{
					Group:  ptr.To(group),
					Weight: ptr.To(int32(1)),
				},
			},
		},
	}
}

func fakeClientWithIndex(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha2.AddToScheme(scheme))

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithIndex(&v1alpha2.LLMInferenceService{}, groupFieldIndex, func(obj client.Object) []string {
			llmSvc := obj.(*v1alpha2.LLMInferenceService)
			if g := llmSvc.Spec.Router.Group(); g != nil {
				return []string{*g}
			}
			return nil
		}).
		Build()
}

func drainUpdateEvent(t *testing.T, h *groupMemberEventHandler, oldObj, newObj *v1alpha2.LLMInferenceService) []reconcile.Request {
	t.Helper()
	q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer q.ShutDown()

	h.Update(t.Context(), event.UpdateEvent{ObjectOld: oldObj, ObjectNew: newObj}, q)

	return drainQueue(q)
}

func drainQueue(q workqueue.TypedRateLimitingInterface[reconcile.Request]) []reconcile.Request {
	reqs := make([]reconcile.Request, 0, q.Len())
	for q.Len() > 0 {
		req, shutdown := q.Get()
		if shutdown {
			break
		}
		reqs = append(reqs, req)
		q.Done(req)
		q.Forget(req)
	}
	return reqs
}

func requestNames(reqs []reconcile.Request) []string {
	names := make([]string, len(reqs))
	for i, r := range reqs {
		names[i] = r.Name
	}
	return names
}
