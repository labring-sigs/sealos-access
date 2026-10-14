package approvaladapter

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	userv1 "github.com/labring-sigs/sealos-access/api/v1"
	"github.com/labring-sigs/sealos-access/pkg/broker"
)

func TestPipelineUsesInjectedSourceAndNotifier(t *testing.T) {
	source := &fakeSource{
		instance: ApprovalInstance{
			InstanceCode:        "instance-1",
			ClusterID:           "cluster-a",
			Target:              userv1.Identity{Issuer: "https://issuer.example", Subject: "alice"},
			RequestedTTLSeconds: 1800,
			ApprovalReason:      "incident",
		},
		email: "alice@example.internal",
	}
	approver := &fakeApprover{result: broker.ApprovalResponse{
		ApprovalID: "instance-1", ApprovalReference: "r1.reference", Profile: userv1.ClusterOpsWriteProfile,
		ApprovalExpiresAt: time.Date(2026, 9, 11, 1, 0, 0, 0, time.UTC),
	}}
	notifier := &fakeNotifier{}
	pipeline, err := NewPipeline(source, approver, notifier)
	if err != nil {
		t.Fatal(err)
	}
	if err := pipeline.Process(context.Background(), "instance-1"); err != nil {
		t.Fatal(err)
	}
	if source.instanceCall != 1 || source.emailCall != 1 || len(approver.records) != 1 || len(notifier.notifications) != 1 {
		t.Fatalf("pipeline calls = source instance %d email %d approver %d notifier %d", source.instanceCall, source.emailCall, len(approver.records), len(notifier.notifications))
	}
	if approver.records[0].Target.Subject != "alice" || notifier.notifications[0].ApprovalReference != "r1.reference" {
		t.Fatalf("pipeline values = record %#v notification %#v", approver.records[0], notifier.notifications[0])
	}
}

func TestPipelineRejectsInvalidDependenciesAndBrokerReference(t *testing.T) {
	if _, err := NewPipeline(nil, &fakeApprover{}, &fakeNotifier{}); err == nil {
		t.Fatal("nil source was accepted")
	}
	if _, err := NewPipeline(&fakeSource{}, nil, &fakeNotifier{}); err == nil {
		t.Fatal("nil approver was accepted")
	}
	if _, err := NewPipeline(&fakeSource{}, &fakeApprover{}, nil); err == nil {
		t.Fatal("nil notifier was accepted")
	}

	pipeline, err := NewPipeline(&fakeSource{instance: ApprovalInstance{
		InstanceCode: "instance-1", ClusterID: "cluster-a", Target: userv1.Identity{Issuer: "https://issuer.example", Subject: "alice"},
		RequestedTTLSeconds: 1800, ApprovalReason: "incident",
	}, email: "alice@example.internal"}, &fakeApprover{result: broker.ApprovalResponse{
		ApprovalID: "instance-1", ApprovalReference: "bad reference", Profile: userv1.ClusterOpsWriteProfile,
	}}, &fakeNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	err = pipeline.Process(context.Background(), "instance-1")
	if err == nil || !strings.Contains(err.Error(), "invalid approval reference") {
		t.Fatalf("Process() error = %v, want invalid reference", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pipeline, err = NewPipeline(&fakeSource{err: errors.New("source should not run")}, &fakeApprover{}, &fakeNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	if err := pipeline.Process(ctx, "instance-1"); err == nil {
		t.Fatal("canceled pipeline unexpectedly succeeded")
	}
}
