package controllers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	userv1 "github.com/labring-sigs/sealos-access/api/v1"
	"github.com/labring-sigs/sealos-access/pkg/internalcredentials"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

type wiringManager struct {
	manager.Manager
	client    client.Client
	scheme    *runtime.Scheme
	recorder  record.EventRecorder
	added     []manager.Runnable
	addErr    error
	recorders []string
	cache     cache.Cache
}

func (m *wiringManager) GetClient() client.Client                { return m.client }
func (m *wiringManager) GetScheme() *runtime.Scheme              { return m.scheme }
func (m *wiringManager) GetCache() cache.Cache                   { return m.cache }
func (m *wiringManager) GetRESTMapper() meta.RESTMapper          { return nil }
func (m *wiringManager) GetLogger() logr.Logger                  { return logr.Discard() }
func (m *wiringManager) GetControllerOptions() config.Controller { return config.Controller{} }
func (m *wiringManager) GetEventRecorderFor(name string) record.EventRecorder {
	m.recorders = append(m.recorders, name)
	if m.recorder != nil {
		return m.recorder
	}
	return record.NewFakeRecorder(32)
}
func (m *wiringManager) Add(runnable manager.Runnable) error {
	if m.addErr != nil {
		return m.addErr
	}
	m.added = append(m.added, runnable)
	return nil
}

var _ ctrl.Manager = (*wiringManager)(nil)

func controllerTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := userv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func TestInternalUserSetupWithManagerDefaultsAndWatches(t *testing.T) {
	scheme := controllerTestScheme(t)
	mgr := &wiringManager{client: fake.NewClientBuilder().WithScheme(scheme).Build(), scheme: scheme, recorder: record.NewFakeRecorder(4)}
	reconciler := &InternalUserReconciler{}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	if reconciler.Client != mgr.client || reconciler.Scheme != scheme {
		t.Fatal("SetupWithManager did not initialize client and scheme")
	}
	if reconciler.UserNamespace != userv1.InternalUserSystemNamespace || reconciler.BaseRoleName != internalcredentials.BaseReadonlyRoleName {
		t.Fatalf("defaults = namespace %q role %q", reconciler.UserNamespace, reconciler.BaseRoleName)
	}
	if reconciler.Recorder == nil || len(mgr.recorders) != 1 || mgr.recorders[0] != internalUserControllerName {
		t.Fatalf("recorder initialization = recorder %v names %#v", reconciler.Recorder, mgr.recorders)
	}
	if len(mgr.added) != 1 {
		t.Fatalf("registered runnables = %d, want controller with InternalUser and managed-resource watches", len(mgr.added))
	}
}

func TestInternalUserMapManagedInternalUserFiltersTypesAndLabels(t *testing.T) {
	reconciler := &InternalUserReconciler{}
	validLabels := map[string]string{userv1.ManagedByLabel: internalUserControllerName, userv1.InternalUserLabel: "alice"}
	tests := []struct {
		name string
		obj  client.Object
		want types.NamespacedName
	}{
		{name: "service account", obj: &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Labels: validLabels}}, want: types.NamespacedName{Name: "alice"}},
		{name: "cluster role binding", obj: &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Labels: validLabels}}, want: types.NamespacedName{Name: "alice"}},
		{name: "missing label", obj: &corev1.ServiceAccount{}},
		{name: "wrong controller", obj: &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{userv1.ManagedByLabel: "other", userv1.InternalUserLabel: "alice"}}}},
		{name: "unrelated type", obj: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Labels: validLabels}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requests := reconciler.mapManagedInternalUser(context.Background(), tt.obj)
			if tt.want.Name == "" {
				if len(requests) != 0 {
					t.Fatalf("requests = %#v, want none", requests)
				}
				return
			}
			if len(requests) != 1 || requests[0].NamespacedName != tt.want {
				t.Fatalf("requests = %#v, want %#v", requests, tt.want)
			}
		})
	}
}

func TestCredentialLeaseSetupWithManagerDefaultsScannerPredicatesAndMapping(t *testing.T) {
	scheme := controllerTestScheme(t)
	mgr := &wiringManager{client: fake.NewClientBuilder().WithScheme(scheme).Build(), scheme: scheme, recorder: record.NewFakeRecorder(4)}
	reconciler := &CredentialLeaseReconciler{}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	if reconciler.UserNamespace != userv1.InternalUserSystemNamespace || reconciler.BrokerNamespace != userv1.CredentialBrokerNamespace ||
		reconciler.BaseRoleName != internalcredentials.BaseReadonlyRoleName || reconciler.ElevatedRoleName != internalcredentials.ClusterOpsWriteRoleName || reconciler.Now == nil {
		t.Fatalf("defaults = %#v", reconciler)
	}
	if reconciler.Recorder == nil || len(mgr.recorders) != 1 || mgr.recorders[0] != credentialLeaseControllerName {
		t.Fatalf("recorder initialization = recorder %v names %#v", reconciler.Recorder, mgr.recorders)
	}
	if len(mgr.added) != 2 {
		t.Fatalf("registered runnables = %d, want scanner and controller", len(mgr.added))
	}
	if _, ok := mgr.added[0].(*credentialLeaseScanner); !ok {
		t.Fatalf("first runnable = %T, want credentialLeaseScanner", mgr.added[0])
	}

	labels := map[string]string{userv1.ManagedByLabel: credentialLeaseControllerName, userv1.CredentialLeaseLabel: "lease-1"}
	for _, obj := range []client.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Labels: labels}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Labels: labels}},
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Labels: labels}},
	} {
		requests := reconciler.mapManagedLease(context.Background(), obj)
		if len(requests) != 1 || requests[0].Namespace != userv1.CredentialBrokerNamespace || requests[0].Name != "lease-1" {
			t.Fatalf("managed %T requests = %#v", obj, requests)
		}
	}
	if requests := reconciler.mapManagedLease(context.Background(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Labels: labels}}); len(requests) != 0 {
		t.Fatalf("wrong object type requests = %#v", requests)
	}
	if requests := reconciler.mapManagedLease(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{userv1.CredentialLeaseLabel: "lease-1"}}}); len(requests) != 0 {
		t.Fatalf("missing managed-by label requests = %#v", requests)
	}

	managedPredicate := credentialLeaseManagedResourcePredicate()
	managed := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "any", Labels: labels}}
	unmanaged := managed.DeepCopy()
	unmanaged.Labels[userv1.ManagedByLabel] = "other"
	if !managedPredicate.Create(event.CreateEvent{Object: managed}) || managedPredicate.Create(event.CreateEvent{Object: unmanaged}) {
		t.Fatal("managed resource predicate accepted the wrong label")
	}
	namespacePredicate := credentialLeaseNamespacePredicate(reconciler.BrokerNamespace)
	brokerLease := &userv1.CredentialLease{ObjectMeta: metav1.ObjectMeta{Namespace: reconciler.BrokerNamespace}}
	otherLease := brokerLease.DeepCopy()
	otherLease.Namespace = "other"
	if !namespacePredicate.Create(event.CreateEvent{Object: brokerLease}) || namespacePredicate.Create(event.CreateEvent{Object: otherLease}) {
		t.Fatal("lease namespace predicate accepted the wrong namespace")
	}
	if requests := credentialLeaseScanEventMapper(reconciler.BrokerNamespace)(context.Background(), brokerLease); len(requests) != 1 || requests[0].Namespace != reconciler.BrokerNamespace {
		t.Fatalf("scan event mapping = %#v", requests)
	}
	if requests := credentialLeaseScanEventMapper(reconciler.BrokerNamespace)(context.Background(), otherLease); len(requests) != 0 {
		t.Fatalf("other namespace scan event mapping = %#v", requests)
	}
}

func TestCredentialLeaseSetupWithManagerReturnsScannerRegistrationError(t *testing.T) {
	scheme := controllerTestScheme(t)
	wantErr := errors.New("scanner registration failed")
	mgr := &wiringManager{client: fake.NewClientBuilder().WithScheme(scheme).Build(), scheme: scheme, addErr: wantErr}
	err := (&CredentialLeaseReconciler{}).SetupWithManager(mgr)
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("SetupWithManager error = %v, want wrapped scanner error", err)
	}
	if len(mgr.added) != 0 {
		t.Fatalf("controller registered after scanner failure: %#v", mgr.added)
	}
}

func TestCredentialLeaseMapInternalUserToLeasesMatchesIdentityAndPreservesNamespace(t *testing.T) {
	scheme := controllerTestScheme(t)
	identity := userv1.Identity{Issuer: "https://issuer.example", Subject: "alice"}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&userv1.CredentialLease{ObjectMeta: metav1.ObjectMeta{Name: "matching", Namespace: "custom-broker-ns"}, Spec: userv1.CredentialLeaseSpec{Target: identity}},
		&userv1.CredentialLease{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "custom-broker-ns"}, Spec: userv1.CredentialLeaseSpec{Target: userv1.Identity{Issuer: identity.Issuer, Subject: "bob"}}},
	).Build()
	reconciler := &CredentialLeaseReconciler{Client: reader, BrokerNamespace: "custom-broker-ns"}
	requests := reconciler.mapInternalUserToLeases(context.Background(), &userv1.InternalUser{ObjectMeta: metav1.ObjectMeta{Name: "alice"}, Spec: userv1.InternalUserSpec{Identity: identity}})
	if len(requests) != 1 || requests[0].Namespace != "custom-broker-ns" || requests[0].Name != "matching" {
		t.Fatalf("matching requests = %#v", requests)
	}
	if requests := reconciler.mapInternalUserToLeases(context.Background(), &corev1.ConfigMap{}); len(requests) != 0 {
		t.Fatalf("wrong object type requests = %#v", requests)
	}
	listErr := errors.New("lease list failed")
	failing := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error { return listErr },
	}).Build()
	reconciler.Client = failing
	if requests := reconciler.mapInternalUserToLeases(context.Background(), &userv1.InternalUser{ObjectMeta: metav1.ObjectMeta{Name: "alice"}, Spec: userv1.InternalUserSpec{Identity: identity}}); len(requests) != 0 {
		t.Fatalf("list failure requests = %#v", requests)
	}
}

func TestCredentialLeaseScannerHandlesCancellationListFailureFullChannelAndDefaultIntervals(t *testing.T) {
	scheme := controllerTestScheme(t)
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&userv1.CredentialLease{ObjectMeta: metav1.ObjectMeta{Name: "first", Namespace: userv1.CredentialBrokerNamespace}},
		&userv1.CredentialLease{ObjectMeta: metav1.ObjectMeta{Name: "second", Namespace: userv1.CredentialBrokerNamespace}},
	).Build()

	t.Run("full channel exits on cancellation", func(t *testing.T) {
		events := make(chan event.GenericEvent, 1)
		ctx, cancel := context.WithCancel(context.Background())
		scanner := &credentialLeaseScanner{Client: reader, BrokerNamespace: userv1.CredentialBrokerNamespace, Events: events, Interval: time.Hour}
		done := make(chan error, 1)
		go func() { done <- scanner.Start(ctx) }()
		select {
		case <-events:
			cancel()
		case <-time.After(time.Second):
			cancel()
			t.Fatal("scanner did not emit first event")
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("scanner remained blocked on full event channel")
		}
	})

	t.Run("list failure emits no event", func(t *testing.T) {
		listErr := errors.New("list failed")
		failingReader := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error { return listErr },
		}).Build()
		events := make(chan event.GenericEvent, 1)
		scanner := &credentialLeaseScanner{Client: failingReader, BrokerNamespace: userv1.CredentialBrokerNamespace, Events: events, Interval: time.Hour}
		scanner.scan(context.Background())
		select {
		case event := <-events:
			t.Fatalf("event after list failure = %#v", event)
		default:
		}
	})

	for _, interval := range []time.Duration{0, -time.Second} {
		t.Run("default interval "+interval.String(), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			scanner := &credentialLeaseScanner{Client: reader, BrokerNamespace: userv1.CredentialBrokerNamespace, Events: make(chan event.GenericEvent, 4), Interval: interval}
			if err := scanner.Start(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}
