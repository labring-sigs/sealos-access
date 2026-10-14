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

package mock

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	userv1 "github.com/labring-sigs/sealos-access/api/v1"
	"github.com/labring-sigs/sealos-access/pkg/approvaladapter"
)

const mockIssuer = "https://mock-issuer.example"

// MockSource is an ApprovalSource that reads from a MockServer over HTTP.
// It implements the approvaladapter.ApprovalSource interface and is intended
// for testing the full approval adapter flow end-to-end.
type MockSource struct {
	baseURL    string
	httpClient *http.Client
}

// NewMockSource creates a MockSource that connects to the given mock server
// URL (typically the value returned by MockServer.Start()).
func NewMockSource(serverURL string) *MockSource {
	return &MockSource{
		baseURL:    serverURL,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// GetInstance fetches an approval instance from the mock server and converts
// it to the approvaladapter.ApprovalInstance type.
func (s *MockSource) GetInstance(ctx context.Context, instanceCode string) (approvaladapter.ApprovalInstance, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+"/v1/approvals/"+instanceCode, nil)
	if err != nil {
		return approvaladapter.ApprovalInstance{}, fmt.Errorf("mock source: create request: %w", err)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return approvaladapter.ApprovalInstance{}, fmt.Errorf("mock source: get instance: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return approvaladapter.ApprovalInstance{}, fmt.Errorf("mock source: HTTP %d", resp.StatusCode)
	}
	const maxResponseBytes = 64 * 1024
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return approvaladapter.ApprovalInstance{}, fmt.Errorf("mock source: read response: %w", err)
	}
	if len(body) > maxResponseBytes {
		return approvaladapter.ApprovalInstance{}, fmt.Errorf("mock source: response is too large")
	}
	var mockApp MockApproval
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&mockApp); err != nil {
		return approvaladapter.ApprovalInstance{}, fmt.Errorf("mock source: decode: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return approvaladapter.ApprovalInstance{}, fmt.Errorf("mock source: response contains more than one JSON value")
		}
		return approvaladapter.ApprovalInstance{}, fmt.Errorf("mock source: decode trailing response: %w", err)
	}
	if mockApp.Status != StatusApproved {
		return approvaladapter.ApprovalInstance{}, fmt.Errorf("mock source: %s is %s, not approved", instanceCode, mockApp.Status)
	}
	formJSON, _ := json.Marshal(mockApp.Form)
	return approvaladapter.ApprovalInstance{
		InstanceCode:        mockApp.ID,
		ApprovalCode:        mockApp.ApprovalCode,
		ClusterID:           mockApp.ClusterID,
		Status:              string(mockApp.Status),
		InitiatorID:         mockApp.InitiatorID,
		Target:              userv1.Identity{Issuer: mockIssuer, Subject: mockApp.TargetSubject},
		RequestedTTLSeconds: mockApp.RequestedTTL,
		ApprovalReason:      mockApp.Reason,
		Form:                string(formJSON),
	}, nil
}

// GetUserEmail resolves a user ID to an email address. For the mock source,
// the email is stored in the approval record as InitiatorEmail. We return a
// deterministic email for the user ID.
func (s *MockSource) GetUserEmail(_ context.Context, userID string) (string, error) {
	// For testing, use a deterministic mapping. A real implementation
	// would look up the user directory or HR system.
	return userID + "@example.internal", nil
}
