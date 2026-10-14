/*
Copyright 2026 labring.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package mock

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	userv1 "github.com/labring-sigs/sealos-access/api/v1"
	"github.com/labring-sigs/sealos-access/pkg/approvaladapter"
	"github.com/labring-sigs/sealos-access/pkg/broker"
)

// TestMockServerLifecycle verifies that the MockServer can start, accept
// approval creation, approve it, and return the correct data.
func TestMockServerLifecycle(t *testing.T) {
	s := NewMockServer()
	baseURL := s.Start()
	defer s.Close()

	// Create an approval via HTTP.
	resp, err := http.Post(baseURL+"/v1/approvals", "application/json",
		strings.NewReader(`{"clusterID":"cluster-a","initiatorID":"alice","targetSubject":"alice","reason":"testing","requestedTTLSeconds":1800}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create: HTTP %d", resp.StatusCode)
	}
	var createResp struct{ InstanceCode string }
	if err := json.NewDecoder(resp.Body).Decode(&createResp); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if createResp.InstanceCode == "" {
		t.Fatal("create: empty instance code")
	}

	// Approve it via HTTP.
	resp, err = http.Post(baseURL+"/v1/approvals/"+createResp.InstanceCode+"/approve", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approve: HTTP %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Fetch the approval and verify its fields.
	app, ok := s.GetApproval(createResp.InstanceCode)
	if !ok {
		t.Fatal("approval not found")
	}
	if app.Status != StatusApproved {
		t.Fatalf("status = %q, want %q", app.Status, StatusApproved)
	}
	if app.InitiatorID != "alice" {
		t.Fatalf("initiatorID = %q, want %q", app.InitiatorID, "alice")
	}
	if app.TargetSubject != "alice" {
		t.Fatalf("targetSubject = %q, want %q", app.TargetSubject, "alice")
	}
}

// TestMockSourceFetchesApprovedInstance verifies that MockSource correctly
// reads from MockServer and returns the proper ApprovalInstance.
func TestMockSourceFetchesApprovedInstance(t *testing.T) {
	s := NewMockServer()
	baseURL := s.Start()
	defer s.Close()

	// Create and approve an approval programmatically.
	code := s.CreateApproval(CreateApprovalRequest{
		ApprovalCode:   "mock-code-001",
		ClusterID:      "cluster-a",
		InitiatorID:    "alice",
		InitiatorEmail: "alice@example.internal",
		Form:           map[string]any{"reason": "incident", "ttl": 1800},
		TargetSubject:  "alice",
		Reason:         "testing",
		RequestedTTL:   1800,
	})
	if err := s.ApproveApproval(code); err != nil {
		t.Fatal(err)
	}

	source := NewMockSource(baseURL)
	instance, err := source.GetInstance(context.Background(), code)
	if err != nil {
		t.Fatal(err)
	}
	if instance.InstanceCode != code {
		t.Fatalf("InstanceCode = %q, want %q", instance.InstanceCode, code)
	}
	if instance.Status != string(StatusApproved) {
		t.Fatalf("Status = %q, want %q", instance.Status, StatusApproved)
	}
	if instance.InitiatorID != "alice" {
		t.Fatalf("InitiatorID = %q, want %q", instance.InitiatorID, "alice")
	}
	if instance.Target != (userv1.Identity{Issuer: mockIssuer, Subject: "alice"}) {
		t.Fatalf("Target = %#v, want mock target", instance.Target)
	}
	if instance.RequestedTTLSeconds != 1800 || instance.ApprovalReason != "testing" {
		t.Fatalf("authoritative request fields = ttl %d reason %q", instance.RequestedTTLSeconds, instance.ApprovalReason)
	}

	// Verify form JSON round-trips correctly.
	var form map[string]any
	if err := json.Unmarshal([]byte(instance.Form), &form); err != nil {
		t.Fatal(err)
	}
	if form["reason"] != "incident" {
		t.Fatalf("form.reason = %v, want %v", form["reason"], "incident")
	}
}

func TestProcessApprovalUsesAuthoritativeTarget(t *testing.T) {
	mockExt := NewMockServer()
	extURL := mockExt.Start()
	defer mockExt.Close()

	code := mockExt.CreateApproval(CreateApprovalRequest{
		ApprovalCode:   "authoritative-target",
		ClusterID:      "cluster-a",
		InitiatorID:    "approval-operator",
		TargetSubject:  "credential-target",
		RequestedTTL:   1200,
		Reason:         "authoritative reason",
		InitiatorEmail: "approval-operator@example.internal",
	})
	if err := mockExt.ApproveApproval(code); err != nil {
		t.Fatal(err)
	}

	approver := &capturingApprover{response: broker.ApprovalResponse{
		ApprovalID:        code,
		ApprovalReference: "r1.authoritative-reference",
		Profile:           userv1.ClusterOpsWriteProfile,
	}}
	notifier := &capturingNotifier{}
	if err := processApproval(context.Background(), NewMockSource(extURL), approver, notifier, code); err != nil {
		t.Fatal(err)
	}
	if len(approver.records) != 1 {
		t.Fatalf("approval records = %d, want 1", len(approver.records))
	}
	record := approver.records[0]
	if record.Target != (userv1.Identity{Issuer: mockIssuer, Subject: "credential-target"}) || record.RequestedTTLSeconds != 1200 || record.ApprovalReason != "authoritative reason" {
		t.Fatalf("approval record = %#v, want authoritative target and fields", record)
	}
	if len(notifier.notifications) != 1 || notifier.notifications[0].Recipient != "credential-target@example.internal" {
		t.Fatalf("notifications = %#v, want target recipient", notifier.notifications)
	}
}

type capturingApprover struct {
	records  []broker.ApprovalRecord
	response broker.ApprovalResponse
}

func (a *capturingApprover) Approve(_ context.Context, record broker.ApprovalRecord) (broker.ApprovalResponse, error) {
	a.records = append(a.records, record)
	return a.response, nil
}

type capturingNotifier struct {
	notifications []approvaladapter.Notification
}

func (n *capturingNotifier) Notify(_ context.Context, notification approvaladapter.Notification) error {
	n.notifications = append(n.notifications, notification)
	return nil
}

// TestMockSourceRejectsNonApproved verifies that GetInstance returns an error
// for a pending (not yet approved) instance.
func TestMockSourceRejectsNonApproved(t *testing.T) {
	s := NewMockServer()
	baseURL := s.Start()
	defer s.Close()

	code := s.CreateApproval(CreateApprovalRequest{
		ClusterID:     "cluster-a",
		InitiatorID:   "bob",
		TargetSubject: "bob",
		Reason:        "testing",
	})
	// Do NOT approve — keep it pending.

	source := NewMockSource(baseURL)
	if _, err := source.GetInstance(context.Background(), code); err == nil {
		t.Fatal("expected error for non-approved instance, got nil")
	}
}

// TestIntegrationFullPipeline exercises the real approval adapter interfaces
// end-to-end using the mock external approval system, a mock Broker server,
// and an in-memory Notifier.
//
// This test demonstrates how a custom ApprovalSource would wire into the
// approval adapter: MockSource -> BrokerHTTPClient -> Notifier.
func TestIntegrationFullPipeline(t *testing.T) {
	// 1. Start the mock external approval system.
	mockExt := NewMockServer()
	extURL := mockExt.Start()
	defer mockExt.Close()

	// 2. Start a mock Broker server (simulates the real Broker's approve
	//    endpoint). Uses TLS — BrokerHTTPClient validates HTTPS.
	mockBroker := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/clusters/cluster-a/internal/credentials/approve" || r.Method != http.MethodPost {
			t.Fatalf("unexpected broker request: %s %s", r.Method, r.URL.Path)
		}
		// Decode the request to verify it.
		var record broker.ApprovalRecord
		if err := json.NewDecoder(r.Body).Decode(&record); err != nil {
			t.Fatal(err)
		}
		if record.Target.Subject != "alice" {
			t.Fatalf("record target = %#v", record.Target)
		}
		// Simulate a successful Broker response.
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"clusterID":"cluster-a",
			"approvalID":"mock-instance-0000",
			"approvalReference":"r1.mock-test-reference",
			"profile":"cluster-ops-write.v1",
			"requestedTTLSeconds":1800,
			"approvalExpiresAt":"2026-09-11T00:00:00Z"
		}`)
	}))
	defer mockBroker.Close()

	// 3. Wire the real implementations.
	source := NewMockSource(extURL)
	approver, err := approvaladapter.NewBrokerHTTPClient(mockBroker.URL, "cluster-a", mockBroker.Client())
	if err != nil {
		t.Fatal(err)
	}

	// 4. Wire an in-memory notifier. A deployment can provide any
	// notification implementation that satisfies the interface.
	notifier := &capturingNotifier{}

	// 5. Create and approve an approval in the mock external system.
	code := mockExt.CreateApproval(CreateApprovalRequest{
		ApprovalCode:   "demo-approval",
		ClusterID:      "cluster-a",
		InitiatorID:    "alice",
		InitiatorEmail: "alice@example.internal",
		Form:           map[string]any{"reason": "demo"},
		TargetSubject:  "alice",
		Reason:         "demo integration test",
		RequestedTTL:   1800,
	})
	if err := mockExt.ApproveApproval(code); err != nil {
		t.Fatal(err)
	}

	// 6. Process the approval — this is what a real ApprovalSource does
	//    in its event handler or poll loop.
	instance, err := source.GetInstance(context.Background(), code)
	if err != nil {
		t.Fatal(err)
	}
	if instance.Status != string(StatusApproved) {
		t.Fatalf("instance status = %q, want %q", instance.Status, StatusApproved)
	}

	// 7. Submit to the mock Broker.
	response, err := approver.Approve(context.Background(), broker.ApprovalRecord{
		ClusterID:           "cluster-a",
		ApprovalID:          instance.InstanceCode,
		Target:              userv1.Identity{Issuer: "https://mock-issuer.example", Subject: "alice"},
		RequestedTTLSeconds: 1800,
		ApprovalReason:      "mock integration test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.ApprovalReference != "r1.mock-test-reference" {
		t.Fatalf("ApprovalReference = %q, want %q", response.ApprovalReference, "r1.mock-test-reference")
	}

	// 8. Notify the target user.
	userEmail, err := source.GetUserEmail(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if userEmail != "alice@example.internal" {
		t.Fatalf("userEmail = %q, want %q", userEmail, "alice@example.internal")
	}
	if err := notifier.Notify(context.Background(), approvaladapter.Notification{
		Recipient:         userEmail,
		ApprovalID:        response.ApprovalID,
		ApprovalReference: response.ApprovalReference,
		Profile:           response.Profile,
		ExpiresAt:         response.ApprovalExpiresAt,
	}); err != nil {
		t.Fatal(err)
	}

	// 9. Verify the notification contains the reference but no credential
	// material.
	if len(notifier.notifications) != 1 || notifier.notifications[0].ApprovalReference != response.ApprovalReference {
		t.Fatalf("notifications = %#v, want approval reference %q", notifier.notifications, response.ApprovalReference)
	}
	notificationJSON, err := json.Marshal(notifier.notifications[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"token", "kubeconfig", "secretData", "privateKey"} {
		if strings.Contains(strings.ToLower(string(notificationJSON)), strings.ToLower(forbidden)) {
			t.Fatalf("notification contains forbidden field %q: %s", forbidden, notificationJSON)
		}
	}

	t.Logf("Integration test passed: code=%s reference=%s", code, response.ApprovalReference)
}

// TestPollProcessesApprovalViaChannel verifies that the Poll loop correctly
// processes an approval when a code is sent through the channel.
func TestPollProcessesApprovalViaChannel(t *testing.T) {
	mockExt := NewMockServer()
	extURL := mockExt.Start()
	defer mockExt.Close()

	mockBroker := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"clusterID":"cluster-a",
			"approvalID":"mock-001",
			"approvalReference":"r1.poll-test-ref",
			"profile":"cluster-ops-write.v1",
			"requestedTTLSeconds":1800,
			"approvalExpiresAt":"2026-09-11T00:00:00Z"
		}`)
	}))
	defer mockBroker.Close()

	source := NewMockSource(extURL)
	approver, err := approvaladapter.NewBrokerHTTPClient(mockBroker.URL, "cluster-a", mockBroker.Client())
	if err != nil {
		t.Fatal(err)
	}

	notifier := &capturingNotifier{}

	// Create + approve an approval programmatically.
	code := mockExt.CreateApproval(CreateApprovalRequest{
		ApprovalCode:  "poll-test",
		ClusterID:     "cluster-a",
		InitiatorID:   "carol",
		TargetSubject: "carol",
		Reason:        "poll test",
		RequestedTTL:  1800,
	})
	if err := mockExt.ApproveApproval(code); err != nil {
		t.Fatal(err)
	}

	// Run Poll with a known code channel.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	codes := make(chan string, 1)
	codes <- code
	close(codes)

	// Use a short interval so the loop doesn't block.
	Poll(ctx, source, approver, notifier, 100*time.Millisecond, codes)

	// Verify the notification was delivered.
	if len(notifier.notifications) != 1 {
		t.Fatalf("notifications = %d, want 1", len(notifier.notifications))
	}
	if notifier.notifications[0].ApprovalReference != "r1.poll-test-ref" {
		t.Fatalf("notification reference = %q, want %q", notifier.notifications[0].ApprovalReference, "r1.poll-test-ref")
	}
}
