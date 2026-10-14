package approvaladapter

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/labring-sigs/sealos-access/pkg/broker"
)

type fakeSource struct {
	instance     ApprovalInstance
	email        string
	instanceCall int
	emailCall    int
	err          error
}

func (s *fakeSource) GetInstance(context.Context, string) (ApprovalInstance, error) {
	s.instanceCall++
	if s.err != nil {
		return ApprovalInstance{}, s.err
	}
	return s.instance, nil
}

func (s *fakeSource) GetUserEmail(context.Context, string) (string, error) {
	s.emailCall++
	if s.err != nil {
		return "", s.err
	}
	return s.email, nil
}

type fakeApprover struct {
	records []broker.ApprovalRecord
	result  broker.ApprovalResponse
	err     error
}

func (a *fakeApprover) Approve(_ context.Context, record broker.ApprovalRecord) (broker.ApprovalResponse, error) {
	a.records = append(a.records, record)
	if a.err != nil {
		return broker.ApprovalResponse{}, a.err
	}
	return a.result, nil
}

type fakeNotifier struct {
	notifications []Notification
	err           error
}

func (n *fakeNotifier) Notify(_ context.Context, notification Notification) error {
	n.notifications = append(n.notifications, notification)
	return n.err
}

func TestValidReference(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{"valid reference", "r1.approval-test.reference", true},
		{"empty", "", false},
		{"has whitespace", "r1 ref", false},
		{"too long", strings.Repeat("a", 513), false},
		{"leading space", " ref", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validReference(tt.value); got != tt.want {
				t.Errorf("validReference(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

func TestNotifierContractSupportsCancellationDeliveryErrorsRetriesAndReferenceOnlyMetadata(t *testing.T) {
	notification := Notification{
		Recipient:         "alice@example.internal",
		ApprovalID:        "approval-1",
		ApprovalReference: "r1.reference",
		Profile:           "cluster-ops-write.v1",
	}

	t.Run("cancellation is propagated", func(t *testing.T) {
		notifier := &contractNotifier{respectContext: true}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := notifier.Notify(ctx, notification); !errors.Is(err, context.Canceled) {
			t.Fatalf("Notify() error = %v, want context.Canceled", err)
		}
		if len(notifier.notifications) != 0 {
			t.Fatalf("canceled notification was recorded: %#v", notifier.notifications)
		}
	})

	t.Run("delivery error is returned", func(t *testing.T) {
		wantErr := errors.New("delivery unavailable")
		notifier := &contractNotifier{err: wantErr}
		if err := notifier.Notify(context.Background(), notification); !errors.Is(err, wantErr) {
			t.Fatalf("Notify() error = %v, want %v", err, wantErr)
		}
	})

	t.Run("retry keeps reference-only metadata", func(t *testing.T) {
		notifier := &contractNotifier{failures: 1}
		if err := notifier.Notify(context.Background(), notification); err == nil {
			t.Fatal("first delivery unexpectedly succeeded")
		}
		if err := notifier.Notify(context.Background(), notification); err != nil {
			t.Fatal(err)
		}
		if len(notifier.notifications) != 1 || notifier.notifications[0] != notification {
			t.Fatalf("notifications = %#v, want one reference-only retry result", notifier.notifications)
		}
		encoded, err := json.Marshal(notifier.notifications[0])
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"token", "kubeconfig", "secretData", "privateKey"} {
			if strings.Contains(strings.ToLower(string(encoded)), strings.ToLower(secret)) {
				t.Fatalf("notification contains credential field %q: %s", secret, encoded)
			}
		}
	})
}

type contractNotifier struct {
	notifications  []Notification
	err            error
	failures       int
	respectContext bool
}

func (n *contractNotifier) Notify(ctx context.Context, notification Notification) error {
	if n.respectContext {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	if n.failures > 0 {
		n.failures--
		return errors.New("transient delivery failure")
	}
	if n.err != nil {
		return n.err
	}
	n.notifications = append(n.notifications, notification)
	return nil
}
