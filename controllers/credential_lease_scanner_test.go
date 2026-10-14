package controllers

import (
	"context"
	"testing"
	"time"

	userv1 "github.com/labring-sigs/sealos-access/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func TestCredentialLeaseScannerEmitsBrokerLeases(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := userv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	watched := &userv1.CredentialLease{ObjectMeta: metav1.ObjectMeta{Name: "lease-scanned", Namespace: userv1.CredentialBrokerNamespace}}
	ignored := &userv1.CredentialLease{ObjectMeta: metav1.ObjectMeta{Name: "lease-ignored", Namespace: "other-namespace"}}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(watched, ignored).Build()
	events := make(chan event.GenericEvent, 1)
	scanner := &credentialLeaseScanner{Client: client.Reader(reader), BrokerNamespace: userv1.CredentialBrokerNamespace, Events: events, Interval: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = scanner.Start(ctx) }()
	select {
	case received := <-events:
		lease, ok := received.Object.(*userv1.CredentialLease)
		if !ok || lease.Name != watched.Name || lease.Namespace != watched.Namespace {
			t.Fatalf("scanner event = %#v, want %s/%s", received.Object, watched.Namespace, watched.Name)
		}
	case <-time.After(time.Second):
		t.Fatal("periodic scanner did not emit a lease")
	}
}

func TestCredentialLeaseScannerRechecksLeasesAfterMissedWatchEvent(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := userv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).Build()
	events := make(chan event.GenericEvent, 1)
	scanner := &credentialLeaseScanner{Client: client.Reader(reader), BrokerNamespace: userv1.CredentialBrokerNamespace, Events: events, Interval: time.Hour}
	ctx := context.Background()

	// The first scan represents a watch event that was missed before the
	// resource existed in the informer's view.
	scanner.scan(ctx)
	select {
	case event := <-events:
		t.Fatalf("unexpected event before lease creation: %#v", event.Object)
	default:
	}

	lease := &userv1.CredentialLease{ObjectMeta: metav1.ObjectMeta{Name: "lease-after-watch-loss", Namespace: userv1.CredentialBrokerNamespace}}
	if err := reader.Create(ctx, lease); err != nil {
		t.Fatal(err)
	}
	scanner.scan(ctx)
	select {
	case received := <-events:
		got, ok := received.Object.(*userv1.CredentialLease)
		if !ok || got.Name != lease.Name || got.Namespace != lease.Namespace {
			t.Fatalf("scanner event = %#v, want %s/%s", received.Object, lease.Namespace, lease.Name)
		}
	case <-time.After(time.Second):
		t.Fatal("periodic rescan did not recover the lease")
	}
}
