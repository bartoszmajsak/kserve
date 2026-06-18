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
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	v1alpha2 "github.com/kserve/kserve/pkg/apis/serving/v1alpha2"
	"github.com/kserve/kserve/pkg/constants"
	"github.com/kserve/kserve/pkg/utils"
)

const (
	reasonModelNameMismatch  = "ModelNameMismatch"
	reasonModelNameAmbiguous = "ModelNameAmbiguous"
)

var _ handler.EventHandler = &groupMemberEventHandler{}

const groupFieldIndex = ".spec.router.route.group"

// resolvedMember holds the effective weight and backend for a single group member.
type resolvedMember struct {
	name       string
	weight     int32
	stopped    bool
	backendRef gwapiv1.BackendObjectReference
}

// setupGroupFieldIndex registers a field indexer on spec.router.route.group
// for efficient group member discovery without listing all LLMISVCs in a namespace.
func setupGroupFieldIndex(ctx context.Context, mgr client.FieldIndexer) error {
	return mgr.IndexField(
		ctx,
		&v1alpha2.LLMInferenceService{},
		groupFieldIndex,
		func(obj client.Object) []string {
			llmSvc := obj.(*v1alpha2.LLMInferenceService)
			if g := llmSvc.Spec.Router.Group(); g != nil {
				return []string{*g}
			}
			return nil
		},
	)
}

// listGroupMembers returns all LLMISVCs in the same namespace with the same group.
func (r *LLMISVCReconciler) listGroupMembers(
	ctx context.Context,
	llmSvc *v1alpha2.LLMInferenceService,
) ([]v1alpha2.LLMInferenceService, error) {
	group := llmSvc.Spec.Router.Group()
	if group == nil {
		return nil, nil
	}
	list := &v1alpha2.LLMInferenceServiceList{}
	if err := r.List(ctx, list,
		client.InNamespace(llmSvc.Namespace),
		client.MatchingFields{groupFieldIndex: *group},
	); err != nil {
		return nil, fmt.Errorf("failed to list group members for group %q: %w", *group, err)
	}
	return list.Items, nil
}

func filterActiveMembers(members []v1alpha2.LLMInferenceService) []v1alpha2.LLMInferenceService {
	active := make([]v1alpha2.LLMInferenceService, 0, len(members))
	for i := range members {
		if members[i].DeletionTimestamp == nil {
			active = append(active, members[i])
		}
	}
	return active
}

// resolveGroupMembers builds the effective backend configuration for each group
// member. Each member may use a different backend type (InferencePool or Service)
// depending on their config-merged spec, enabling mixed groups during migration.
func (r *LLMISVCReconciler) resolveGroupMembers(
	ctx context.Context,
	members []v1alpha2.LLMInferenceService,
	cfg *Config,
) []resolvedMember {
	resolved := make([]resolvedMember, 0, len(members))
	for i := range members {
		m := &members[i]
		stopped := utils.GetForceStopRuntime(m)
		w := ptr.Deref(m.Spec.Router.Weight(), 0)
		if stopped {
			w = 0
		}

		backendRef, err := r.resolveMemberBackendRef(ctx, m, cfg)
		if err != nil {
			log.FromContext(ctx).Error(err, "skipping member in group - backend resolution failed",
				"member", m.Name)
			continue
		}

		resolved = append(resolved, resolvedMember{
			name:       m.Name,
			weight:     w,
			stopped:    stopped,
			backendRef: backendRef,
		})
	}

	slices.SortFunc(resolved, func(a, b resolvedMember) int {
		return cmp.Compare(a.name, b.name)
	})

	return resolved
}

// isExpectedBackendRef checks whether a backendRef matches the backend that
// the controller would produce for this LLMISVC: default InferencePool,
// scheduler pool ref, or workload Service. The spec must be config-merged.
func isExpectedBackendRef(llmSvc *v1alpha2.LLMInferenceService, ref gwapiv1.BackendRef) bool {
	if isDefaultBackendRef(llmSvc, ref) {
		return true
	}
	if ptr.Deref(ref.Kind, "Service") == "Service" &&
		string(ref.Name) == workloadServiceName(llmSvc) {
		return true
	}
	if llmSvc.Spec.Router != nil &&
		llmSvc.Spec.Router.Scheduler != nil &&
		llmSvc.Spec.Router.Scheduler.Pool.HasRef() &&
		ptr.Deref(ref.Kind, "") == "InferencePool" &&
		string(ref.Name) == llmSvc.Spec.Router.Scheduler.Pool.Ref.Name {
		return true
	}
	return false
}

// resolveMemberBackendRef derives the backend reference for a group member by
// merging its baseRef configs to get the effective spec. The config merge
// produces the final HTTPRoute with the correct backend - including custom
// pool refs (scheduler.pool.ref), default pool names, or bare Services.
func (r *LLMISVCReconciler) resolveMemberBackendRef(
	ctx context.Context,
	member *v1alpha2.LLMInferenceService,
	cfg *Config,
) (gwapiv1.BackendObjectReference, error) {
	combined, err := r.combineBaseRefsConfig(ctx, member, cfg)
	if err != nil {
		return gwapiv1.BackendObjectReference{}, err
	}

	// Use the merged spec for backendRef matching (scheduler/pool may come from baseRefs).
	mergedMember := *member
	mergedMember.Spec = combined.Config.Spec

	if combined.Config.Spec.Router != nil &&
		combined.Config.Spec.Router.Route != nil &&
		combined.Config.Spec.Router.Route.HTTP.HasSpec() {
		for _, rule := range combined.Config.Spec.Router.Route.HTTP.Spec.Rules {
			for _, ref := range rule.BackendRefs {
				if isExpectedBackendRef(&mergedMember, ref.BackendRef) {
					return ref.BackendObjectReference, nil
				}
			}
		}
	}

	return gwapiv1.BackendObjectReference{}, fmt.Errorf(
		"no controller-managed backend found for member %s/%s in config-merged route",
		member.Namespace, member.Name)
}

// injectGroupBackendRefs post-processes the template-rendered HTTPRoute to add
// weighted backendRefs for all group members. Each member's route carries
// backendRefs for ALL members so the gateway can distribute traffic proportionally.
func (r *LLMISVCReconciler) injectGroupBackendRefs(
	ctx context.Context,
	llmSvc *v1alpha2.LLMInferenceService,
	route *gwapiv1.HTTPRoute,
	cfg *Config,
) error {
	allMembers, err := r.listGroupMembers(ctx, llmSvc)
	if err != nil {
		return fmt.Errorf("injecting group backends: %w", err)
	}

	// Exclude terminating members from both validation and traffic shaping.
	members := filterActiveMembers(allMembers)

	// Resolve backends first - combineBaseRefsConfig updates each member's
	// Spec.Model.Name with the config-merged value, so validateGroupModelName
	// sees effective model names (including baseRef overrides).
	groupCfg := *cfg
	resolved := r.resolveGroupMembers(ctx, members, &groupCfg)

	// Validate model name consistency using post-merge names.
	canonicalModel, mismatched := validateGroupModelName(members)

	if isSelfMismatched(llmSvc, mismatched) {
		selfModel := ptr.Deref(llmSvc.Spec.Model.Name, llmSvc.Name)
		if canonicalModel == "" {
			llmSvc.MarkTrafficGroupNotReady(reasonModelNameAmbiguous,
				ambiguousModelMessage(members))
			r.Eventf(llmSvc, corev1.EventTypeWarning, reasonModelNameAmbiguous,
				"model.name %q does not match other group members; group has no majority and is inactive",
				selfModel)
		} else {
			llmSvc.MarkTrafficGroupNotReady(reasonModelNameMismatch, fmt.Sprintf(
				"member %q has model.name %q, but group serves %q; "+
					"all group members must serve the same model",
				llmSvc.Name, selfModel, canonicalModel,
			))
			r.Eventf(llmSvc, corev1.EventTypeWarning, reasonModelNameMismatch,
				"model.name %q differs from group model %q; excluded from traffic splitting",
				selfModel, canonicalModel)
		}
		if llmSvc.Status.Router != nil {
			llmSvc.Status.Router.Group = nil
		}
		return nil
	}

	if len(mismatched) > 0 {
		for _, mm := range mismatched {
			r.Eventf(llmSvc, corev1.EventTypeWarning, reasonModelNameMismatch,
				"Group member %s has model.name %q (expected %q) and is excluded from traffic splitting",
				mm.member, mm.memberModel, canonicalModel)
		}
	}

	// Filter resolved members to only include conforming ones.
	excludedNames := make(map[string]bool, len(mismatched))
	for _, mm := range mismatched {
		excludedNames[mm.member.Name] = true
	}
	conformingResolved := make([]resolvedMember, 0, len(resolved))
	for _, rm := range resolved {
		if !excludedNames[rm.name] {
			conformingResolved = append(conformingResolved, rm)
		}
	}
	resolved = conformingResolved

	if len(resolved) == 0 {
		llmSvc.MarkTrafficGroupNotReady("BackendResolutionFailed",
			"no group members could be resolved - check member configs and baseRefs")
		if llmSvc.Status.Router != nil {
			llmSvc.Status.Router.Group = nil
		}
		return nil
	}

	// Align InferencePool API group across all members. combineBaseRefsConfig
	// returns the template default (v1alpha2), but the cluster may have
	// migrated to v1. Check self's route first, then fall back to querying
	// any peer's existing route for the migration annotation.
	r.alignInferencePoolAPIGroup(ctx, llmSvc, route, resolved)

	rewriteRulesForGroup(route, llmSvc, resolved)
	updateGroupStatus(llmSvc, resolved)

	llmSvc.MarkTrafficGroupReady()

	conformingCount := len(members) - len(mismatched)
	if len(resolved) < conformingCount {
		dropped := conformingCount - len(resolved)
		llmSvc.MarkTrafficGroupDegraded("PartialBackendResolution",
			"%d of %d conforming group members could not be resolved - check member configs and baseRefs",
			dropped, conformingCount)
		r.Eventf(llmSvc, corev1.EventTypeWarning, "PartialBackendResolution",
			"%d group members could not be resolved - traffic splitting is degraded", dropped)
	} else {
		llmSvc.MarkTrafficGroupNotDegraded()
	}

	return nil
}

// inferencePoolAPIGroupFromRoute determines the InferencePool API group from
// a route. Checks backendRefs first (authoritative after migration), then
// the migration annotation.
func inferencePoolAPIGroupFromRoute(route *gwapiv1.HTTPRoute) string {
	for _, rule := range route.Spec.Rules {
		for _, ref := range rule.BackendRefs {
			if ref.Kind != nil && *ref.Kind == "InferencePool" && ref.Group != nil {
				return string(*ref.Group)
			}
		}
	}
	if route.Annotations != nil {
		if _, migrated := route.Annotations[AnnotationInferencePoolMigrated]; migrated {
			return constants.InferencePoolV1APIGroupName
		}
	}
	return ""
}

// alignInferencePoolAPIGroup ensures all InferencePool backendRefs in the
// resolved members use a consistent API group. Determines the migration state
// from self's route first. If self has no InferencePool refs (Service-only
// member in a mixed group), queries peer routes to find the migration state.
func (r *LLMISVCReconciler) alignInferencePoolAPIGroup(
	ctx context.Context,
	llmSvc *v1alpha2.LLMInferenceService,
	selfRoute *gwapiv1.HTTPRoute,
	resolved []resolvedMember,
) {
	hasInferencePool := false
	for _, m := range resolved {
		if m.backendRef.Kind != nil && *m.backendRef.Kind == "InferencePool" {
			hasInferencePool = true
			break
		}
	}
	if !hasInferencePool {
		return
	}

	group := inferencePoolAPIGroupFromRoute(selfRoute)

	// Self has no InferencePool refs (Service-only in mixed group) -
	// check peer routes for the cluster's migration state. The peer's
	// existing route has both the API group in backendRefs and the
	// ResolvedRefs status from the gateway.
	if group == "" {
		group = r.detectPeerInferencePoolAPIGroup(ctx, llmSvc, resolved)
	}

	if group == "" {
		return
	}

	for i := range resolved {
		if resolved[i].backendRef.Kind != nil && *resolved[i].backendRef.Kind == "InferencePool" {
			resolved[i].backendRef.Group = ptr.To(gwapiv1.Group(group))
		}
	}
}

// detectPeerInferencePoolAPIGroup finds the first peer with an existing route
// and reads its InferencePool API group. The migration state is cluster-wide
// (same gateway, same CRDs), so any peer's route is authoritative.
func (r *LLMISVCReconciler) detectPeerInferencePoolAPIGroup(
	ctx context.Context,
	llmSvc *v1alpha2.LLMInferenceService,
	resolved []resolvedMember,
) string {
	for _, m := range resolved {
		if m.backendRef.Kind == nil || *m.backendRef.Kind != "InferencePool" {
			continue
		}
		peerRoutes := &gwapiv1.HTTPRouteList{}
		if err := r.List(ctx, peerRoutes,
			client.InNamespace(llmSvc.Namespace),
			client.MatchingLabels{
				constants.KubernetesAppNameLabelKey:   m.name,
				constants.KubernetesComponentLabelKey: constants.LLMComponentRouter,
			},
		); err != nil {
			log.FromContext(ctx).V(1).Error(err, "failed to list peer routes for API group detection",
				"peer", m.name)
			continue
		}
		for i := range peerRoutes.Items {
			if group := inferencePoolAPIGroupFromRoute(&peerRoutes.Items[i]); group != "" {
				return group
			}
		}
	}
	return ""
}

// updateGroupStatus populates the group topology in the LLMISVC status.
func updateGroupStatus(llmSvc *v1alpha2.LLMInferenceService, resolved []resolvedMember) {
	groupStatus := &v1alpha2.GroupStatus{
		Name: *llmSvc.Spec.Router.Route.Group,
	}

	for _, m := range resolved {
		ref := m.backendRef
		groupStatus.Members = append(groupStatus.Members, v1alpha2.GroupMemberStatus{
			Name:       m.name,
			Weight:     m.weight,
			Stopped:    m.stopped,
			BackendRef: &ref,
		})
	}

	if llmSvc.Status.Router == nil {
		llmSvc.Status.Router = &v1alpha2.RouterStatus{}
	}
	llmSvc.Status.Router.Group = groupStatus
}

// rewriteRulesForGroup replaces backendRefs on rules that have a controller-managed
// backend with the full set of weighted group member backends. Rules with only
// user-managed custom backendRefs are left untouched.
func rewriteRulesForGroup(route *gwapiv1.HTTPRoute, llmSvc *v1alpha2.LLMInferenceService, members []resolvedMember) {
	allBackendRefs := make([]gwapiv1.HTTPBackendRef, 0, len(members))
	for _, m := range members {
		allBackendRefs = append(allBackendRefs, gwapiv1.HTTPBackendRef{
			BackendRef: gwapiv1.BackendRef{
				BackendObjectReference: m.backendRef,
				Weight:                 ptr.To(m.weight),
			},
		})
	}

	perParticipantPrefix := "/" + llmSvc.Namespace + "/" + llmSvc.Name

	for i := range route.Spec.Rules {
		if isPerParticipantRule(route.Spec.Rules[i], perParticipantPrefix) {
			continue
		}
		if !hasControllerManagedBackendRef(route.Spec.Rules[i], llmSvc) {
			continue
		}
		route.Spec.Rules[i].BackendRefs = slices.Clone(allBackendRefs)
	}
}

// isPerParticipantRule returns true for rules whose path matches the
// per-participant pattern (/{namespace}/{name}/...). These are direct-access
// routes unique to this member and should not get weighted group backendRefs.
func isPerParticipantRule(rule gwapiv1.HTTPRouteRule, perParticipantPrefix string) bool {
	for _, match := range rule.Matches {
		if match.Path == nil || match.Path.Value == nil {
			continue
		}
		path := *match.Path.Value
		if path == perParticipantPrefix ||
			strings.HasPrefix(path, perParticipantPrefix+"/") {
			return true
		}
	}
	return false
}

// hasControllerManagedBackendRef returns true if any backendRef in the rule is
// a controller-managed backend. At this point llmSvc.Spec is config-merged,
// so Scheduler reflects the effective config including baseRef inheritance.
func hasControllerManagedBackendRef(rule gwapiv1.HTTPRouteRule, llmSvc *v1alpha2.LLMInferenceService) bool {
	for _, ref := range rule.BackendRefs {
		if isExpectedBackendRef(llmSvc, ref.BackendRef) {
			return true
		}
	}
	return false
}

// groupMemberEventHandler implements handler.EventHandler to enqueue group
// siblings when a grouped LLMISVC changes. Using a typed handler (instead of
// EnqueueRequestsFromMapFunc) gives access to both old and new objects on
// updates - needed for old-group cleanup since the defaulting webhook
// synchronizes label and spec before the controller sees the new object.
type groupMemberEventHandler struct {
	reconciler *LLMISVCReconciler
}

func (h *groupMemberEventHandler) Create(ctx context.Context, e event.CreateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	h.enqueueCurrentGroupMembers(ctx, e.Object, q)
}

func (h *groupMemberEventHandler) Update(ctx context.Context, e event.UpdateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	oldSvc := e.ObjectOld.(*v1alpha2.LLMInferenceService)
	newSvc := e.ObjectNew.(*v1alpha2.LLMInferenceService)

	h.enqueueCurrentGroupMembers(ctx, newSvc, q)

	// When group changes, also enqueue OLD group members so they remove the
	// departed member's backendRef. The old object carries the previous group
	// value (informer cache snapshot before the update).
	oldGroup := oldSvc.Spec.Router.Group()
	newGroup := newSvc.Spec.Router.Group()
	if oldGroup != nil && !ptr.Equal(oldGroup, newGroup) {
		h.enqueueOldGroupMembers(ctx, oldSvc, newSvc.Name, q)
	}
}

func (h *groupMemberEventHandler) Delete(ctx context.Context, e event.DeleteEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	h.enqueueCurrentGroupMembers(ctx, e.Object, q)
}

func (h *groupMemberEventHandler) Generic(_ context.Context, _ event.GenericEvent, _ workqueue.TypedRateLimitingInterface[reconcile.Request]) {
}

func (h *groupMemberEventHandler) enqueueCurrentGroupMembers(ctx context.Context, obj client.Object, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	llmSvc := obj.(*v1alpha2.LLMInferenceService)
	if !llmSvc.Spec.Router.HasGroup() {
		return
	}

	members, err := h.reconciler.listGroupMembers(ctx, llmSvc)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to list group members for enqueue")
		return
	}

	for i := range members {
		if members[i].Name == llmSvc.Name {
			continue
		}
		q.Add(reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: members[i].Namespace,
				Name:      members[i].Name,
			},
		})
	}
}

func (h *groupMemberEventHandler) enqueueOldGroupMembers(
	ctx context.Context,
	oldSvc *v1alpha2.LLMInferenceService,
	selfName string,
	q workqueue.TypedRateLimitingInterface[reconcile.Request],
) {
	oldGroup := *oldSvc.Spec.Router.Group()
	list := &v1alpha2.LLMInferenceServiceList{}
	if err := h.reconciler.List(ctx, list,
		client.InNamespace(oldSvc.Namespace),
		client.MatchingLabels{constants.LLMRoutingGroupLabelKey: oldGroup},
	); err != nil {
		log.FromContext(ctx).Error(err, "failed to list old group members for enqueue",
			"oldGroup", oldGroup)
		return
	}

	for i := range list.Items {
		if list.Items[i].Name == selfName {
			continue
		}
		q.Add(reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: list.Items[i].Namespace,
				Name:      list.Items[i].Name,
			},
		})
	}
}

// groupMemberChangePredicate limits fan-out to traffic-relevant changes only.
// Without this, any spec change to a grouped LLMISVC triggers reconciliation
// of all group members.
func groupMemberChangePredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return e.Object.(*v1alpha2.LLMInferenceService).Spec.Router.HasGroup()
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldSvc := e.ObjectOld.(*v1alpha2.LLMInferenceService)
			newSvc := e.ObjectNew.(*v1alpha2.LLMInferenceService)
			return trafficFieldsChanged(oldSvc, newSvc)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return e.Object.(*v1alpha2.LLMInferenceService).Spec.Router.HasGroup()
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
	}
}

func trafficFieldsChanged(old, new *v1alpha2.LLMInferenceService) bool {
	oldGroup := old.Spec.Router.Group()
	newGroup := new.Spec.Router.Group()

	if !ptr.Equal(oldGroup, newGroup) {
		return true
	}

	oldWeight := old.Spec.Router.Weight()
	newWeight := new.Spec.Router.Weight()

	if !ptr.Equal(oldWeight, newWeight) {
		return true
	}

	if oldGroup == nil {
		return false
	}

	if utils.GetForceStopRuntime(old) != utils.GetForceStopRuntime(new) {
		return true
	}

	if ptr.Deref(old.Spec.Model.Name, old.Name) != ptr.Deref(new.Spec.Model.Name, new.Name) {
		return true
	}

	if !slices.Equal(old.Spec.BaseRefs, new.Spec.BaseRefs) {
		return true
	}

	oldHasScheduler := old.Spec.Router != nil && old.Spec.Router.Scheduler != nil
	newHasScheduler := new.Spec.Router != nil && new.Spec.Router.Scheduler != nil
	if oldHasScheduler != newHasScheduler {
		return true
	}
	if oldHasScheduler && newHasScheduler {
		oldPoolRef := old.Spec.Router.Scheduler.Pool.HasRef()
		newPoolRef := new.Spec.Router.Scheduler.Pool.HasRef()
		if oldPoolRef != newPoolRef {
			return true
		}
		if oldPoolRef && newPoolRef &&
			old.Spec.Router.Scheduler.Pool.Ref.Name != new.Spec.Router.Scheduler.Pool.Ref.Name {
			return true
		}
	}

	return false
}
