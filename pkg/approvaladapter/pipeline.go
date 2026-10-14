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

package approvaladapter

import (
	"context"
	"errors"
	"fmt"

	"github.com/labring-sigs/sealos-access/pkg/broker"
)

// Pipeline composes an authoritative ApprovalSource, a Broker Approver, and
// a protected Notifier. The source and notifier are intentionally injected so
// protocol and delivery choices remain outside the adapter core.
type Pipeline struct {
	source   ApprovalSource
	approver Approver
	notifier Notifier
}

// NewPipeline constructs an approval processing pipeline from its required
// pluggable dependencies.
func NewPipeline(source ApprovalSource, approver Approver, notifier Notifier) (*Pipeline, error) {
	if source == nil || approver == nil || notifier == nil {
		return nil, errors.New("approval pipeline requires source, approver, and notifier")
	}
	return &Pipeline{source: source, approver: approver, notifier: notifier}, nil
}

// Process fetches the authoritative approval instance, submits its
// non-secret record to the Broker, and delivers only the Broker-generated
// reference through the injected notifier.
func (p *Pipeline) Process(ctx context.Context, instanceCode string) error {
	if p == nil || p.source == nil || p.approver == nil || p.notifier == nil {
		return errors.New("approval pipeline is not configured")
	}
	instance, err := p.source.GetInstance(ctx, instanceCode)
	if err != nil {
		return fmt.Errorf("fetch instance %s: %w", instanceCode, err)
	}
	if instance.Target.Issuer == "" || instance.Target.Subject == "" {
		return fmt.Errorf("approval instance %s has no authoritative target", instanceCode)
	}
	if instance.RequestedTTLSeconds <= 0 {
		return fmt.Errorf("approval instance %s has no valid authoritative TTL", instanceCode)
	}
	if instance.ApprovalReason == "" {
		return fmt.Errorf("approval instance %s has no authoritative reason", instanceCode)
	}
	clusterID := instance.ClusterID
	if err := broker.ValidateTargetID(clusterID); err != nil {
		return fmt.Errorf("approval instance %s has invalid target cluster: %w", instanceCode, err)
	}

	userEmail, err := p.source.GetUserEmail(ctx, instance.Target.Subject)
	if err != nil {
		return fmt.Errorf("resolve email for %s: %w", instance.Target.Subject, err)
	}

	response, err := p.approver.Approve(ctx, broker.ApprovalRecord{
		ClusterID:           clusterID,
		ApprovalID:          instance.InstanceCode,
		Target:              instance.Target,
		RequestedTTLSeconds: instance.RequestedTTLSeconds,
		ApprovalReason:      instance.ApprovalReason,
	})
	if err != nil {
		return fmt.Errorf("submit approval to broker: %w", err)
	}
	if !validReference(response.ApprovalReference) {
		return fmt.Errorf("Broker returned an invalid approval reference")
	}

	if err := p.notifier.Notify(ctx, Notification{
		ClusterID:         clusterID,
		Recipient:         userEmail,
		ApprovalID:        response.ApprovalID,
		ApprovalReference: response.ApprovalReference,
		Profile:           response.Profile,
		ExpiresAt:         response.ApprovalExpiresAt,
	}); err != nil {
		return fmt.Errorf("notify user %s: %w", instance.Target.Subject, err)
	}
	return nil
}
