//go:build acceptance

/*
Copyright 2026 labring.

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

package broker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	userv1 "github.com/labring-sigs/sealos-access/api/v1"
	"github.com/labring-sigs/sealos-access/pkg/internalcredentials"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const acceptanceKubernetesAudience = "kubernetes.default.svc"

// discardingTokenIssuer proves that the API Server accepted a real
// TokenRequest while keeping the returned credential out of the test state.
type discardingTokenIssuer struct {
	delegate TokenIssuer
	accepted bool
}

func (i *discardingTokenIssuer) Issue(ctx context.Context, namespace, serviceAccount string, spec authenticationv1.TokenRequestSpec) (TokenResult, error) {
	if _, err := i.delegate.Issue(ctx, namespace, serviceAccount, spec); err != nil {
		return TokenResult{}, fmt.Errorf("real Kubernetes TokenRequest failed: %w", err)
	}
	i.accepted = true
	return TokenResult{}, errors.New("simulated response loss after API Server acceptance")
}

func TestEnvtestKubernetesTokenIssuerResponseLoss(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	useExisting := false
	testEnv := &envtest.Environment{
		UseExistingCluster:       &useExisting,
		CRDDirectoryPaths:        []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing:    true,
		ControlPlaneStartTimeout: 60 * time.Second,
		ControlPlaneStopTimeout:  60 * time.Second,
	}
	testEnv.ControlPlane.APIServer = &envtest.APIServer{}
	testEnv.ControlPlane.APIServer.Configure().Set("api-audiences", acceptanceKubernetesAudience)

	config, err := testEnv.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := testEnv.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})

	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("create Kubernetes client: %v", err)
	}
	leaseClient, err := client.New(config, client.Options{Scheme: testScheme()})
	if err != nil {
		t.Fatalf("create controller-runtime client: %v", err)
	}

	for _, namespace := range []string{userv1.InternalUserSystemNamespace, userv1.CredentialBrokerNamespace} {
		if _, err := kubeClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create namespace %q: %v", namespace, err)
		}
	}

	identity := userv1.Identity{Issuer: "https://issuer.example", Subject: "envtest-response-loss"}
	user := &userv1.InternalUser{
		ObjectMeta: metav1.ObjectMeta{Name: internalcredentials.DeriveInternalUserName(identity.Issuer, identity.Subject)},
		Spec: userv1.InternalUserSpec{
			Identity:    identity,
			RoleProfile: userv1.BaseReadonlyProfile,
		},
	}
	if err := leaseClient.Create(ctx, user); err != nil {
		t.Fatalf("create InternalUser: %v", err)
	}
	user.Status = userv1.InternalUserStatus{
		Phase:       userv1.InternalUserActive,
		DerivedName: user.Name,
	}
	if err := leaseClient.Status().Update(ctx, user); err != nil {
		t.Fatalf("mark InternalUser active: %v", err)
	}

	profile, ok := internalcredentials.ProfileFor(userv1.ClusterOpsWriteProfile)
	if !ok {
		t.Fatal("cluster-ops-write profile is not configured")
	}
	if _, err := kubeClient.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: profile.RoleName},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create profile ClusterRole: %v", err)
	}

	leaseName := "lease-envtest-response-loss"
	reference, err := newApprovalReference(leaseName)
	if err != nil {
		t.Fatalf("create approval reference: %v", err)
	}
	lease := &userv1.CredentialLease{
		ObjectMeta: metav1.ObjectMeta{Name: leaseName, Namespace: userv1.CredentialBrokerNamespace},
		Spec: userv1.CredentialLeaseSpec{
			Target:              identity,
			Requester:           identity,
			Profile:             userv1.ClusterOpsWriteProfile,
			RequestedTTLSeconds: 30 * 60,
			ApprovalReference:   reference,
			ApprovalID:          "envtest-response-loss",
			ApprovalReason:      "envtest response loss",
		},
	}
	if err := leaseClient.Create(ctx, lease); err != nil {
		t.Fatalf("create CredentialLease: %v", err)
	}

	serviceAccountName := internalcredentials.ResourceName("internal-lease", lease.Namespace, lease.Name)
	secretName := internalcredentials.ResourceName("internal-lease-secret", lease.Namespace, lease.Name)
	bindingName := internalcredentials.ResourceName("internal-lease-binding", lease.Namespace, lease.Name)
	labels := map[string]string{
		userv1.ManagedByLabel:          "credential-lease-controller",
		userv1.CredentialLeaseLabel:    lease.Name,
		userv1.CredentialLeaseUIDLabel: string(lease.UID),
	}
	serviceAccount, err := kubeClient.CoreV1().ServiceAccounts(userv1.InternalUserSystemNamespace).Create(ctx, &corev1.ServiceAccount{
		ObjectMeta:                   metav1.ObjectMeta{Name: serviceAccountName, Namespace: userv1.InternalUserSystemNamespace, Labels: labels},
		AutomountServiceAccountToken: boolPtr(false),
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create temporary ServiceAccount: %v", err)
	}
	secret, err := kubeClient.CoreV1().Secrets(userv1.InternalUserSystemNamespace).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: userv1.InternalUserSystemNamespace, Labels: labels},
		Type:       corev1.SecretTypeOpaque,
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create bound Secret: %v", err)
	}
	binding, err := kubeClient.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: bindingName, Labels: labels},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     profile.RoleName,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Namespace: userv1.InternalUserSystemNamespace,
			Name:      serviceAccount.Name,
		}},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create temporary ClusterRoleBinding: %v", err)
	}

	lease.Status = userv1.CredentialLeaseStatus{
		Phase: userv1.CredentialLeasePrepared,
		ServiceAccountRef: &corev1.ObjectReference{
			APIVersion: "v1", Kind: "ServiceAccount", Namespace: serviceAccount.Namespace,
			Name: serviceAccount.Name, UID: serviceAccount.UID,
		},
		BoundSecretRef: &corev1.ObjectReference{
			APIVersion: "v1", Kind: "Secret", Namespace: secret.Namespace,
			Name: secret.Name, UID: secret.UID,
		},
		BindingRef: &corev1.ObjectReference{
			APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRoleBinding",
			Name: binding.Name, UID: binding.UID,
		},
	}
	if err := leaseClient.Status().Update(ctx, lease); err != nil {
		t.Fatalf("mark CredentialLease prepared: %v", err)
	}

	issuer := &discardingTokenIssuer{delegate: &KubernetesTokenIssuer{Client: kubeClient}}
	server := newTestServer(t, leaseClient, staticAuthenticator{principal: Principal{
		Issuer:  identity.Issuer,
		Subject: identity.Subject,
	}}, issuer, time.Now())

	_, err = server.issueLease(ctx, lease, profile)
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Code != http.StatusServiceUnavailable {
		t.Fatalf("issue() error = %v, want service unavailable", err)
	}
	if !issuer.accepted {
		t.Fatal("real KubernetesTokenIssuer did not complete TokenRequest and TokenReview")
	}

	storedLease := &userv1.CredentialLease{}
	if err := leaseClient.Get(ctx, types.NamespacedName{Namespace: lease.Namespace, Name: lease.Name}, storedLease); err != nil {
		t.Fatalf("get stored CredentialLease: %v", err)
	}
	if storedLease.Status.ConsumedAt == nil || storedLease.Status.Phase != userv1.CredentialLeasePrepared {
		t.Fatalf("lease status = %#v, want consumed Prepared", storedLease.Status)
	}
	if _, err := server.markConsumed(p1Context(ctx), storedLease); err == nil {
		t.Fatal("consumed CredentialLease was redeemable after accepted TokenRequest response loss")
	}
}

func boolPtr(value bool) *bool {
	return &value
}
