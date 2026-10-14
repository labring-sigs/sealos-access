package mock

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	userv1 "github.com/labring-sigs/sealos-access/api/v1"
	"github.com/labring-sigs/sealos-access/pkg/approvaladapter"
	"github.com/labring-sigs/sealos-access/pkg/broker"
)

func TestMockSourceRejectsNotFoundMalformedOversizedAndAPIErrorResponses(t *testing.T) {
	valid := `{"id":"instance-1","approvalCode":"code","status":"approved","initiatorID":"operator","targetSubject":"alice","requestedTTLSeconds":1800,"reason":"incident"}`
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "not found", status: http.StatusNotFound, body: `{"error":"missing"}`},
		{name: "malformed", status: http.StatusOK, body: `{"id":`},
		{name: "oversized", status: http.StatusOK, body: valid + strings.Repeat("x", 64*1024)},
		{name: "multiple values", status: http.StatusOK, body: valid + valid},
		{name: "API error", status: http.StatusInternalServerError, body: `{"error":"backend"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			if _, err := NewMockSource(server.URL).GetInstance(context.Background(), "instance-1"); err == nil {
				t.Fatal("invalid approval response was accepted")
			}
		})
	}
}

func TestMockProcessApprovalPropagatesSourceApproverNotifierErrorsAndCancellation(t *testing.T) {
	validInstance := approvaladapter.ApprovalInstance{
		InstanceCode:        "instance-1",
		ClusterID:           "cluster-a",
		Target:              userv1.Identity{Issuer: "https://issuer.example", Subject: "alice"},
		RequestedTTLSeconds: 1800,
		ApprovalReason:      "incident",
	}
	for _, test := range []struct {
		name        string
		sourceErr   error
		emailErr    error
		approverErr error
		notifierErr error
		cancel      bool
	}{
		{name: "source error", sourceErr: errors.New("source unavailable")},
		{name: "email error", emailErr: errors.New("directory unavailable")},
		{name: "approver error", approverErr: errors.New("broker unavailable")},
		{name: "notifier error", notifierErr: errors.New("delivery unavailable")},
		{name: "canceled context", cancel: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := &p1Source{instance: validInstance, instanceErr: test.sourceErr, emailErr: test.emailErr, respectContext: test.cancel}
			approver := &p1Approver{response: broker.ApprovalResponse{ApprovalID: "instance-1", ApprovalReference: "r1.reference", Profile: userv1.ClusterOpsWriteProfile}, err: test.approverErr}
			notifier := &p1Notifier{err: test.notifierErr}
			ctx := context.Background()
			if test.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if err := processApproval(ctx, source, approver, notifier, "instance-1"); err == nil {
				t.Fatal("processApproval unexpectedly succeeded")
			}
			if test.sourceErr != nil || test.cancel || test.emailErr != nil {
				if approver.calls != 0 {
					t.Fatalf("approver calls = %d, want 0", approver.calls)
				}
			}
			if test.approverErr != nil && notifier.calls != 0 {
				t.Fatalf("notifier calls = %d, want 0", notifier.calls)
			}
		})
	}
}

func TestMockPollHandlesClosedEmptyChannelsAndRetriesRepeatedApprovalCodes(t *testing.T) {
	source := &p1Source{instance: approvaladapter.ApprovalInstance{
		InstanceCode:        "instance-1",
		ClusterID:           "cluster-a",
		Target:              userv1.Identity{Issuer: "https://issuer.example", Subject: "alice"},
		RequestedTTLSeconds: 1800,
		ApprovalReason:      "incident",
	}, email: "alice@example.internal"}
	approver := &p1Approver{response: broker.ApprovalResponse{ApprovalID: "instance-1", ApprovalReference: "r1.reference", Profile: userv1.ClusterOpsWriteProfile}}
	notifier := &p1Notifier{failures: 1}
	codes := make(chan string, 2)
	codes <- "instance-1"
	codes <- "instance-1"
	close(codes)
	Poll(context.Background(), source, approver, notifier, 0, codes)
	if approver.calls != 2 || notifier.calls != 2 || source.instanceCalls != 2 {
		t.Fatalf("retry counts = source %d approver %d notifier %d, want 2/2/2", source.instanceCalls, approver.calls, notifier.calls)
	}

	closed := make(chan string)
	close(closed)
	start := time.Now()
	Poll(context.Background(), source, approver, notifier, time.Hour, closed)
	if time.Since(start) > time.Second {
		t.Fatal("Poll did not stop when its channel closed")
	}

	empty := make(chan string)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	Poll(ctx, source, approver, notifier, time.Hour, empty)
}

func TestMockPollCancellationStopsAnInFlightApproval(t *testing.T) {
	source := &p1Source{waitForContext: true}
	approver := &p1Approver{}
	notifier := &p1Notifier{}
	codes := make(chan string, 1)
	codes <- "instance-1"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	Poll(ctx, source, approver, notifier, time.Hour, codes)
	if approver.calls != 0 || notifier.calls != 0 {
		t.Fatalf("in-flight cancellation reached downstream: approver %d notifier %d", approver.calls, notifier.calls)
	}
}

type p1Source struct {
	instance       approvaladapter.ApprovalInstance
	instanceErr    error
	email          string
	emailErr       error
	respectContext bool
	waitForContext bool
	instanceCalls  int
}

func (s *p1Source) GetInstance(ctx context.Context, _ string) (approvaladapter.ApprovalInstance, error) {
	s.instanceCalls++
	if s.waitForContext {
		<-ctx.Done()
		return approvaladapter.ApprovalInstance{}, ctx.Err()
	}
	if s.respectContext {
		select {
		case <-ctx.Done():
			return approvaladapter.ApprovalInstance{}, ctx.Err()
		default:
		}
	}
	if s.instanceErr != nil {
		return approvaladapter.ApprovalInstance{}, s.instanceErr
	}
	return s.instance, nil
}

func (s *p1Source) GetUserEmail(ctx context.Context, _ string) (string, error) {
	if s.respectContext {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
	}
	if s.emailErr != nil {
		return "", s.emailErr
	}
	if s.email != "" {
		return s.email, nil
	}
	return "alice@example.internal", nil
}

type p1Approver struct {
	response broker.ApprovalResponse
	err      error
	calls    int
	mu       sync.Mutex
}

func (a *p1Approver) Approve(_ context.Context, _ broker.ApprovalRecord) (broker.ApprovalResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if a.err != nil {
		return broker.ApprovalResponse{}, a.err
	}
	return a.response, nil
}

type p1Notifier struct {
	err      error
	failures int
	calls    int
	mu       sync.Mutex
}

func (n *p1Notifier) Notify(_ context.Context, _ approvaladapter.Notification) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls++
	if n.failures > 0 {
		n.failures--
		return errors.New("transient notification failure")
	}
	return n.err
}
