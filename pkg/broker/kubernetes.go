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
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// KubernetesTokenIssuer requests a bound ServiceAccount token and validates it
// through TokenReview before the Broker returns it. The TokenReview is needed
// because a TokenRequest response must not be treated as an opaque success by
// the Broker without checking the intended audience.
type KubernetesTokenIssuer struct {
	Client kubernetes.Interface
	Clock  func() time.Time
}

func (i *KubernetesTokenIssuer) Issue(ctx context.Context, namespace, serviceAccount string, spec authenticationv1.TokenRequestSpec) (TokenResult, error) {
	if i == nil || i.Client == nil {
		return TokenResult{}, errors.New("Kubernetes TokenIssuer is not configured")
	}
	if len(spec.Audiences) != 1 || spec.Audiences[0] == "" {
		return TokenResult{}, errors.New("exactly one token audience is required")
	}
	request, err := i.Client.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, serviceAccount, &authenticationv1.TokenRequest{Spec: spec}, metav1.CreateOptions{})
	if err != nil {
		return TokenResult{}, fmt.Errorf("create ServiceAccount TokenRequest: %w", err)
	}
	if request == nil || request.Status.Token == "" {
		return TokenResult{}, errors.New("TokenRequest returned an empty token")
	}
	if err := i.verifyToken(ctx, request.Status.Token, spec.Audiences); err != nil {
		return TokenResult{}, err
	}
	return TokenResult{
		Token:      request.Status.Token,
		Expiration: request.Status.ExpirationTimestamp.Time,
		Audiences:  append([]string(nil), spec.Audiences...),
	}, nil
}

func (i *KubernetesTokenIssuer) verifyToken(ctx context.Context, token string, audiences []string) error {
	review, err := i.Client.AuthenticationV1().TokenReviews().Create(ctx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: token, Audiences: audiences},
	}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("verify TokenRequest with TokenReview: %w", err)
	}
	if review == nil || !review.Status.Authenticated {
		return errors.New("TokenRequest token did not authenticate")
	}
	for _, expected := range audiences {
		for _, actual := range review.Status.Audiences {
			if actual == expected {
				return nil
			}
		}
	}
	return errors.New("TokenReview did not confirm the requested audience")
}
