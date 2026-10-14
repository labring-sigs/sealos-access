package broker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	userv1 "github.com/labring-sigs/sealos-access/api/v1"
)

func TestLogAuditSinkWritesAtomicNonSecretEvent(t *testing.T) {
	var output bytes.Buffer
	sink := &LogAuditSink{Writer: &output}
	event := AuditEvent{
		Operation:         "issue_base",
		Requester:         &userv1.Identity{Issuer: "https://issuer.example", Subject: "alice"},
		ApprovalReference: "reference-only",
		Result:            "success",
		StatusCode:        200,
	}
	if err := sink.Record(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	encoded := output.String()
	if !strings.HasSuffix(encoded, "\n") || !strings.Contains(encoded, `"operation":"issue_base"`) || strings.Contains(encoded, "token") || strings.Contains(encoded, "kubeconfig") {
		t.Fatalf("audit output = %q", encoded)
	}

	var concurrent bytes.Buffer
	concurrentSink := &LogAuditSink{Writer: &concurrent}
	var group sync.WaitGroup
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := concurrentSink.Record(context.Background(), event); err != nil {
				t.Errorf("concurrent Record() error = %v", err)
			}
		}()
	}
	group.Wait()
	if lines := strings.Count(concurrent.String(), "\n"); lines != 20 {
		t.Fatalf("audit line count = %d, want 20", lines)
	}
}

func TestLogAuditSinkHandlesWriterAndContextFailures(t *testing.T) {
	if err := (&LogAuditSink{}).Record(context.Background(), AuditEvent{}); err == nil {
		t.Fatal("nil writer was accepted")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (&LogAuditSink{Writer: io.Discard}).Record(canceled, AuditEvent{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Record() error = %v, want context.Canceled", err)
	}
	if err := (&LogAuditSink{Writer: zeroWriter{}}).Record(context.Background(), AuditEvent{}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("zero-writer Record() error = %v, want io.ErrShortWrite", err)
	}
	if err := (&LogAuditSink{Writer: failingWriter{}}).Record(context.Background(), AuditEvent{}); !errors.Is(err, errAuditWriter) {
		t.Fatalf("failing-writer Record() error = %v, want writer error", err)
	}
}

var errAuditWriter = errors.New("audit writer failed")

type zeroWriter struct{}

func (zeroWriter) Write([]byte) (int, error) { return 0, nil }

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errAuditWriter }
