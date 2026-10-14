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
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

const (
	approvalReferenceVersion = "r1"
	approvalReferenceBytes   = 32
	approvalLeasePrefix      = "approval-"
)

func validateApprovalID(value string) error {
	if value == "" || strings.TrimSpace(value) != value || strings.IndexFunc(value, unicode.IsSpace) >= 0 || len(value) > 512 {
		return errors.New("approval ID is invalid")
	}
	return nil
}

func validateApprovalReason(value string) error {
	if strings.TrimSpace(value) == "" || len(value) > 2048 {
		return errors.New("approval reason is required and must be at most 2048 bytes")
	}
	return nil
}

func approvalLeaseName(approvalID string) string {
	digest := sha256.Sum256([]byte(approvalID))
	return approvalLeasePrefix + hex.EncodeToString(digest[:])[:32]
}

func newApprovalReference(leaseName string) (string, error) {
	secret := make([]byte, approvalReferenceBytes)
	if _, err := rand.Read(secret); err != nil {
		return "", fmt.Errorf("generate approval reference: %w", err)
	}
	return approvalReferenceVersion + "." + leaseName + "." + base64.RawURLEncoding.EncodeToString(secret), nil
}

func validateApprovalReference(reference string) error {
	leaseName, err := leaseNameFromApprovalReference(reference)
	if err != nil || leaseName == "" {
		return errors.New("approval reference is invalid")
	}
	return nil
}

func leaseNameFromApprovalReference(reference string) (string, error) {
	if reference == "" || strings.TrimSpace(reference) != reference || strings.IndexFunc(reference, unicode.IsSpace) >= 0 || len(reference) > 512 {
		return "", errors.New("approval reference is invalid")
	}
	parts := strings.Split(reference, ".")
	if len(parts) != 3 || parts[0] != approvalReferenceVersion || parts[1] == "" || parts[2] == "" {
		return "", errors.New("approval reference is invalid")
	}
	if !strings.HasPrefix(parts[1], approvalLeasePrefix) || len(parts[1]) != len(approvalLeasePrefix)+32 {
		return "", errors.New("approval reference is invalid")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(parts[1], approvalLeasePrefix)); err != nil {
		return "", errors.New("approval reference is invalid")
	}
	randomPart, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(randomPart) != approvalReferenceBytes {
		return "", errors.New("approval reference is invalid")
	}
	return parts[1], nil
}
