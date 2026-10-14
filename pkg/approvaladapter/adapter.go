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

// Package approvaladapter defines the interfaces for translating external
// approval events into Broker approval records. Implementations supply an
// ApprovalSource and wire it to an Approver and Notifier. The package
// provides reusable BrokerHTTPClient (Approver) and optional EmailNotifier
// (Notifier) implementations.
package approvaladapter

import (
	"context"
	"strings"
	"time"
	"unicode"

	userv1 "github.com/labring-sigs/sealos-access/api/v1"
	"github.com/labring-sigs/sealos-access/pkg/broker"
)

// ApprovalInstance is the authoritative data fetched from an external
// approval source after an approved event. Form is the JSON-encoded
// approval form value whose schema depends on the source system.
type ApprovalInstance struct {
	InstanceCode        string
	ApprovalCode        string
	ClusterID           string
	Status              string
	InitiatorID         string
	Target              userv1.Identity
	RequestedTTLSeconds int64
	ApprovalReason      string
	Form                string
}

// ApprovalSource fetches authoritative approval data. Implementations must
// not return data from the callback event when the external approval API
// can be queried.
type ApprovalSource interface {
	GetInstance(ctx context.Context, instanceCode string) (ApprovalInstance, error)
	GetUserEmail(ctx context.Context, userID string) (string, error)
}

// Approver submits a validated external approval to the Broker.
type Approver interface {
	Approve(ctx context.Context, record broker.ApprovalRecord) (broker.ApprovalResponse, error)
}

// Notification is non-Kubernetes delivery metadata. It contains the one-time
// approval reference because the target user needs it for redemption, but it
// never contains a token or kubeconfig.
type Notification struct {
	ClusterID         string
	Recipient         string
	ApprovalID        string
	ApprovalReference string
	Profile           string
	ExpiresAt         time.Time
}

// Notifier delivers a Broker-generated reference to the approved target.
type Notifier interface {
	Notify(ctx context.Context, notification Notification) error
}

// validReference checks that a reference is non-empty, trimmed, has no
// internal whitespace, and fits within a reasonable length.
func validReference(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && strings.IndexFunc(value, unicode.IsSpace) < 0 && len(value) <= 512
}

// validEmail performs a basic structural check on an email address.
func validEmail(value string) bool {
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n") || len(value) > 320 {
		return false
	}
	parts := strings.Split(value, "@")
	return len(parts) == 2 && parts[0] != "" && parts[1] != "" && !strings.ContainsAny(parts[0], " <>(),:;[]\\\"") && !strings.ContainsAny(parts[1], " <>(),:;[]\\\"")
}
