//go:build acceptance

package acceptance

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/labring-sigs/sealos-access/pkg/approvaladapter"
)

func TestNotificationSinkRequiresBearerAndStoresReferenceOnly(t *testing.T) {
	accessToken := strings.Repeat("a", 48)
	sink, err := NewNotificationSink(accessToken)
	if err != nil {
		t.Fatal(err)
	}
	notification := approvaladapter.Notification{
		Recipient:         "alice@example.internal",
		ApprovalID:        "approval-1",
		ApprovalReference: "r1.reference",
		Profile:           "cluster-ops-write.v1",
		ExpiresAt:         time.Date(2026, 9, 11, 1, 0, 0, 0, time.UTC),
	}
	if err := sink.Notify(context.Background(), notification); err != nil {
		t.Fatal(err)
	}
	if _, err := sink.WaitForNotification(context.Background(), "wrong-token"); err != ErrUnauthorized {
		t.Fatalf("wrong token error = %v, want ErrUnauthorized", err)
	}
	got, err := sink.WaitForNotification(context.Background(), accessToken)
	if err != nil {
		t.Fatal(err)
	}
	if got != notification {
		t.Fatalf("notification = %#v, want %#v", got, notification)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"token", "kubeconfig", "secretData", "privateKey"} {
		if strings.Contains(strings.ToLower(string(encoded)), strings.ToLower(forbidden)) {
			t.Fatalf("notification contains forbidden field %q: %s", forbidden, encoded)
		}
	}
}

func TestNotificationSinkRejectsWeakTokenInvalidNotificationAndCancellation(t *testing.T) {
	if _, err := NewNotificationSink(strings.Repeat("a", 31)); err == nil {
		t.Fatal("weak access token was accepted")
	}
	sink, err := NewNotificationSink(strings.Repeat("b", 32))
	if err != nil {
		t.Fatal(err)
	}
	invalid := approvaladapter.Notification{ApprovalID: "approval-1", ApprovalReference: "r1.reference", Profile: "cluster-ops-write.v1"}
	if err := sink.Notify(context.Background(), invalid); err != ErrInvalid {
		t.Fatalf("invalid notification error = %v, want ErrInvalid", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sink.Notify(ctx, approvaladapter.Notification{
		Recipient: "alice@example.internal", ApprovalID: "approval-1", ApprovalReference: "r1.reference",
		Profile: "cluster-ops-write.v1", ExpiresAt: time.Now(),
	}); err == nil {
		t.Fatal("canceled notification unexpectedly succeeded")
	}
}
