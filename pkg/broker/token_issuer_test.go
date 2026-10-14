package broker

import (
	"context"
	"errors"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestKubernetesTokenIssuerValidatesTokenReview(t *testing.T) {
	now := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name           string
		token          string
		tokenErr       error
		tokenReviewErr error
		authenticated  bool
		audiences      []string
		wantErr        bool
	}{
		{name: "success", token: "issued-token", authenticated: true, audiences: []string{"kubernetes.default.svc"}},
		{name: "TokenRequest error", tokenErr: errors.New("TokenRequest unavailable"), wantErr: true},
		{name: "empty TokenRequest", authenticated: true, audiences: []string{"kubernetes.default.svc"}, wantErr: true},
		{name: "TokenReview error", token: "issued-token", tokenReviewErr: errors.New("TokenReview unavailable"), wantErr: true},
		{name: "unauthenticated", token: "issued-token", authenticated: false, audiences: []string{"kubernetes.default.svc"}, wantErr: true},
		{name: "audience mismatch", token: "issued-token", authenticated: true, audiences: []string{"other-audience"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := k8sfake.NewSimpleClientset()
			var tokenNamespace, tokenName string
			var tokenSpec authenticationv1.TokenRequestSpec
			var reviewSpec authenticationv1.TokenReviewSpec
			clientset.PrependReactor("create", "serviceaccounts", func(action k8stesting.Action) (bool, runtime.Object, error) {
				create := action.(k8stesting.CreateAction)
				if create.GetSubresource() != "token" {
					return false, nil, nil
				}
				tokenNamespace = create.GetNamespace()
				tokenName = action.(k8stesting.CreateActionImpl).Name
				tokenRequest := create.GetObject().(*authenticationv1.TokenRequest)
				tokenSpec = tokenRequest.Spec
				if tt.tokenErr != nil {
					return true, nil, tt.tokenErr
				}
				return true, &authenticationv1.TokenRequest{Status: authenticationv1.TokenRequestStatus{
					Token:               tt.token,
					ExpirationTimestamp: metav1.NewTime(now.Add(30 * time.Minute)),
				}}, nil
			})
			clientset.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
				create := action.(k8stesting.CreateAction)
				reviewSpec = create.GetObject().(*authenticationv1.TokenReview).Spec
				if tt.tokenReviewErr != nil {
					return true, nil, tt.tokenReviewErr
				}
				return true, &authenticationv1.TokenReview{Status: authenticationv1.TokenReviewStatus{
					Authenticated: tt.authenticated,
					Audiences:     tt.audiences,
				}}, nil
			})

			issuer := &KubernetesTokenIssuer{Client: clientset, Clock: func() time.Time { return now }}
			spec := authenticationv1.TokenRequestSpec{
				Audiences: []string{"kubernetes.default.svc"},
				BoundObjectRef: &authenticationv1.BoundObjectReference{
					Kind:       "Secret",
					APIVersion: "v1",
					Name:       "bound-secret",
					UID:        "secret-uid",
				},
			}
			result, err := issuer.Issue(context.Background(), "access-system", "temporary-account", spec)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Issue() error = %v, wantErr %t", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if result.Token != tt.token || !result.Expiration.Equal(now.Add(30*time.Minute)) || len(result.Audiences) != 1 || result.Audiences[0] != spec.Audiences[0] {
				t.Fatalf("result = %#v", result)
			}
			if tokenNamespace != "access-system" || tokenName != "temporary-account" {
				t.Fatalf("TokenRequest target = %s/%s", tokenNamespace, tokenName)
			}
			if len(tokenSpec.Audiences) != 1 || tokenSpec.Audiences[0] != spec.Audiences[0] || tokenSpec.BoundObjectRef == nil || tokenSpec.BoundObjectRef.UID != "secret-uid" {
				t.Fatalf("TokenRequest spec = %#v", tokenSpec)
			}
			if len(reviewSpec.Audiences) != 1 || reviewSpec.Audiences[0] != spec.Audiences[0] || reviewSpec.Token != tt.token {
				t.Fatalf("TokenReview spec = %#v", reviewSpec)
			}
		})
	}
}

func TestKubernetesTokenIssuerRejectsInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name   string
		issuer *KubernetesTokenIssuer
		spec   authenticationv1.TokenRequestSpec
	}{
		{name: "nil issuer", issuer: nil, spec: authenticationv1.TokenRequestSpec{Audiences: []string{"audience"}}},
		{name: "nil client", issuer: &KubernetesTokenIssuer{}, spec: authenticationv1.TokenRequestSpec{Audiences: []string{"audience"}}},
		{name: "missing audience", issuer: &KubernetesTokenIssuer{Client: k8sfake.NewSimpleClientset()}, spec: authenticationv1.TokenRequestSpec{}},
		{name: "multiple audiences", issuer: &KubernetesTokenIssuer{Client: k8sfake.NewSimpleClientset()}, spec: authenticationv1.TokenRequestSpec{Audiences: []string{"a", "b"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.issuer.Issue(context.Background(), "namespace", "service-account", tt.spec); err == nil {
				t.Fatal("Issue() accepted invalid configuration")
			}
		})
	}
}
