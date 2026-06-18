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

package llmisvc_test

import (
	"context"

	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/kserve/kserve/pkg/apis/serving/v1alpha2"
	"github.com/kserve/kserve/pkg/constants"
	. "github.com/kserve/kserve/pkg/controller/v1alpha2/llmisvc/fixture"
)

var _ = Describe("LLMInferenceService Group Routing", func() {
	Context("Group formation", func() {
		It("should form a group with weighted backendRefs when two members share the same group", func(ctx SpecContext) {
			// given
			groupName := "my-group"
			svcNameA := "test-group-a"
			svcNameB := "test-group-b"
			testNs := NewTestNamespace(ctx, envTest,
				WithIstioShadowService(svcNameA),
				WithIstioShadowService(svcNameB),
			)

			llmSvcA := LLMInferenceService(svcNameA,
				InNamespace[*v1alpha2.LLMInferenceService](testNs.Name),
				WithModelURI("hf://facebook/opt-125m"),
				WithModelName("facebook/opt-125m"),
				WithManagedRoute(),
				WithManagedGateway(),
				WithManagedScheduler(),
				WithGroup(groupName),
				WithWeight(80),
			)

			llmSvcB := LLMInferenceService(svcNameB,
				InNamespace[*v1alpha2.LLMInferenceService](testNs.Name),
				WithModelURI("hf://facebook/opt-125m"),
				WithModelName("facebook/opt-125m"),
				WithManagedRoute(),
				WithManagedGateway(),
				WithManagedScheduler(),
				WithGroup(groupName),
				WithWeight(20),
			)

			// when
			Expect(envTest.Create(ctx, llmSvcA)).To(Succeed())
			Expect(envTest.Create(ctx, llmSvcB)).To(Succeed())
			defer func() {
				testNs.DeleteAndWait(ctx, llmSvcA)
				testNs.DeleteAndWait(ctx, llmSvcB)
			}()

			ensureRouterManagedResourcesAreReady(ctx, envTest.Client, llmSvcA)
			ensureRouterManagedResourcesAreReady(ctx, envTest.Client, llmSvcB)

			// then - both members should have the routing-group label
			Eventually(func(g Gomega, ctx context.Context) {
				currentA := &v1alpha2.LLMInferenceService{}
				g.Expect(envTest.Get(ctx, client.ObjectKeyFromObject(llmSvcA), currentA)).To(Succeed())
				g.Expect(currentA.Labels).To(HaveKeyWithValue(constants.LLMRoutingGroupLabelKey, groupName))

				currentB := &v1alpha2.LLMInferenceService{}
				g.Expect(envTest.Get(ctx, client.ObjectKeyFromObject(llmSvcB), currentB)).To(Succeed())
				g.Expect(currentB.Labels).To(HaveKeyWithValue(constants.LLMRoutingGroupLabelKey, groupName))
			}).WithContext(ctx).Should(Succeed())

			// then - HTTPRoute for member A should have weighted backendRefs for both members
			Eventually(func(g Gomega, ctx context.Context) {
				routes, err := managedRoutes(ctx, llmSvcA)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(routes).To(HaveLen(1))

				backendRefs := firstRuleBackendRefs(&routes[0])
				g.Expect(backendRefs).To(HaveLen(2), "should have backendRefs for both group members")

				g.Expect(backendRefs).To(ContainElement(
					SatisfyAll(
						HaveBackendName(svcNameA),
						HaveBackendWeight(int32(80)),
					),
				))
				g.Expect(backendRefs).To(ContainElement(
					SatisfyAll(
						HaveBackendName(svcNameB),
						HaveBackendWeight(int32(20)),
					),
				))
			}).WithContext(ctx).Should(Succeed())

			// then - group status should be populated on both members
			Eventually(func(g Gomega, ctx context.Context) {
				currentA := &v1alpha2.LLMInferenceService{}
				g.Expect(envTest.Get(ctx, client.ObjectKeyFromObject(llmSvcA), currentA)).To(Succeed())

				g.Expect(currentA.Status.Router).ToNot(BeNil())
				g.Expect(currentA.Status.Router.Group).ToNot(BeNil())
				g.Expect(currentA.Status.Router.Group.Name).To(Equal(groupName))
				g.Expect(currentA.Status.Router.Group.Members).To(HaveLen(2))

				cond := currentA.Status.GetCondition(v1alpha2.TrafficGroupReady)
				g.Expect(cond).ToNot(BeNil())
				g.Expect(cond.IsTrue()).To(BeTrue())
			}).WithContext(ctx).Should(Succeed())

			Eventually(func(g Gomega, ctx context.Context) {
				currentB := &v1alpha2.LLMInferenceService{}
				g.Expect(envTest.Get(ctx, client.ObjectKeyFromObject(llmSvcB), currentB)).To(Succeed())

				g.Expect(currentB.Status.Router).ToNot(BeNil())
				g.Expect(currentB.Status.Router.Group).ToNot(BeNil())
				g.Expect(currentB.Status.Router.Group.Name).To(Equal(groupName))
				g.Expect(currentB.Status.Router.Group.Members).To(HaveLen(2))

				cond := currentB.Status.GetCondition(v1alpha2.TrafficGroupReady)
				g.Expect(cond).ToNot(BeNil())
				g.Expect(cond.IsTrue()).To(BeTrue())
			}).WithContext(ctx).Should(Succeed())
		})
	})

	Context("Weight change", func() {
		It("should update HTTPRoute backendRefs when a member's weight changes", func(ctx SpecContext) {
			// given
			groupName := "weight-group"
			svcNameA := "test-weight-a"
			svcNameB := "test-weight-b"
			testNs := NewTestNamespace(ctx, envTest,
				WithIstioShadowService(svcNameA),
				WithIstioShadowService(svcNameB),
			)

			llmSvcA := LLMInferenceService(svcNameA,
				InNamespace[*v1alpha2.LLMInferenceService](testNs.Name),
				WithModelURI("hf://facebook/opt-125m"),
				WithModelName("facebook/opt-125m"),
				WithManagedRoute(),
				WithManagedGateway(),
				WithManagedScheduler(),
				WithGroup(groupName),
				WithWeight(50),
			)

			llmSvcB := LLMInferenceService(svcNameB,
				InNamespace[*v1alpha2.LLMInferenceService](testNs.Name),
				WithModelURI("hf://facebook/opt-125m"),
				WithModelName("facebook/opt-125m"),
				WithManagedRoute(),
				WithManagedGateway(),
				WithManagedScheduler(),
				WithGroup(groupName),
				WithWeight(50),
			)

			Expect(envTest.Create(ctx, llmSvcA)).To(Succeed())
			Expect(envTest.Create(ctx, llmSvcB)).To(Succeed())
			defer func() {
				testNs.DeleteAndWait(ctx, llmSvcA)
				testNs.DeleteAndWait(ctx, llmSvcB)
			}()

			ensureRouterManagedResourcesAreReady(ctx, envTest.Client, llmSvcA)
			ensureRouterManagedResourcesAreReady(ctx, envTest.Client, llmSvcB)

			// Verify initial equal weights
			Eventually(func(g Gomega, ctx context.Context) {
				routes, err := managedRoutes(ctx, llmSvcA)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(routes).To(HaveLen(1))

				backendRefs := firstRuleBackendRefs(&routes[0])
				g.Expect(backendRefs).To(HaveLen(2))
				g.Expect(backendRefs).To(ContainElement(HaveBackendWeight(int32(50))))
			}).WithContext(ctx).Should(Succeed())

			// when - change weight on member A
			errRetry := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				_, errUpdate := ctrl.CreateOrUpdate(ctx, envTest.Client, llmSvcA, func() error {
					llmSvcA.Spec.Router.Route.Weight = ptr.To[int32](90)
					return nil
				})
				return errUpdate
			})
			Expect(errRetry).ToNot(HaveOccurred())

			// then - backendRefs should reflect the updated weights
			Eventually(func(g Gomega, ctx context.Context) {
				routes, err := managedRoutes(ctx, llmSvcA)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(routes).To(HaveLen(1))

				backendRefs := firstRuleBackendRefs(&routes[0])
				g.Expect(backendRefs).To(HaveLen(2))
				g.Expect(backendRefs).To(ContainElement(
					SatisfyAll(
						HaveBackendName(svcNameA),
						HaveBackendWeight(int32(90)),
					),
				))
				g.Expect(backendRefs).To(ContainElement(
					SatisfyAll(
						HaveBackendName(svcNameB),
						HaveBackendWeight(int32(50)),
					),
				))
			}).WithContext(ctx).Should(Succeed())
		})
	})

	Context("Model name mismatch", func() {
		It("should exclude a mismatched member from backendRefs while conforming members stay Ready", func(ctx SpecContext) {
			// given
			groupName := "mismatch-group"
			svcNameA := "test-mm-a"
			svcNameB := "test-mm-b"
			svcNameC := "test-mm-c"
			testNs := NewTestNamespace(ctx, envTest,
				WithIstioShadowService(svcNameA),
				WithIstioShadowService(svcNameB),
				WithIstioShadowService(svcNameC),
			)

			// A and B share the same model name (majority)
			llmSvcA := LLMInferenceService(svcNameA,
				InNamespace[*v1alpha2.LLMInferenceService](testNs.Name),
				WithModelURI("hf://facebook/opt-125m"),
				WithModelName("facebook/opt-125m"),
				WithManagedRoute(),
				WithManagedGateway(),
				WithManagedScheduler(),
				WithGroup(groupName),
				WithWeight(50),
			)

			llmSvcB := LLMInferenceService(svcNameB,
				InNamespace[*v1alpha2.LLMInferenceService](testNs.Name),
				WithModelURI("hf://facebook/opt-125m"),
				WithModelName("facebook/opt-125m"),
				WithManagedRoute(),
				WithManagedGateway(),
				WithManagedScheduler(),
				WithGroup(groupName),
				WithWeight(30),
			)

			// C has a different model name - should be excluded
			llmSvcC := LLMInferenceService(svcNameC,
				InNamespace[*v1alpha2.LLMInferenceService](testNs.Name),
				WithModelURI("hf://meta-llama/llama-2-7b"),
				WithModelName("meta-llama/llama-2-7b"),
				WithManagedRoute(),
				WithManagedGateway(),
				WithManagedScheduler(),
				WithGroup(groupName),
				WithWeight(20),
			)

			// when
			Expect(envTest.Create(ctx, llmSvcA)).To(Succeed())
			Expect(envTest.Create(ctx, llmSvcB)).To(Succeed())
			Expect(envTest.Create(ctx, llmSvcC)).To(Succeed())
			defer func() {
				testNs.DeleteAndWait(ctx, llmSvcA)
				testNs.DeleteAndWait(ctx, llmSvcB)
				testNs.DeleteAndWait(ctx, llmSvcC)
			}()

			ensureRouterManagedResourcesAreReady(ctx, envTest.Client, llmSvcA)
			ensureRouterManagedResourcesAreReady(ctx, envTest.Client, llmSvcB)
			ensureRouterManagedResourcesAreReady(ctx, envTest.Client, llmSvcC)

			// then - mismatched member C should get TrafficGroupReady=False with ModelNameMismatch
			Eventually(func(g Gomega, ctx context.Context) {
				currentC := &v1alpha2.LLMInferenceService{}
				g.Expect(envTest.Get(ctx, client.ObjectKeyFromObject(llmSvcC), currentC)).To(Succeed())

				cond := currentC.Status.GetCondition(v1alpha2.TrafficGroupReady)
				g.Expect(cond).ToNot(BeNil())
				g.Expect(cond.IsFalse()).To(BeTrue())
				g.Expect(cond.Reason).To(Equal("ModelNameMismatch"))
			}).WithContext(ctx).Should(Succeed())

			// then - conforming members A and B should remain TrafficGroupReady=True
			Eventually(func(g Gomega, ctx context.Context) {
				currentA := &v1alpha2.LLMInferenceService{}
				g.Expect(envTest.Get(ctx, client.ObjectKeyFromObject(llmSvcA), currentA)).To(Succeed())

				cond := currentA.Status.GetCondition(v1alpha2.TrafficGroupReady)
				g.Expect(cond).ToNot(BeNil())
				g.Expect(cond.IsTrue()).To(BeTrue())
			}).WithContext(ctx).Should(Succeed())

			// then - backendRefs on member A should contain only A and B (not C)
			Eventually(func(g Gomega, ctx context.Context) {
				routes, err := managedRoutes(ctx, llmSvcA)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(routes).To(HaveLen(1))

				backendRefs := firstRuleBackendRefs(&routes[0])
				g.Expect(backendRefs).To(HaveLen(2), "only conforming members should be in backendRefs")

				backendNames := backendRefNames(backendRefs)
				g.Expect(backendNames).To(ContainElements(
					ContainSubstring(svcNameA),
					ContainSubstring(svcNameB),
				))
				g.Expect(backendNames).ToNot(ContainElement(ContainSubstring(svcNameC)))
			}).WithContext(ctx).Should(Succeed())
		})
	})

	Context("Model name ambiguous (no majority)", func() {
		It("should mark all members TrafficGroupReady=False without blocking Ready", func(ctx SpecContext) {
			// given - two members with different model names (1-1 split, no majority)
			groupName := "ambiguous-group"
			svcNameA := "test-ambig-a"
			svcNameB := "test-ambig-b"
			testNs := NewTestNamespace(ctx, envTest,
				WithIstioShadowService(svcNameA),
				WithIstioShadowService(svcNameB),
			)

			llmSvcA := LLMInferenceService(svcNameA,
				InNamespace[*v1alpha2.LLMInferenceService](testNs.Name),
				WithModelURI("hf://facebook/opt-125m"),
				WithModelName("model-alpha"),
				WithManagedRoute(),
				WithManagedGateway(),
				WithManagedScheduler(),
				WithGroup(groupName),
				WithWeight(50),
			)

			llmSvcB := LLMInferenceService(svcNameB,
				InNamespace[*v1alpha2.LLMInferenceService](testNs.Name),
				WithModelURI("hf://facebook/opt-125m"),
				WithModelName("model-beta"),
				WithManagedRoute(),
				WithManagedGateway(),
				WithManagedScheduler(),
				WithGroup(groupName),
				WithWeight(50),
			)

			// when
			Expect(envTest.Create(ctx, llmSvcA)).To(Succeed())
			Expect(envTest.Create(ctx, llmSvcB)).To(Succeed())
			defer func() {
				testNs.DeleteAndWait(ctx, llmSvcA)
				testNs.DeleteAndWait(ctx, llmSvcB)
			}()

			ensureRouterManagedResourcesAreReady(ctx, envTest.Client, llmSvcA)
			ensureRouterManagedResourcesAreReady(ctx, envTest.Client, llmSvcB)

			// then - both members should get TrafficGroupReady=False with ModelNameAmbiguous
			for _, svc := range []*v1alpha2.LLMInferenceService{llmSvcA, llmSvcB} {
				svc := svc
				Eventually(func(g Gomega, ctx context.Context) {
					current := &v1alpha2.LLMInferenceService{}
					g.Expect(envTest.Get(ctx, client.ObjectKeyFromObject(svc), current)).To(Succeed())

					cond := current.Status.GetCondition(v1alpha2.TrafficGroupReady)
					g.Expect(cond).ToNot(BeNil())
					g.Expect(cond.IsFalse()).To(BeTrue())
					g.Expect(cond.Reason).To(Equal("ModelNameAmbiguous"))

					// TrafficGroupReady=False must NOT cascade to RouterReady
					routerCond := current.Status.GetCondition(v1alpha2.RouterReady)
					g.Expect(routerCond).ToNot(BeNil())
					g.Expect(routerCond.IsTrue()).To(BeTrue(), "RouterReady should stay True - group issues do not block readiness")
				}).WithContext(ctx).Should(Succeed())
			}
		})
	})

	Context("Member deletion", func() {
		It("should update remaining member's HTTPRoute when a group member is deleted", func(ctx SpecContext) {
			// given
			groupName := "del-group"
			svcNameA := "test-del-a"
			svcNameB := "test-del-b"
			testNs := NewTestNamespace(ctx, envTest,
				WithIstioShadowService(svcNameA),
				WithIstioShadowService(svcNameB),
			)

			llmSvcA := LLMInferenceService(svcNameA,
				InNamespace[*v1alpha2.LLMInferenceService](testNs.Name),
				WithModelURI("hf://facebook/opt-125m"),
				WithModelName("facebook/opt-125m"),
				WithManagedRoute(),
				WithManagedGateway(),
				WithManagedScheduler(),
				WithGroup(groupName),
				WithWeight(60),
			)

			llmSvcB := LLMInferenceService(svcNameB,
				InNamespace[*v1alpha2.LLMInferenceService](testNs.Name),
				WithModelURI("hf://facebook/opt-125m"),
				WithModelName("facebook/opt-125m"),
				WithManagedRoute(),
				WithManagedGateway(),
				WithManagedScheduler(),
				WithGroup(groupName),
				WithWeight(40),
			)

			Expect(envTest.Create(ctx, llmSvcA)).To(Succeed())
			Expect(envTest.Create(ctx, llmSvcB)).To(Succeed())
			defer func() {
				testNs.DeleteAndWait(ctx, llmSvcA)
			}()

			ensureRouterManagedResourcesAreReady(ctx, envTest.Client, llmSvcA)
			ensureRouterManagedResourcesAreReady(ctx, envTest.Client, llmSvcB)

			// Wait for group to be formed with both members
			Eventually(func(g Gomega, ctx context.Context) {
				routes, err := managedRoutes(ctx, llmSvcA)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(routes).To(HaveLen(1))
				g.Expect(firstRuleBackendRefs(&routes[0])).To(HaveLen(2))
			}).WithContext(ctx).Should(Succeed())

			// when - delete member B
			testNs.DeleteAndWait(ctx, llmSvcB)

			// then - remaining member A should have only its own backendRef
			Eventually(func(g Gomega, ctx context.Context) {
				routes, err := managedRoutes(ctx, llmSvcA)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(routes).To(HaveLen(1))

				backendRefs := firstRuleBackendRefs(&routes[0])
				g.Expect(backendRefs).To(HaveLen(1), "should have only one backendRef after member deletion")
				g.Expect(backendRefs[0]).To(HaveBackendName(svcNameA))
			}).WithContext(ctx).Should(Succeed())

			// then - group status should show only one member
			Eventually(func(g Gomega, ctx context.Context) {
				currentA := &v1alpha2.LLMInferenceService{}
				g.Expect(envTest.Get(ctx, client.ObjectKeyFromObject(llmSvcA), currentA)).To(Succeed())
				g.Expect(currentA.Status.Router).ToNot(BeNil())
				g.Expect(currentA.Status.Router.Group).ToNot(BeNil())
				g.Expect(currentA.Status.Router.Group.Members).To(HaveLen(1))
				g.Expect(currentA.Status.Router.Group.Members[0].Name).To(Equal(svcNameA))
			}).WithContext(ctx).Should(Succeed())
		})
	})

	Context("Leave group", func() {
		It("should remove routing-group label and update peer when a member leaves the group", func(ctx SpecContext) {
			// given
			groupName := "leave-group"
			svcNameA := "test-leave-a"
			svcNameB := "test-leave-b"
			testNs := NewTestNamespace(ctx, envTest,
				WithIstioShadowService(svcNameA),
				WithIstioShadowService(svcNameB),
			)

			llmSvcA := LLMInferenceService(svcNameA,
				InNamespace[*v1alpha2.LLMInferenceService](testNs.Name),
				WithModelURI("hf://facebook/opt-125m"),
				WithModelName("facebook/opt-125m"),
				WithManagedRoute(),
				WithManagedGateway(),
				WithManagedScheduler(),
				WithGroup(groupName),
				WithWeight(70),
			)

			llmSvcB := LLMInferenceService(svcNameB,
				InNamespace[*v1alpha2.LLMInferenceService](testNs.Name),
				WithModelURI("hf://facebook/opt-125m"),
				WithModelName("facebook/opt-125m"),
				WithManagedRoute(),
				WithManagedGateway(),
				WithManagedScheduler(),
				WithGroup(groupName),
				WithWeight(30),
			)

			Expect(envTest.Create(ctx, llmSvcA)).To(Succeed())
			Expect(envTest.Create(ctx, llmSvcB)).To(Succeed())
			defer func() {
				testNs.DeleteAndWait(ctx, llmSvcA)
				testNs.DeleteAndWait(ctx, llmSvcB)
			}()

			ensureRouterManagedResourcesAreReady(ctx, envTest.Client, llmSvcA)
			ensureRouterManagedResourcesAreReady(ctx, envTest.Client, llmSvcB)

			// Wait for group to be formed
			Eventually(func(g Gomega, ctx context.Context) {
				routes, err := managedRoutes(ctx, llmSvcA)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(routes).To(HaveLen(1))
				g.Expect(firstRuleBackendRefs(&routes[0])).To(HaveLen(2))
			}).WithContext(ctx).Should(Succeed())

			// when - remove group/weight from member B (leaving the group)
			// Also remove the routing-group label since the defaulting webhook
			// is not installed in envtest (in a real cluster the webhook does this).
			errRetry := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				_, errUpdate := ctrl.CreateOrUpdate(ctx, envTest.Client, llmSvcB, func() error {
					llmSvcB.Spec.Router.Route.Group = nil
					llmSvcB.Spec.Router.Route.Weight = nil
					delete(llmSvcB.Labels, constants.LLMRoutingGroupLabelKey)
					return nil
				})
				return errUpdate
			})
			Expect(errRetry).ToNot(HaveOccurred())

			// then - remaining member A should have only its own backendRef
			Eventually(func(g Gomega, ctx context.Context) {
				routes, err := managedRoutes(ctx, llmSvcA)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(routes).To(HaveLen(1))

				backendRefs := firstRuleBackendRefs(&routes[0])
				g.Expect(backendRefs).To(HaveLen(1))
				g.Expect(backendRefs[0]).To(HaveBackendName(svcNameA))
			}).WithContext(ctx).Should(Succeed())

			// then - remaining member A's group status should show only itself
			Eventually(func(g Gomega, ctx context.Context) {
				currentA := &v1alpha2.LLMInferenceService{}
				g.Expect(envTest.Get(ctx, client.ObjectKeyFromObject(llmSvcA), currentA)).To(Succeed())
				g.Expect(currentA.Status.Router).ToNot(BeNil())
				g.Expect(currentA.Status.Router.Group).ToNot(BeNil())
				g.Expect(currentA.Status.Router.Group.Members).To(HaveLen(1))
			}).WithContext(ctx).Should(Succeed())
		})
	})

	Context("Force-stop grouped member", func() {
		It("should set weight to 0 for a force-stopped member while preserving group status", func(ctx SpecContext) {
			// given
			groupName := "stop-group"
			svcNameA := "test-stop-grp-a"
			svcNameB := "test-stop-grp-b"
			testNs := NewTestNamespace(ctx, envTest,
				WithIstioShadowService(svcNameA),
				WithIstioShadowService(svcNameB),
			)

			llmSvcA := LLMInferenceService(svcNameA,
				InNamespace[*v1alpha2.LLMInferenceService](testNs.Name),
				WithModelURI("hf://facebook/opt-125m"),
				WithModelName("facebook/opt-125m"),
				WithManagedRoute(),
				WithManagedGateway(),
				WithManagedScheduler(),
				WithGroup(groupName),
				WithWeight(60),
			)

			llmSvcB := LLMInferenceService(svcNameB,
				InNamespace[*v1alpha2.LLMInferenceService](testNs.Name),
				WithModelURI("hf://facebook/opt-125m"),
				WithModelName("facebook/opt-125m"),
				WithManagedRoute(),
				WithManagedGateway(),
				WithManagedScheduler(),
				WithGroup(groupName),
				WithWeight(40),
			)

			Expect(envTest.Create(ctx, llmSvcA)).To(Succeed())
			Expect(envTest.Create(ctx, llmSvcB)).To(Succeed())
			defer func() {
				testNs.DeleteAndWait(ctx, llmSvcA)
				testNs.DeleteAndWait(ctx, llmSvcB)
			}()

			ensureRouterManagedResourcesAreReady(ctx, envTest.Client, llmSvcA)
			ensureRouterManagedResourcesAreReady(ctx, envTest.Client, llmSvcB)

			// Wait for group to be formed
			Eventually(func(g Gomega, ctx context.Context) {
				routes, err := managedRoutes(ctx, llmSvcA)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(routes).To(HaveLen(1))
				g.Expect(firstRuleBackendRefs(&routes[0])).To(HaveLen(2))
			}).WithContext(ctx).Should(Succeed())

			// when - force-stop member A
			errRetry := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				_, errUpdate := ctrl.CreateOrUpdate(ctx, envTest.Client, llmSvcA, func() error {
					if llmSvcA.Annotations == nil {
						llmSvcA.Annotations = make(map[string]string)
					}
					llmSvcA.Annotations[constants.StopAnnotationKey] = "true"
					return nil
				})
				return errUpdate
			})
			Expect(errRetry).ToNot(HaveOccurred())

			// then - on the remaining member B's HTTPRoute, member A should have weight 0
			Eventually(func(g Gomega, ctx context.Context) {
				routes, err := managedRoutes(ctx, llmSvcB)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(routes).To(HaveLen(1))

				backendRefs := firstRuleBackendRefs(&routes[0])
				g.Expect(backendRefs).To(HaveLen(2), "stopped member should still appear in backendRefs")

				g.Expect(backendRefs).To(ContainElement(
					SatisfyAll(
						HaveBackendName(svcNameA),
						HaveBackendWeight(int32(0)),
					),
				))
				g.Expect(backendRefs).To(ContainElement(
					SatisfyAll(
						HaveBackendName(svcNameB),
						HaveBackendWeight(int32(40)),
					),
				))
			}).WithContext(ctx).Should(Succeed())

			// then - group status on member B should show member A with weight 0
			Eventually(func(g Gomega, ctx context.Context) {
				currentB := &v1alpha2.LLMInferenceService{}
				g.Expect(envTest.Get(ctx, client.ObjectKeyFromObject(llmSvcB), currentB)).To(Succeed())
				g.Expect(currentB.Status.Router).ToNot(BeNil())
				g.Expect(currentB.Status.Router.Group).ToNot(BeNil(), "group status should be preserved")
				g.Expect(currentB.Status.Router.Group.Name).To(Equal(groupName))
				g.Expect(currentB.Status.Router.Group.Members).To(HaveLen(2))

				for _, member := range currentB.Status.Router.Group.Members {
					if member.Name == svcNameA {
						g.Expect(member.Weight).To(Equal(int32(0)), "stopped member weight should be 0 in group status")
					}
					if member.Name == svcNameB {
						g.Expect(member.Weight).To(Equal(int32(40)), "non-stopped member weight should be preserved")
					}
				}
			}).WithContext(ctx).Should(Succeed())
		})
	})
})

// firstRuleBackendRefs returns backendRefs from the first rule in an HTTPRoute.
// With group routing, all rules share the same set of weighted backendRefs
// (rewriteRulesForGroup replaces backendRefs on every rule), so inspecting
// the first rule is sufficient to verify group membership and weights.
func firstRuleBackendRefs(route *gwapiv1.HTTPRoute) []gwapiv1.HTTPBackendRef {
	if len(route.Spec.Rules) == 0 {
		return nil
	}
	return route.Spec.Rules[0].BackendRefs
}

// backendRefNames extracts the backend names from a slice of HTTPBackendRef.
func backendRefNames(refs []gwapiv1.HTTPBackendRef) []string {
	names := make([]string, len(refs))
	for i, ref := range refs {
		names[i] = string(ref.Name)
	}
	return names
}

// HaveBackendName matches an HTTPBackendRef whose Name contains the given substring.
// Backend names include suffixes like "-inference-pool" or "-kserve-workload-svc",
// so we match on the service name prefix.
func HaveBackendName(nameSubstring string) OmegaMatcher {
	return WithTransform(func(ref gwapiv1.HTTPBackendRef) string {
		return string(ref.Name)
	}, ContainSubstring(nameSubstring))
}

// HaveBackendWeight matches an HTTPBackendRef with the given weight.
func HaveBackendWeight(weight int32) OmegaMatcher {
	return WithTransform(func(ref gwapiv1.HTTPBackendRef) *int32 {
		return ref.Weight
	}, Equal(ptr.To(weight)))
}
