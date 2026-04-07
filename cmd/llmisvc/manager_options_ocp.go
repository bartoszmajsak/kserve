//go:build distro

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

package main

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"

	"github.com/kserve/kserve/pkg/controller/v1alpha2/llmisvc"
)

func customizeManagerOptions(opts *ctrl.Options) error {
	// Replace the simple label-based Secret cache with a namespace-aware one that
	// also watches the platform CA signing secret used for workload TLS certificates.
	for obj := range opts.Cache.ByObject {
		if _, ok := obj.(*corev1.Secret); ok {
			bo := opts.Cache.ByObject[obj]

			// Shallow-copy the Namespaces map so we don't mutate the original,
			// then add the CA signing secret namespace watch while preserving
			// any other per-namespace settings and top-level ByObject fields
			// (Transform, UnsafeDisableDeepCopy, EnableWatchBookmarks, etc.).
			ns := make(map[string]cache.Config, len(bo.Namespaces)+2)
			for k, v := range bo.Namespaces {
				ns[k] = v
			}
			ns[llmisvc.ServiceCASigningSecretNamespace] = cache.Config{
				FieldSelector: fields.SelectorFromSet(map[string]string{
					"metadata.name": llmisvc.ServiceCASigningSecretName,
				}),
			}
			ns[cache.AllNamespaces] = cache.Config{
				LabelSelector: bo.Label,
			}
			bo.Label = nil // now applied via AllNamespaces in the Namespaces map
			bo.Namespaces = ns
			opts.Cache.ByObject[obj] = bo
			return nil
		}
	}

	return fmt.Errorf("customizeManagerOptions: *corev1.Secret not found in opts.Cache.ByObject; " +
		"this is a programming error - ensure main.go registers a *corev1.Secret cache entry before calling this function")
}
