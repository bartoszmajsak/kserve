/*
Copyright 2025 The KServe Authors.

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
	"reflect"
	"testing"

	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"knative.dev/pkg/apis"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayapi "sigs.k8s.io/gateway-api/apis/v1"

	. "github.com/kserve/kserve/pkg/controller/llmisvc/fixture"

	"github.com/kserve/kserve/pkg/controller/llmisvc"
)

type discoverURLsTestCase struct {
	scenario           string
	name               string
	route              *gatewayapi.HTTPRoute
	gateway            *gatewayapi.Gateway
	additionalGateways []*gatewayapi.Gateway
	expectedURLs       []string
	expectedErrorCheck func(error) bool
}

func TestDiscoverURLs(t *testing.T) {
	tests := []discoverURLsTestCase{
		{
			scenario: "address",
			name:     "basic external IP",
			route: HTTPRoute("test-route",
				InNamespace[*gatewayapi.HTTPRoute]("test-ns"),
				WithParentRef(GatewayRef("test-gateway", RefInNamespace("test-ns"))),
			),
			gateway:      HTTPGateway("test-gateway", "test-ns", "203.0.113.1"),
			expectedURLs: []string{"http://203.0.113.1/"},
		},
		{
			scenario: "address",
			name:     "ordering consistency - same addresses different order",
			route: HTTPRoute("consistency-route",
				InNamespace[*gatewayapi.HTTPRoute]("test-ns"),
				WithParentRef(GatewayRef("consistency-gateway", RefInNamespace("test-ns"))),
			),
			gateway: Gateway("consistency-gateway",
				InNamespace[*gatewayapi.Gateway]("test-ns"),
				WithListener(gatewayapi.HTTPProtocolType),
				WithAddresses([]string{"203.0.113.200", "203.0.113.100"}...),
			),
			expectedURLs: []string{
				"http://203.0.113.100/",
				"http://203.0.113.200/",
			},
		},
		{
			scenario: "address",
			name:     "mixed internal and external - deterministic selection",
			route: HTTPRoute("mixed-route",
				InNamespace[*gatewayapi.HTTPRoute]("test-ns"),
				WithParentRef(GatewayRef("mixed-gateway", RefInNamespace("test-ns"))),
			),
			gateway: Gateway("mixed-gateway",
				InNamespace[*gatewayapi.Gateway]("test-ns"),
				WithListener(gatewayapi.HTTPProtocolType),
				WithAddresses("192.168.1.10", "203.0.113.50", "10.0.0.20", "203.0.113.25"),
			),
			expectedURLs: []string{
				"http://10.0.0.20/",
				"http://192.168.1.10/",
				"http://203.0.113.25/",
				"http://203.0.113.50/",
			},
		},
		{
			scenario: "address",
			name:     "hostname addresses - basic resolution",
			route: HTTPRoute("hostname-route",
				InNamespace[*gatewayapi.HTTPRoute]("test-ns"),
				WithParentRef(GatewayRef("hostname-gateway", RefInNamespace("test-ns"))),
			),
			gateway: Gateway("hostname-gateway",
				InNamespace[*gatewayapi.Gateway]("test-ns"),
				WithListener(gatewayapi.HTTPProtocolType),
				WithHostnameAddresses("api.example.com"),
			),
			expectedURLs: []string{"http://api.example.com/"},
		},
		{
			scenario: "address",
			name:     "mixed hostname and IP addresses - deterministic selection",
			route: HTTPRoute("mixed-types-route",
				InNamespace[*gatewayapi.HTTPRoute]("test-ns"),
				WithParentRef(GatewayRef("mixed-types-gateway", RefInNamespace("test-ns"))),
			),
			gateway: Gateway("mixed-types-gateway",
				InNamespace[*gatewayapi.Gateway]("test-ns"),
				WithListener(gatewayapi.HTTPProtocolType),
				WithMixedAddresses(
					HostnameAddress("z.example.com"),
					IPAddress("203.0.113.1"),
					HostnameAddress("api.example.com"),
					IPAddress("198.51.100.1"),
				),
			),
			expectedURLs: []string{
				"http://198.51.100.1/",
				"http://203.0.113.1/",
				"http://api.example.com/",
				"http://z.example.com/",
			},
		},
		{
			scenario: "address",
			name:     "hostname addresses with internal hostnames filtered",
			route: HTTPRoute("internal-hostname-route",
				InNamespace[*gatewayapi.HTTPRoute]("test-ns"),
				WithParentRef(GatewayRef("internal-hostname-gateway", RefInNamespace("test-ns"))),
			),
			gateway: Gateway("internal-hostname-gateway",
				InNamespace[*gatewayapi.Gateway]("test-ns"),
				WithListener(gatewayapi.HTTPProtocolType),
				WithMixedAddresses(
					HostnameAddress("localhost"),
					HostnameAddress("service.local"),
					HostnameAddress("app.internal"),
					HostnameAddress("api.example.com"),
					HostnameAddress("backup.example.com"),
				),
			),
			expectedURLs: []string{
				"http://api.example.com/",
				"http://app.internal/",
				"http://backup.example.com/",
				"http://localhost/",
				"http://service.local/",
			},
		},
		{
			scenario: "address",
			name:     "only internal addresses (IP + hostnames)",
			route: HTTPRoute("only-internal-route",
				InNamespace[*gatewayapi.HTTPRoute]("test-ns"),
				WithParentRef(GatewayRef("only-internal-gateway", RefInNamespace("test-ns"))),
			),
			gateway: Gateway("only-internal-gateway",
				InNamespace[*gatewayapi.Gateway]("test-ns"),
				WithListener(gatewayapi.HTTPProtocolType),
				WithMixedAddresses(
					IPAddress("192.168.1.10"),
					IPAddress("10.0.0.20"),
					HostnameAddress("localhost"),
					HostnameAddress("app.local"),
				),
			),
			expectedURLs: []string{
				"http://10.0.0.20/",
				"http://192.168.1.10/",
				"http://app.local/",
				"http://localhost/",
			},
		},
		{
			scenario: "address",
			name:     "backwards compatibility - nil Type defaults to IP behavior",
			route: HTTPRoute("nil-type-route",
				InNamespace[*gatewayapi.HTTPRoute]("test-ns"),
				WithParentRef(GatewayRef("nil-type-gateway", RefInNamespace("test-ns"))),
			),
			gateway: Gateway("nil-type-gateway",
				InNamespace[*gatewayapi.Gateway]("test-ns"),
				WithListener(gatewayapi.HTTPProtocolType),
				WithAddresses("203.0.113.1", "192.168.1.10"),
			),
			expectedURLs: []string{
				"http://192.168.1.10/",
				"http://203.0.113.1/",
			},
		},
		{
			scenario: "hostname",
			name:     "route hostname overrides gateway address",
			route: HTTPRoute("hostname-route",
				InNamespace[*gatewayapi.HTTPRoute]("test-ns"),
				WithParentRef(GatewayRef("hostname-gateway", RefInNamespace("test-ns"))),
				WithHostnames("api.example.com"),
			),
			gateway: Gateway("hostname-gateway",
				InNamespace[*gatewayapi.Gateway]("test-ns"),
				WithListener(gatewayapi.HTTPProtocolType),
				WithAddresses([]string{"203.0.113.1"}...),
			),
			expectedURLs: []string{"http://api.example.com/"},
		},
		{
			scenario: "hostname",
			name:     "route wildcard hostname - use gateway address",
			route: HTTPRoute("wildcard-route",
				InNamespace[*gatewayapi.HTTPRoute]("test-ns"),
				WithParentRef(GatewayRef("wildcard-gateway", RefInNamespace("test-ns"))),
				WithHostnames("*"),
			),
			gateway: Gateway("wildcard-gateway",
				InNamespace[*gatewayapi.Gateway]("test-ns"),
				WithListener(gatewayapi.HTTPProtocolType),
				WithAddresses("203.0.113.100"),
			),
			expectedURLs: []string{"http://203.0.113.100/"},
		},
		{
			scenario: "hostname",
			name:     "multiple hostnames - generates multiple URLs",
			route: HTTPRoute("multi-hostname-route",
				InNamespace[*gatewayapi.HTTPRoute]("test-ns"),
				WithParentRef(GatewayRef("multi-hostname-gateway", RefInNamespace("test-ns"))),
				WithHostnames("*", "", "api.example.com", "alt.example.com"),
			),
			gateway: Gateway("multi-hostname-gateway",
				InNamespace[*gatewayapi.Gateway]("test-ns"),
				WithListener(gatewayapi.HTTPProtocolType),
				WithAddresses("203.0.113.1"),
			),
			expectedURLs: []string{
				"http://alt.example.com/",
				"http://api.example.com/",
			},
		},
		{
			scenario: "hostname",
			name:     "multiple hostnames and addresses - comprehensive URL generation",
			route: HTTPRoute("comprehensive-route",
				InNamespace[*gatewayapi.HTTPRoute]("test-ns"),
				WithParentRef(GatewayRef("comprehensive-gateway", RefInNamespace("test-ns"))),
				WithHostnames("api.example.com", "backup.example.com", "primary.example.com"),
			),
			gateway: Gateway("comprehensive-gateway",
				InNamespace[*gatewayapi.Gateway]("test-ns"),
				WithListener(gatewayapi.HTTPProtocolType),
				WithAddresses("203.0.113.1", "198.51.100.1"),
			),
			expectedURLs: []string{
				"http://api.example.com/",
				"http://backup.example.com/",
				"http://primary.example.com/",
			},
		},
		{
			scenario: "hostname",
			name:     "listener hostname fallback - no route hostnames",
			route: HTTPRoute("listener-hostname-route",
				InNamespace[*gatewayapi.HTTPRoute]("test-ns"),
				WithParentRef(GatewayRef("listener-hostname-gateway", RefInNamespace("test-ns"))),
			),
			gateway: Gateway("listener-hostname-gateway",
				InNamespace[*gatewayapi.Gateway]("test-ns"),
				WithListeners(gatewayapi.Listener{
					Protocol: gatewayapi.HTTPProtocolType,
					Port:     80,
					Hostname: ptr.To(gatewayapi.Hostname("listener.example.com")),
				}),
				WithAddresses("203.0.113.1"),
			),
			expectedURLs: []string{"http://listener.example.com/"},
		},
		{
			scenario: "hostname",
			name:     "listener hostname fallback - route has wildcard hostname",
			route: HTTPRoute("listener-hostname-wildcard-route",
				InNamespace[*gatewayapi.HTTPRoute]("test-ns"),
				WithParentRef(GatewayRef("listener-hostname-wildcard-gateway", RefInNamespace("test-ns"))),
				WithHostnames("*"),
			),
			gateway: Gateway("listener-hostname-wildcard-gateway",
				InNamespace[*gatewayapi.Gateway]("test-ns"),
				WithListeners(gatewayapi.Listener{
					Protocol: gatewayapi.HTTPProtocolType,
					Port:     80,
					Hostname: ptr.To(gatewayapi.Hostname("fallback.example.com")),
				}),
				WithAddresses("203.0.113.1"),
			),
			expectedURLs: []string{"http://fallback.example.com/"},
		},
		{
			scenario: "hostname",
			name:     "listener hostname fallback - route hostname takes precedence",
			route: HTTPRoute("route-hostname-precedence",
				InNamespace[*gatewayapi.HTTPRoute]("test-ns"),
				WithParentRef(GatewayRef("route-hostname-precedence-gateway", RefInNamespace("test-ns"))),
				WithHostnames("route.example.com"),
			),
			gateway: Gateway("route-hostname-precedence-gateway",
				InNamespace[*gatewayapi.Gateway]("test-ns"),
				WithListeners(gatewayapi.Listener{
					Protocol: gatewayapi.HTTPProtocolType,
					Port:     80,
					Hostname: ptr.To(gatewayapi.Hostname("listener.example.com")),
				}),
				WithAddresses("203.0.113.1"),
			),
			expectedURLs: []string{"http://route.example.com/"},
		},
		{
			scenario: "hostname",
			name:     "listener hostname fallback - empty listener hostname uses addresses",
			route: HTTPRoute("empty-listener-hostname-route",
				InNamespace[*gatewayapi.HTTPRoute]("test-ns"),
				WithParentRef(GatewayRef("empty-listener-hostname-gateway", RefInNamespace("test-ns"))),
			),
			gateway: Gateway("empty-listener-hostname-gateway",
				InNamespace[*gatewayapi.Gateway]("test-ns"),
				WithListeners(gatewayapi.Listener{
					Protocol: gatewayapi.HTTPProtocolType,
					Port:     80,
					Hostname: ptr.To(gatewayapi.Hostname("")),
				}),
				WithAddresses("203.0.113.1"),
			),
			expectedURLs: []string{"http://203.0.113.1/"},
		},
		{
			scenario: "hostname",
			name:     "listener wildcard hostname - basic wildcard expansion",
			route: HTTPRoute("wildcard-listener-route",
				InNamespace[*gatewayapi.HTTPRoute]("test-ns"),
				WithParentRef(GatewayRef("wildcard-listener-gateway", RefInNamespace("test-ns"))),
			),
			gateway: Gateway("wildcard-listener-gateway",
				InNamespace[*gatewayapi.Gateway]("test-ns"),
				WithListeners(gatewayapi.Listener{
					Protocol: gatewayapi.HTTPProtocolType,
					Port:     80,
					Hostname: ptr.To(gatewayapi.Hostname("*.example.com")),
				}),
				WithAddresses("203.0.113.1"),
			),
			expectedURLs: []string{"http://inference.example.com/"},
		},
		{
			scenario: "hostname",
			name:     "listener wildcard hostname - wildcard with subdomain",
			route: HTTPRoute("wildcard-subdomain-route",
				InNamespace[*gatewayapi.HTTPRoute]("test-ns"),
				WithParentRef(GatewayRef("wildcard-subdomain-gateway", RefInNamespace("test-ns"))),
			),
			gateway: Gateway("wildcard-subdomain-gateway",
				InNamespace[*gatewayapi.Gateway]("test-ns"),
				WithListeners(gatewayapi.Listener{
					Protocol: gatewayapi.HTTPProtocolType,
					Port:     80,
					Hostname: ptr.To(gatewayapi.Hostname("*.api.example.com")),
				}),
				WithAddresses("203.0.113.1"),
			),
			expectedURLs: []string{"http://inference.api.example.com/"},
		},
		{
			scenario: "hostname",
			name:     "listener wildcard hostname - route hostname takes precedence over wildcard",
			route: HTTPRoute("route-over-wildcard-route",
				InNamespace[*gatewayapi.HTTPRoute]("test-ns"),
				WithParentRef(GatewayRef("route-over-wildcard-gateway", RefInNamespace("test-ns"))),
				WithHostnames("custom.example.com"),
			),
			gateway: Gateway("route-over-wildcard-gateway",
				InNamespace[*gatewayapi.Gateway]("test-ns"),
				WithListeners(gatewayapi.Listener{
					Protocol: gatewayapi.HTTPProtocolType,
					Port:     80,
					Hostname: ptr.To(gatewayapi.Hostname("*.example.com")),
				}),
				WithAddresses("203.0.113.1"),
			),
			expectedURLs: []string{"http://custom.example.com/"},
		},
		{
			scenario: "hostname",
			name:     "listener wildcard hostname - HTTPS with wildcard",
			route: HTTPRoute("https-wildcard-route",
				InNamespace[*gatewayapi.HTTPRoute]("test-ns"),
				WithParentRef(GatewayRef("https-wildcard-gateway", RefInNamespace("test-ns"))),
			),
			gateway: Gateway("https-wildcard-gateway",
				InNamespace[*gatewayapi.Gateway]("test-ns"),
				WithListeners(gatewayapi.Listener{
					Protocol: gatewayapi.HTTPSProtocolType,
					Port:     443,
					Hostname: ptr.To(gatewayapi.Hostname("*.secure.example.com")),
				}),
				WithAddresses("203.0.113.1"),
			),
			expectedURLs: []string{"https://inference.secure.example.com/"},
		},
		{
			scenario: "hostname",
			name:     "listener wildcard hostname - custom port with wildcard",
			route: HTTPRoute("custom-port-wildcard-route",
				InNamespace[*gatewayapi.HTTPRoute]("test-ns"),
				WithParentRef(GatewayRef("custom-port-wildcard-gateway", RefInNamespace("test-ns"))),
			),
			gateway: Gateway("custom-port-wildcard-gateway",
				InNamespace[*gatewayapi.Gateway]("test-ns"),
				WithListeners(gatewayapi.Listener{
					Protocol: gatewayapi.HTTPProtocolType,
					Port:     8080,
					Hostname: ptr.To(gatewayapi.Hostname("*.apps.example.com")),
				}),
				WithAddresses("203.0.113.1"),
			),
			expectedURLs: []string{"http://inference.apps.example.com:8080/"},
		},
		{
			scenario: "path",
			name:     "custom path extraction",
			route: HTTPRoute("path-route",
				InNamespace[*gatewayapi.HTTPRoute]("test-ns"),
				WithParentRef(GatewayRef("path-gateway", RefInNamespace("test-ns"))),
				WithPath("/api/v1/models"),
			),
			gateway: Gateway("path-gateway",
				InNamespace[*gatewayapi.Gateway]("test-ns"),
				WithListener(gatewayapi.HTTPProtocolType),
				WithAddresses("203.0.113.1"),
			),
			expectedURLs: []string{"http://203.0.113.1/api/v1/models"},
		},
		{
			scenario: "path",
			name:     "empty route rules - default path",
			route: HTTPRoute("empty-rules-route",
				InNamespace[*gatewayapi.HTTPRoute]("test-ns"),
				WithParentRef(GatewayRef("empty-rules-gateway", RefInNamespace("test-ns"))),
				WithRules(),
			),
			gateway: Gateway("empty-rules-gateway",
				InNamespace[*gatewayapi.Gateway]("test-ns"),
				WithListener(gatewayapi.HTTPProtocolType),
				WithAddresses("203.0.113.1"),
			),
			expectedURLs: []string{"http://203.0.113.1/"},
		},
		{
			scenario: "port",
			name:     "HTTPS scheme from gateway listener",
			route: HTTPRoute("https-route",
				InNamespace[*gatewayapi.HTTPRoute]("test-ns"),
				WithParentRef(GatewayRef("https-gateway", RefInNamespace("test-ns"))),
			),
			gateway:      HTTPSGateway("https-gateway", "test-ns", "203.0.113.1"),
			expectedURLs: []string{"https://203.0.113.1/"},
		},
		{
			scenario: "port",
			name:     "non-standard HTTP port included in URL",
			route: HTTPRoute("custom-port-route",
				InNamespace[*gatewayapi.HTTPRoute]("test-ns"),
				WithParentRef(GatewayRef("custom-port-gateway", RefInNamespace("test-ns"))),
			),
			gateway: Gateway("custom-port-gateway",
				InNamespace[*gatewayapi.Gateway]("test-ns"),
				WithListeners(gatewayapi.Listener{
					Protocol: gatewayapi.HTTPProtocolType,
					Port:     8080,
				}),
				WithAddresses("203.0.113.1"),
			),
			expectedURLs: []string{"http://203.0.113.1:8080/"},
		},
		{
			scenario: "port",
			name:     "non-standard HTTPS port included in URL",
			route: HTTPRoute("custom-https-port-route",
				InNamespace[*gatewayapi.HTTPRoute]("test-ns"),
				WithParentRef(GatewayRef("custom-https-port-gateway", RefInNamespace("test-ns"))),
				WithHostnames("secure.example.com"),
			),
			gateway: Gateway("custom-https-port-gateway",
				InNamespace[*gatewayapi.Gateway]("test-ns"),
				WithListeners(gatewayapi.Listener{
					Protocol: gatewayapi.HTTPSProtocolType,
					Port:     8443,
				}),
				WithAddresses("203.0.113.1"),
			),
			expectedURLs: []string{"https://secure.example.com:8443/"},
		},
		{
			scenario: "port",
			name:     "standard HTTP port 80 omitted from URL",
			route: HTTPRoute("standard-http-route",
				InNamespace[*gatewayapi.HTTPRoute]("test-ns"),
				WithParentRef(GatewayRef("standard-http-gateway", RefInNamespace("test-ns"))),
			),
			gateway: Gateway("standard-http-gateway",
				InNamespace[*gatewayapi.Gateway]("test-ns"),
				WithListeners(gatewayapi.Listener{
					Protocol: gatewayapi.HTTPProtocolType,
					Port:     80,
				}),
				WithAddresses("203.0.113.1"),
			),
			expectedURLs: []string{"http://203.0.113.1/"},
		},
		{
			scenario: "port",
			name:     "standard HTTPS port 443 omitted from URL",
			route: HTTPRoute("standard-https-route",
				InNamespace[*gatewayapi.HTTPRoute]("test-ns"),
				WithParentRef(GatewayRef("standard-https-gateway", RefInNamespace("test-ns"))),
				WithHostnames("secure.example.com"),
			),
			gateway: Gateway("standard-https-gateway",
				InNamespace[*gatewayapi.Gateway]("test-ns"),
				WithListeners(gatewayapi.Listener{
					Protocol: gatewayapi.HTTPSProtocolType,
					Port:     443,
				}),
				WithAddresses("203.0.113.1"),
			),
			expectedURLs: []string{"https://secure.example.com/"},
		},
		{
			scenario: "port",
			name:     "sectionName selects specific listener",
			route: HTTPRoute("section-route",
				InNamespace[*gatewayapi.HTTPRoute]("test-ns"),
				WithParentRef(gatewayapi.ParentReference{
					Name:        "multi-listener-gateway",
					Namespace:   ptr.To(gatewayapi.Namespace("test-ns")),
					SectionName: ptr.To(gatewayapi.SectionName("https-listener")),
					Group:       ptr.To(gatewayapi.Group("gateway.networking.k8s.io")),
					Kind:        ptr.To(gatewayapi.Kind("Gateway")),
				}),
				WithHostnames("secure.example.com"),
			),
			gateway: Gateway("multi-listener-gateway",
				InNamespace[*gatewayapi.Gateway]("test-ns"),
				WithListeners(
					gatewayapi.Listener{
						Name:     "http-listener",
						Protocol: gatewayapi.HTTPProtocolType,
						Port:     80,
					},
					gatewayapi.Listener{
						Name:     "https-listener",
						Protocol: gatewayapi.HTTPSProtocolType,
						Port:     443,
					},
				),
				WithAddresses("203.0.113.1"),
			),
			expectedURLs: []string{"https://secure.example.com/"},
		},
		{
			scenario: "parent-ref",
			name:     "multiple parent refs - sorted selection",
			route: HTTPRoute("multi-parent-route",
				InNamespace[*gatewayapi.HTTPRoute]("default-ns"),
				WithParentRefs(
					GatewayRef("z-gateway", RefInNamespace("z-namespace")),
					GatewayRef("a-gateway", RefInNamespace("a-namespace")),
					GatewayRef("b-gateway", RefInNamespace("a-namespace")),
				),
			),
			gateway: Gateway("a-gateway",
				InNamespace[*gatewayapi.Gateway]("a-namespace"),
				WithListener(gatewayapi.HTTPProtocolType),
				WithAddresses([]string{"203.0.113.1"}...),
			),
			additionalGateways: []*gatewayapi.Gateway{
				Gateway("z-gateway",
					InNamespace[*gatewayapi.Gateway]("z-namespace"),
					WithListener(gatewayapi.HTTPProtocolType),
					WithAddresses("203.0.113.2"),
				),
				Gateway("b-gateway",
					InNamespace[*gatewayapi.Gateway]("a-namespace"),
					WithListener(gatewayapi.HTTPProtocolType),
					WithAddresses("203.0.113.3"),
				),
			},
			expectedURLs: []string{
				"http://203.0.113.2/",
				"http://203.0.113.1/",
				"http://203.0.113.3/",
			},
		},
		{
			scenario: "parent-ref",
			name:     "without namespace - use route namespace",
			route: HTTPRoute("no-ns-route",
				InNamespace[*gatewayapi.HTTPRoute]("route-ns"),
				WithParentRef(GatewayRefWithoutNamespace("no-ns-gateway")),
			),
			gateway: Gateway("no-ns-gateway",
				InNamespace[*gatewayapi.Gateway]("route-ns"),
				WithListener(gatewayapi.HTTPProtocolType),
				WithAddresses("203.0.113.1"),
			),
			expectedURLs: []string{"http://203.0.113.1/"},
		},
		{
			scenario: "error",
			name:     "gateway not found",
			route: HTTPRoute("missing-gw-route",
				InNamespace[*gatewayapi.HTTPRoute]("test-ns"),
				WithParentRef(GatewayRef("missing-gateway", RefInNamespace("test-ns"))),
			),
			expectedErrorCheck: apierrors.IsNotFound,
		},
		{
			scenario: "error",
			name:     "no external addresses - custom ExternalAddressNotFoundError",
			route: HTTPRoute("no-external-route",
				InNamespace[*gatewayapi.HTTPRoute]("test-ns"),
				WithParentRef(GatewayRef("no-external-addresses-gateway", RefInNamespace("test-ns"))),
			),
			gateway: Gateway("no-external-addresses-gateway",
				InNamespace[*gatewayapi.Gateway]("test-ns"),
				WithListener(gatewayapi.HTTPProtocolType),
				WithAddresses("192.168.1.10", "10.0.0.20"),
			),
			expectedURLs: []string{
				"http://10.0.0.20/",
				"http://192.168.1.10/",
			},
		},
		{
			scenario: "error",
			name:     "no addresses at all - ExternalAddressNotFoundError",
			route: HTTPRoute("no-addresses-route",
				InNamespace[*gatewayapi.HTTPRoute]("test-ns"),
				WithParentRef(GatewayRef("no-addresses-gateway", RefInNamespace("test-ns"))),
			),
			gateway: Gateway("no-addresses-gateway",
				InNamespace[*gatewayapi.Gateway]("test-ns"),
				WithListener(gatewayapi.HTTPProtocolType),
			),
			expectedErrorCheck: llmisvc.IsExternalAddressNotFound,
		},
	}

	for _, tt := range tests {
		testName := tt.scenario + "/" + tt.name
		t.Run(testName, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ctx := t.Context()

			scheme := runtime.NewScheme()
			err := gatewayapi.Install(scheme)
			g.Expect(err).ToNot(HaveOccurred())

			var objects []client.Object
			appendIfNotNil := func(o client.Object) {
				if o != nil && !reflect.ValueOf(o).IsNil() {
					objects = append(objects, o)
				}
			}
			appendIfNotNil(tt.gateway)
			appendIfNotNil(tt.route)
			for _, gw := range tt.additionalGateways {
				appendIfNotNil(gw)
			}
			objects = append(objects, DefaultGatewayClass())

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(objects...).
				Build()

			urls, err := llmisvc.DiscoverURLs(ctx, fakeClient, tt.route)

			if tt.expectedErrorCheck != nil {
				g.Expect(err).To(HaveOccurred())
				g.Expect(tt.expectedErrorCheck(err)).To(BeTrue(), "Error check function failed for error: %v", err)
			} else {
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(urls).To(HaveLen(len(tt.expectedURLs)))

				var actualURLs []string
				for _, url := range urls {
					actualURLs = append(actualURLs, url.String())
				}

				g.Expect(actualURLs).To(Equal(tt.expectedURLs))
			}
		})
	}
}

type urlClassificationTest struct {
	url        string
	isInternal bool
}

func TestFilterURLs(t *testing.T) {
	classifications := []urlClassificationTest{
		// External
		{"http://203.0.113.1/", false},
		{"http://api.example.com/", false},
		{"https://secure.example.com/", false},
		{"http://api.example.com:8080/", false},
		{"https://secure.example.com:8443/", false},
		{"http://api.example.com/api/v1/models", false},

		// Internal (private IPs)
		{"http://192.168.1.10/", true},
		{"http://10.0.0.20/", true},
		{"http://192.168.1.10:8080/", true},
		{"http://192.168.1.10/api/v1/models", true},

		// Internal (local hostnames)
		{"http://localhost/", true},
		{"http://service.local/", true},
		{"http://app.localhost/", true},
		{"http://backend.internal/", true},
		{"http://localhost:3000/", true},
		{"http://localhost:8080/health", true},
	}

	convertToURLs := func(urls []string) ([]*apis.URL, error) {
		var parsedURLs []*apis.URL
		for _, urlStr := range urls {
			url, err := apis.ParseURL(urlStr)
			if err != nil {
				return nil, err
			}
			parsedURLs = append(parsedURLs, url)
		}
		return parsedURLs, nil
	}

	t.Run("URL classification", func(t *testing.T) {
		for _, tt := range classifications {
			t.Run(tt.url, func(t *testing.T) {
				g := NewGomegaWithT(t)
				url, err := apis.ParseURL(tt.url)
				g.Expect(err).ToNot(HaveOccurred())

				g.Expect(llmisvc.IsInternalURL(url)).To(Equal(tt.isInternal), "IsInternalURL")
				g.Expect(llmisvc.IsExternalURL(url)).To(Equal(!tt.isInternal), "IsExternalURL")
			})
		}
	})

	t.Run("FilterInternalURLs", func(t *testing.T) {
		g := NewGomegaWithT(t)

		var allURLs []*apis.URL
		var expectedInternal []string
		for _, c := range classifications {
			url, err := apis.ParseURL(c.url)
			g.Expect(err).ToNot(HaveOccurred())
			allURLs = append(allURLs, url)
			if c.isInternal {
				expectedInternal = append(expectedInternal, c.url)
			}
		}

		result := llmisvc.FilterInternalURLs(allURLs)
		var resultStrings []string
		for _, u := range result {
			resultStrings = append(resultStrings, u.String())
		}
		g.Expect(resultStrings).To(Equal(expectedInternal))
	})

	t.Run("FilterExternalURLs", func(t *testing.T) {
		g := NewGomegaWithT(t)

		var allURLs []*apis.URL
		var expectedExternal []string
		for _, c := range classifications {
			url, err := apis.ParseURL(c.url)
			g.Expect(err).ToNot(HaveOccurred())
			allURLs = append(allURLs, url)
			if !c.isInternal {
				expectedExternal = append(expectedExternal, c.url)
			}
		}

		result := llmisvc.FilterExternalURLs(allURLs)
		var resultStrings []string
		for _, u := range result {
			resultStrings = append(resultStrings, u.String())
		}
		g.Expect(resultStrings).To(Equal(expectedExternal))
	})

	t.Run("empty input", func(t *testing.T) {
		g := NewGomegaWithT(t)
		g.Expect(llmisvc.FilterInternalURLs(nil)).To(BeEmpty())
		g.Expect(llmisvc.FilterExternalURLs(nil)).To(BeEmpty())

		emptySlice, err := convertToURLs([]string{})
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(llmisvc.FilterInternalURLs(emptySlice)).To(BeEmpty())
		g.Expect(llmisvc.FilterExternalURLs(emptySlice)).To(BeEmpty())
	})
}
