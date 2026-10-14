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

// Package acceptance contains test-only delivery helpers for acceptance
// evidence. It is not a production notification implementation.
package acceptance

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"strings"

	"github.com/labring-sigs/sealos-access/pkg/approvaladapter"
)

var (
	ErrUnauthorized = errors.New("acceptance notification access denied")
	ErrInvalid      = errors.New("acceptance notification is invalid")
)

// NotificationSink retains non-secret notification metadata in process
// memory. Reading it requires the bearer token supplied at construction;
// only its SHA-256 digest is retained.
type NotificationSink struct {
	tokenHash     [sha256.Size]byte
	notifications chan approvaladapter.Notification
}

// NewNotificationSink creates a protected in-memory sink. Acceptance tests
// should generate the token with crypto/rand and keep it in the test process.
func NewNotificationSink(accessToken string) (*NotificationSink, error) {
	if len(accessToken) < 32 || strings.TrimSpace(accessToken) != accessToken {
		return nil, errors.New("acceptance notification access token must contain at least 32 non-space bytes")
	}
	return &NotificationSink{
		tokenHash:     sha256.Sum256([]byte(accessToken)),
		notifications: make(chan approvaladapter.Notification, 4),
	}, nil
}

// Notify implements approvaladapter.Notifier. The payload type contains only
// approval metadata and the one-time reference; credential material cannot be
// carried by this sink's contract.
func (s *NotificationSink) Notify(ctx context.Context, notification approvaladapter.Notification) error {
	if s == nil {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(notification.Recipient) != notification.Recipient || notification.Recipient == "" ||
		strings.TrimSpace(notification.ApprovalID) != notification.ApprovalID || notification.ApprovalID == "" ||
		strings.TrimSpace(notification.Profile) != notification.Profile || notification.Profile == "" ||
		!validReference(notification.ApprovalReference) || notification.ExpiresAt.IsZero() {
		return ErrInvalid
	}
	select {
	case s.notifications <- notification:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WaitForNotification authenticates the reader before consuming the next
// notification. A wrong token never consumes a queued reference.
func (s *NotificationSink) WaitForNotification(ctx context.Context, accessToken string) (approvaladapter.Notification, error) {
	if s == nil || !s.authorized(accessToken) {
		return approvaladapter.Notification{}, ErrUnauthorized
	}
	select {
	case notification := <-s.notifications:
		return notification, nil
	case <-ctx.Done():
		return approvaladapter.Notification{}, ctx.Err()
	}
}

func (s *NotificationSink) authorized(accessToken string) bool {
	provided := sha256.Sum256([]byte(accessToken))
	return subtle.ConstantTimeCompare(provided[:], s.tokenHash[:]) == 1
}

func validReference(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && !strings.ContainsAny(value, " \t\r\n") && len(value) <= 512
}
