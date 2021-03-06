// Copyright 2017 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	k8ssecret "istio.io/istio/security/pkg/k8s/secret"
	mockca "istio.io/istio/security/pkg/pki/ca/mock"
	"istio.io/istio/security/pkg/pki/util"
	mockutil "istio.io/istio/security/pkg/pki/util/mock"
)

const (
	defaultTTL                = time.Hour
	defaultGracePeriodRatio   = 0.5
	defaultMinGracePeriod     = 10 * time.Minute
	sidecarInjectorSvcAccount = "istio-sidecar-injector-service-account"
	sidecarInjectorSvc        = "istio-sidecar-injector"
)

var (
	enableNamespacesByDefault = true

	caCert          = []byte("fake CA cert")
	caKey           = []byte("fake private key")
	certChain       = []byte("fake cert chain")
	rootCert        = []byte("fake root cert")
	signedCert      = []byte("fake signed cert")
	istioTestSecret = k8ssecret.BuildSecret("test", "istio.test", "test-ns", certChain, caKey, rootCert, nil, nil, IstioSecretType)
)

func TestSecretController(t *testing.T) {
	gvr := schema.GroupVersionResource{
		Resource: "secrets",
		Version:  "v1",
	}
	nsSchema := schema.GroupVersionResource{
		Resource: "namespaces",
		Version:  "v1",
	}
	testCases := map[string]struct {
		existingSecret   *v1.Secret
		saToAdd          *v1.ServiceAccount
		saToDelete       *v1.ServiceAccount
		expectedActions  []ktesting.Action
		gracePeriodRatio float32
		injectFailure    bool
		shouldFail       bool
	}{
		"invalid gracePeriodRatio": {
			saToAdd: createServiceAccount("test", "test-ns"),
			expectedActions: []ktesting.Action{
				ktesting.NewCreateAction(gvr, "test-ns", istioTestSecret),
			},
			gracePeriodRatio: 1.4,
			shouldFail:       true,
		},
		"adding service account creates new secret": {
			saToAdd: createServiceAccount("test", "test-ns"),
			expectedActions: []ktesting.Action{
				ktesting.NewGetAction(nsSchema, "test-ns", "test-ns"),
				ktesting.NewCreateAction(gvr, "test-ns", istioTestSecret),
			},
			gracePeriodRatio: defaultGracePeriodRatio,
			shouldFail:       false,
		},
		"removing service account deletes existing secret": {
			saToDelete: createServiceAccount("deleted", "deleted-ns"),
			expectedActions: []ktesting.Action{
				ktesting.NewDeleteAction(gvr, "deleted-ns", "istio.deleted"),
			},
			gracePeriodRatio: defaultGracePeriodRatio,
			shouldFail:       false,
		},
		"adding new service account does not overwrite existing secret": {
			existingSecret:   istioTestSecret,
			saToAdd:          createServiceAccount("test", "test-ns"),
			gracePeriodRatio: defaultGracePeriodRatio,
			expectedActions: []ktesting.Action{
				ktesting.NewGetAction(nsSchema, "test-ns", "test-ns"),
			},
			shouldFail: false,
		},
		"adding service account retries when failed": {
			saToAdd: createServiceAccount("test", "test-ns"),
			expectedActions: []ktesting.Action{
				ktesting.NewGetAction(nsSchema, "test-ns", "test-ns"),
				ktesting.NewCreateAction(gvr, "test-ns", istioTestSecret),
				ktesting.NewCreateAction(gvr, "test-ns", istioTestSecret),
				ktesting.NewCreateAction(gvr, "test-ns", istioTestSecret),
			},
			gracePeriodRatio: defaultGracePeriodRatio,
			injectFailure:    true,
			shouldFail:       false,
		},
		"adding webhook service account": {
			saToAdd: createServiceAccount(sidecarInjectorSvcAccount, "test-ns"),
			expectedActions: []ktesting.Action{
				ktesting.NewGetAction(nsSchema, "test-ns", "test-ns"),
				ktesting.NewCreateAction(gvr, "test-ns",
					k8ssecret.BuildSecret("test", sidecarInjectorSvcAccount, "test-ns", certChain, caKey, rootCert, nil, nil, IstioSecretType)),
			},
			gracePeriodRatio: defaultGracePeriodRatio,
			shouldFail:       false,
		},
	}

	for k, tc := range testCases {
		client := fake.NewSimpleClientset()

		if tc.injectFailure {
			callCount := 0
			// PrependReactor to ensure action handled by our handler.
			client.Fake.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
				if a.GetVerb() == "create" {
					callCount++
					if callCount < secretCreationRetry {
						return true, nil, errors.New("failed to create secret deliberately")
					}
				}
				return true, nil, nil
			})
		}

		webhooks := map[string]*DNSNameEntry{
			sidecarInjectorSvcAccount: {
				ServiceName: sidecarInjectorSvc,
				Namespace:   "test-ns",
			},
		}
		controller, err := NewSecretController(createFakeCA(), enableNamespacesByDefault, defaultTTL,
			tc.gracePeriodRatio, defaultMinGracePeriod, false, client.CoreV1(), false, false,
			[]string{metav1.NamespaceAll}, webhooks, "test-ns")
		if tc.shouldFail {
			if err == nil {
				t.Errorf("should have failed to create secret controller")
			} else {
				// Should fail, skip the current case.
				continue
			}
		} else if err != nil {
			t.Errorf("failed to create secret controller: %v", err)
		}

		if tc.existingSecret != nil {
			err := controller.scrtStore.Add(tc.existingSecret)
			if err != nil {
				t.Errorf("Failed to add a secret (error %v)", err)
			}
		}

		if tc.saToAdd != nil {
			controller.saAdded(tc.saToAdd)
		}
		if tc.saToDelete != nil {
			controller.saDeleted(tc.saToDelete)
		}

		if err := checkActions(client.Actions(), tc.expectedActions); err != nil {
			t.Errorf("Case %q: %s", k, err.Error())
		}
	}
}

func TestSecretContent(t *testing.T) {
	saName := "test-serviceaccount"
	saNamespace := "test-namespace"
	client := fake.NewSimpleClientset()
	controller, err := NewSecretController(createFakeCA(), enableNamespacesByDefault, defaultTTL,
		defaultGracePeriodRatio, defaultMinGracePeriod, false, client.CoreV1(), false, false,
		[]string{metav1.NamespaceAll}, map[string]*DNSNameEntry{}, "test-namespace")
	if err != nil {
		t.Errorf("Failed to create secret controller: %v", err)
	}
	controller.saAdded(createServiceAccount(saName, saNamespace))

	_ = k8ssecret.BuildSecret(saName, GetSecretName(saName), saNamespace, nil, nil, nil, nil, nil, IstioSecretType)
	secret, err := client.CoreV1().Secrets(saNamespace).Get(GetSecretName(saName), metav1.GetOptions{})
	if err != nil {
		t.Errorf("Failed to retrieve secret: %v", err)
		return
	}
	if !bytes.Equal(rootCert, secret.Data[RootCertID]) {
		t.Errorf("Root cert verification error: expected %v but got %v", rootCert, secret.Data[RootCertID])
	}
	if !bytes.Equal(append(signedCert, certChain...), secret.Data[CertChainID]) {
		t.Errorf("Cert chain verification error: expected %v but got %v\n\n\n", certChain, secret.Data[CertChainID])
	}
}
func TestDeletedIstioSecret(t *testing.T) {
	client := fake.NewSimpleClientset()
	controller, err := NewSecretController(createFakeCA(), enableNamespacesByDefault, defaultTTL,
		defaultGracePeriodRatio, defaultMinGracePeriod, false, client.CoreV1(), false, false,
		[]string{metav1.NamespaceAll}, nil, "test-ns")
	if err != nil {
		t.Errorf("failed to create secret controller: %v", err)
	}
	sa := createServiceAccount("test-sa", "test-ns")
	if _, err := client.CoreV1().ServiceAccounts("test-ns").Create(sa); err != nil {
		t.Error(err)
	}

	saGvr := schema.GroupVersionResource{
		Resource: "serviceaccounts",
		Version:  "v1",
	}
	scrtGvr := schema.GroupVersionResource{
		Resource: "secrets",
		Version:  "v1",
	}
	nsGvr := schema.GroupVersionResource{
		Resource: "namespaces",
		Version:  "v1",
	}

	testCases := map[string]struct {
		secret          *v1.Secret
		expectedActions []ktesting.Action
	}{
		"Recover secret for existing service account": {
			secret: k8ssecret.BuildSecret("test-sa", "istio.test-sa", "test-ns", nil, nil, nil, nil, nil, IstioSecretType),
			expectedActions: []ktesting.Action{
				ktesting.NewGetAction(saGvr, "test-ns", "test-sa"),
				ktesting.NewGetAction(nsGvr, "test-ns", "test-ns"),
				ktesting.NewCreateAction(scrtGvr, "test-ns", k8ssecret.BuildSecret("test-sa", "istio.test-sa", "test-ns", nil, nil, nil, nil, nil, IstioSecretType)),
			},
		},
		"Do not recover secret for non-existing service account in the same namespace": {
			secret: k8ssecret.BuildSecret("test-sa2", "istio.test-sa2", "test-ns", nil, nil, nil, nil, nil, IstioSecretType),
			expectedActions: []ktesting.Action{
				ktesting.NewGetAction(saGvr, "test-ns", "test-sa2"),
			},
		},
		"Do not recover secret for service account in different namespace": {
			secret: k8ssecret.BuildSecret("test-sa", "istio.test-sa", "test-ns2", nil, nil, nil, nil, nil, IstioSecretType),
			expectedActions: []ktesting.Action{
				ktesting.NewGetAction(saGvr, "test-ns2", "test-sa"),
			},
		},
	}

	for k, tc := range testCases {
		client.ClearActions()
		controller.scrtDeleted(tc.secret)
		if err := checkActions(client.Actions(), tc.expectedActions); err != nil {
			t.Errorf("Failure in test case %s: %v", k, err)
		}
	}
}

func TestUpdateSecret(t *testing.T) {
	secretSchema := schema.GroupVersionResource{
		Resource: "secrets",
		Version:  "v1",
	}
	nsSchema := schema.GroupVersionResource{
		Resource: "namespaces",
		Version:  "v1",
	}

	testCases := map[string]struct {
		expectedActions  []ktesting.Action
		ttl              time.Duration
		minGracePeriod   time.Duration
		rootCert         []byte
		gracePeriodRatio float32
		certIsInvalid    bool
	}{
		"Does not update non-expiring secret": {
			expectedActions:  []ktesting.Action{},
			ttl:              time.Hour,
			gracePeriodRatio: 0.5,
			minGracePeriod:   10 * time.Minute,
		},
		"Update secret in grace period": {
			expectedActions: []ktesting.Action{
				ktesting.NewGetAction(nsSchema, "test-ns", "test-ns"),
				ktesting.NewUpdateAction(secretSchema, "test-ns", istioTestSecret),
			},
			ttl:              time.Hour,
			gracePeriodRatio: 1, // Always in grace period
			minGracePeriod:   10 * time.Minute,
		},
		"Update secret in min grace period": {
			expectedActions: []ktesting.Action{
				ktesting.NewGetAction(nsSchema, "test-ns", "test-ns"),
				ktesting.NewUpdateAction(secretSchema, "test-ns", istioTestSecret),
			},
			ttl:              10 * time.Minute,
			gracePeriodRatio: 0.5,
			minGracePeriod:   time.Hour, // ttl is always in minGracePeriod
		},
		"Update expired secret": {
			expectedActions: []ktesting.Action{
				ktesting.NewGetAction(nsSchema, "test-ns", "test-ns"),
				ktesting.NewUpdateAction(secretSchema, "test-ns", istioTestSecret),
			},
			ttl:              -time.Second,
			gracePeriodRatio: 0.5,
			minGracePeriod:   10 * time.Minute,
		},
		"Update secret with different root cert": {
			expectedActions: []ktesting.Action{
				ktesting.NewGetAction(nsSchema, "test-ns", "test-ns"),
				ktesting.NewUpdateAction(secretSchema, "test-ns", istioTestSecret),
			},
			ttl:              time.Hour,
			gracePeriodRatio: 0.5,
			minGracePeriod:   10 * time.Minute,
			rootCert:         []byte("Outdated root cert"),
		},
		"Update secret with invalid certificate": {
			expectedActions: []ktesting.Action{
				ktesting.NewGetAction(nsSchema, "test-ns", "test-ns"),
				ktesting.NewUpdateAction(secretSchema, "test-ns", istioTestSecret),
			},
			ttl:              time.Hour,
			gracePeriodRatio: 0.5,
			minGracePeriod:   10 * time.Minute,
			certIsInvalid:    true,
		},
	}

	for k, tc := range testCases {
		client := fake.NewSimpleClientset()

		controller, err := NewSecretController(createFakeCA(), enableNamespacesByDefault, time.Hour,
			tc.gracePeriodRatio, tc.minGracePeriod, false, client.CoreV1(), false, false,
			[]string{metav1.NamespaceAll}, nil, "")
		if err != nil {
			t.Errorf("failed to create secret controller: %v", err)
		}

		scrt := istioTestSecret
		if rc := tc.rootCert; rc != nil {
			scrt.Data[RootCertID] = rc
		}

		opts := util.CertOptions{
			IsSelfSigned: true,
			TTL:          tc.ttl,
			RSAKeySize:   512,
		}
		if !tc.certIsInvalid {
			bs, _, err := util.GenCertKeyFromOptions(opts)
			if err != nil {
				t.Error(err)
			}
			scrt.Data[CertChainID] = bs
		}

		controller.scrtUpdated(nil, scrt)

		if err := checkActions(client.Actions(), tc.expectedActions); err != nil {
			t.Errorf("Case %q: %s", k, err.Error())
		}
	}
}

func TestManagedNamespaceRules(t *testing.T) {
	testCases := map[string]struct {
		ns                        *v1.Namespace
		istioCaStorageNamespace   string
		enableNamespacesByDefault bool
		result                    bool
	}{
		"not managed by default, no override, and namespace label does not match actual ns => no secret": {
			ns:                        createNS("unlabeled", map[string]string{}),
			istioCaStorageNamespace:   "random",
			enableNamespacesByDefault: false,
			result:                    false,
		},
		"not managed by default, no override, and namespace matches => secret": {
			ns:                        createNS("unlabeled", map[string]string{NamespaceManagedLabel: "test-ns"}),
			istioCaStorageNamespace:   "test-ns",
			enableNamespacesByDefault: false,
			result:                    true,
		},
		"not managed by default, override is false, and namespace matches => no secret": {
			ns:                        createNS("unlabeled", map[string]string{NamespaceManagedLabel: "test-ns", NamespaceOverrideLabel: "false"}),
			istioCaStorageNamespace:   "test-ns",
			enableNamespacesByDefault: false,
			result:                    false,
		},
		"is managed by default, override is not present, and no namespace tag => secret": {
			ns:                        createNS("unlabeled", map[string]string{}),
			istioCaStorageNamespace:   "test-ns",
			enableNamespacesByDefault: true,
			result:                    true,
		},
		"is managed by default, override is false, and no namespace tag => no secret": {
			ns:                        createNS("unlabeled", map[string]string{NamespaceOverrideLabel: "false"}),
			istioCaStorageNamespace:   "test-ns",
			enableNamespacesByDefault: true,
			result:                    false,
		},
	}

	for k, tc := range testCases {
		t.Run(k, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			controller, err := NewSecretController(createFakeCA(), tc.enableNamespacesByDefault, defaultTTL,
				defaultGracePeriodRatio, defaultMinGracePeriod, false, client.CoreV1(), false, false,
				[]string{metav1.NamespaceAll}, nil, tc.istioCaStorageNamespace)
			if err != nil {
				t.Errorf("failed to create secret controller: %v", err)
			}
			client.ClearActions()

			if err != nil {
				t.Errorf("failed to create ns in %s: %v", k, err)
			}
			isManaged := controller.namespaceIsManaged(tc.ns)

			if isManaged != tc.result {
				t.Errorf("Failure in test case %s: expected %t but got %t", k, tc.result, isManaged)
			}
		})
	}
}

func TestRetroactiveNamespaceActivation(t *testing.T) {
	nsSchema := schema.GroupVersionResource{
		Resource: "namespaces",
		Version:  "v1",
	}
	saSchema := schema.GroupVersionResource{
		Resource: "serviceaccounts",
		Version:  "v1",
	}
	secretSchema := schema.GroupVersionResource{
		Resource: "secrets",
		Version:  "v1",
	}

	testCases := map[string]struct {
		enableNamespacesByDefault bool
		istioCaStorageNamespace   string
		oldNamespace              *v1.Namespace
		newNamespace              *v1.Namespace
		secret                    *v1.Secret
		sa                        *v1.ServiceAccount
		expectedActions           []ktesting.Action
	}{
		"toggling label ca.istio.io/env from false->true generates service accounts": {
			enableNamespacesByDefault: false,
			istioCaStorageNamespace:   "citadel",
			oldNamespace:              createNS("test", map[string]string{NamespaceManagedLabel: ""}),
			newNamespace:              createNS("test", map[string]string{NamespaceManagedLabel: "citadel"}),
			secret:                    k8ssecret.BuildSecret("test-sa", "istio.test-sa", "test", nil, nil, nil, nil, nil, IstioSecretType),
			sa:                        createServiceAccount("test-sa", "test"),
			expectedActions: []ktesting.Action{
				ktesting.NewCreateAction(nsSchema, "", createNS("test", map[string]string{})),
				ktesting.NewCreateAction(saSchema, "test", createServiceAccount("test-sa", "test")),
				ktesting.NewListAction(saSchema, schema.GroupVersionKind{}, "test", metav1.ListOptions{}),
				ktesting.NewCreateAction(secretSchema, "test", k8ssecret.BuildSecret("test-sa", "istio.test-sa", "test", nil, nil, nil, nil, nil, IstioSecretType)),
			},
		},
		"toggling label ca.istio.io/env from unlabeled to false should not generate secret": {
			enableNamespacesByDefault: false,
			istioCaStorageNamespace:   "citadel",
			oldNamespace:              createNS("test", map[string]string{}),
			newNamespace:              createNS("test", map[string]string{NamespaceManagedLabel: "false"}),
			secret:                    k8ssecret.BuildSecret("test-sa", "istio.test-sa", "test", nil, nil, nil, nil, nil, IstioSecretType),
			sa:                        createServiceAccount("test-sa", "test"),
			expectedActions: []ktesting.Action{
				ktesting.NewCreateAction(nsSchema, "", createNS("test", map[string]string{})),
				ktesting.NewCreateAction(saSchema, "test", createServiceAccount("test-sa", "test")),
			},
		},
	}

	for k, tc := range testCases {
		t.Run(k, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			controller, err := NewSecretController(createFakeCA(), tc.enableNamespacesByDefault, defaultTTL,
				defaultGracePeriodRatio, defaultMinGracePeriod, false, client.CoreV1(), false, false,
				[]string{metav1.NamespaceAll}, nil, tc.istioCaStorageNamespace)
			if err != nil {
				t.Errorf("failed to create secret controller: %v", err)
			}
			client.ClearActions()

			if _, err := client.CoreV1().Namespaces().Create(tc.oldNamespace); err != nil {
				t.Error(err)
			}
			if _, err := client.CoreV1().ServiceAccounts(tc.oldNamespace.GetName()).Create(tc.sa); err != nil {
				t.Error(err)
			}

			controller.namespaceUpdated(tc.oldNamespace, tc.newNamespace)

			if err := checkActions(client.Actions(), tc.expectedActions); err != nil {
				t.Errorf("Failure in test case %s: %v", k, err)
			}
		})
	}
}

func checkActions(actual, expected []ktesting.Action) error {
	if len(actual) != len(expected) {
		return fmt.Errorf("unexpected number of actions, want %d but got %d", len(expected), len(actual))
	}

	for i, action := range actual {
		expectedAction := expected[i]
		verb := expectedAction.GetVerb()
		resource := expectedAction.GetResource().Resource
		if !action.Matches(verb, resource) {
			return fmt.Errorf("unexpected %dth action, want %q but got %q", i, expectedAction, action)
		}
	}

	return nil
}

func createFakeCA() *mockca.FakeCA {
	return &mockca.FakeCA{
		SignedCert: signedCert,
		SignErr:    nil,
		KeyCertBundle: &mockutil.FakeKeyCertBundle{
			CertBytes:      caCert,
			PrivKeyBytes:   caKey,
			CertChainBytes: certChain,
			RootCertBytes:  rootCert,
		},
	}
}

func createServiceAccount(name, namespace string) *v1.ServiceAccount {
	return &v1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
}

func createNS(name string, labels map[string]string) *v1.Namespace {
	return &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
	}
}
