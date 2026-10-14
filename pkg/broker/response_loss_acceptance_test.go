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
	"os"
	"testing"
	"time"

	userv1 "github.com/labring-sigs/sealos-access/api/v1"
	"github.com/labring-sigs/sealos-access/pkg/internalcredentials"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// apiServerAcceptedResponseLossIssuer performs the real TokenRequest and then
// discards the successful response. This models the response being lost after
// the API Server accepted the request, without ever retaining token material.
type apiServerAcceptedResponseLossIssuer struct {
	client   kubernetes.Interface
	accepted bool
}

func (i *apiServerAcceptedResponseLossIssuer) Issue(ctx context.Context, namespace, serviceAccount string, spec authenticationv1.TokenRequestSpec) (TokenResult, error) {
	_, err := i.client.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, serviceAccount, &authenticationv1.TokenRequest{Spec: spec}, metav1.CreateOptions{})
	if err != nil {
		return TokenResult{}, fmt.Errorf("TokenRequest was not accepted: %w", err)
	}
	i.accepted = true
	return TokenResult{}, errors.New("simulated response loss after API Server acceptance")
}

func TestTokenRequestResponseLossAfterAPIServerAcceptance(t *testing.T) {
	if os.Getenv("INTERNAL_USER_ACCEPTANCE_CLUSTER") != "true" {
		t.Skip("set INTERNAL_USER_ACCEPTANCE_CLUSTER=true to run against the disposable acceptance cluster")
	}

	ctx := context.Background()
	config, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	namespace := userv1.InternalUserSystemNamespace
	suffix := string(uuid.NewUUID())
	leaseName := "lease-response-loss-" + suffix
	serviceAccountName := internalcredentials.ResourceName("internal-lease", userv1.CredentialBrokerNamespace, leaseName)
	secretName := "acceptance-response-loss-" + suffix
	serviceAccount, err := kubeClient.CoreV1().ServiceAccounts(namespace).Create(ctx, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccountName, Namespace: namespace},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = kubeClient.CoreV1().Secrets(namespace).Delete(context.Background(), secretName, metav1.DeleteOptions{})
		_ = kubeClient.CoreV1().ServiceAccounts(namespace).Delete(context.Background(), serviceAccount.Name, metav1.DeleteOptions{})
	}()
	secret, err := kubeClient.CoreV1().Secrets(namespace).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: namespace},
		Type:       corev1.SecretTypeOpaque,
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	identity := userv1.Identity{Issuer: "https://issuer.example", Subject: "acceptance-response-loss"}
	user := &userv1.InternalUser{
		ObjectMeta: metav1.ObjectMeta{Name: internalcredentials.DeriveInternalUserName(identity.Issuer, identity.Subject)},
		Spec:       userv1.InternalUserSpec{Identity: identity, RoleProfile: userv1.BaseReadonlyProfile},
		Status:     userv1.InternalUserStatus{Phase: userv1.InternalUserActive},
	}
	reference, err := newApprovalReference(leaseName)
	if err != nil {
		t.Fatal(err)
	}
	lease := &userv1.CredentialLease{
		ObjectMeta: metav1.ObjectMeta{Name: leaseName, Namespace: userv1.CredentialBrokerNamespace, UID: types.UID("lease-response-loss-uid")},
		Spec: userv1.CredentialLeaseSpec{
			Target:              identity,
			Requester:           identity,
			Profile:             userv1.ClusterOpsWriteProfile,
			RequestedTTLSeconds: 30 * 60,
			ApprovalReference:   reference,
			ApprovalID:          "acceptance-response-loss",
			ApprovalReason:      "response loss test",
		},
		Status: userv1.CredentialLeaseStatus{
			Phase: userv1.CredentialLeasePrepared,
			ServiceAccountRef: &corev1.ObjectReference{
				APIVersion: "v1", Kind: "ServiceAccount", Namespace: namespace, Name: serviceAccount.Name,
			},
			BoundSecretRef: &corev1.ObjectReference{
				APIVersion: "v1", Kind: "Secret", Namespace: namespace, Name: secret.Name, UID: secret.UID,
			},
			BindingRef: &corev1.ObjectReference{
				APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding",
				Name: internalcredentials.ResourceName("internal-lease-binding", userv1.CredentialBrokerNamespace, leaseName),
			},
		},
	}
	leaseClient := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&userv1.CredentialLease{}).WithObjects(user, lease).Build()
	issuer := &apiServerAcceptedResponseLossIssuer{client: kubeClient}
	server := newTestServer(t, leaseClient, staticAuthenticator{principal: Principal{Issuer: identity.Issuer, Subject: identity.Subject}}, issuer, time.Now())

	profile, _ := internalcredentials.ProfileFor(userv1.ClusterOpsWriteProfile)
	_, err = server.issueLease(ctx, lease, profile)
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Code != http.StatusServiceUnavailable {
		t.Fatalf("issue() error = %v, want service unavailable", err)
	}
	if !issuer.accepted {
		t.Fatal("TokenRequest did not reach API Server acceptance")
	}

	storedLease := &userv1.CredentialLease{}
	if err := leaseClient.Get(ctx, types.NamespacedName{Namespace: userv1.CredentialBrokerNamespace, Name: leaseName}, storedLease); err != nil {
		t.Fatal(err)
	}
	if storedLease.Status.ConsumedAt == nil || storedLease.Status.Phase != userv1.CredentialLeasePrepared {
		t.Fatalf("lease status = %#v, want consumed Prepared", storedLease.Status)
	}
	if _, err := server.markConsumed(p1Context(ctx), storedLease); err == nil {
		t.Fatal("consumed Lease was redeemable after an accepted TokenRequest response loss")
	}
}
