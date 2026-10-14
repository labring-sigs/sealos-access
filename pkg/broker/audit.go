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
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"

	userv1 "github.com/labring-sigs/sealos-access/api/v1"
)

// LogAuditSink writes one JSON event per line. The container runtime or a
// configured log collector is responsible for shipping stderr centrally.
type LogAuditSink struct {
	Writer io.Writer
	mu     sync.Mutex
}

func (s *LogAuditSink) Record(ctx context.Context, event AuditEvent) error {
	if s == nil || s.Writer == nil {
		return errors.New("audit writer is not configured")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	for len(data) > 0 {
		written, err := s.Writer.Write(data)
		if err != nil {
			return err
		}
		if written <= 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

type auditEventContextKey struct{}

func withAuditEvent(ctx context.Context, event *AuditEvent) context.Context {
	return context.WithValue(ctx, auditEventContextKey{}, event)
}

func auditEventFromContext(ctx context.Context) *AuditEvent {
	event, _ := ctx.Value(auditEventContextKey{}).(*AuditEvent)
	return event
}

func setAuditRequester(ctx context.Context, identity userv1.Identity) {
	if event := auditEventFromContext(ctx); event != nil {
		event.Requester = identityPointer(identity)
	}
}

func setAuditOperation(ctx context.Context, operation string) {
	if event := auditEventFromContext(ctx); event != nil {
		event.Operation = operation
	}
}

func setAuditProfile(ctx context.Context, profile string) {
	if event := auditEventFromContext(ctx); event != nil {
		event.Profile = profile
	}
}

func setAuditApprovalReference(ctx context.Context, reference string) {
	if event := auditEventFromContext(ctx); event != nil {
		event.ApprovalReference = reference
	}
}

func setAuditTarget(ctx context.Context, identity userv1.Identity) {
	if event := auditEventFromContext(ctx); event != nil {
		event.Target = identityPointer(identity)
	}
}

func setAuditLeaseID(ctx context.Context, leaseID string) {
	if event := auditEventFromContext(ctx); event != nil {
		event.LeaseID = leaseID
	}
}

func identityPointer(identity userv1.Identity) *userv1.Identity {
	copy := identity
	return &copy
}

func auditResult(status int) string {
	switch {
	case status >= http.StatusOK && status < http.StatusMultipleChoices:
		return "success"
	case status >= http.StatusBadRequest && status < http.StatusInternalServerError:
		return "rejected"
	default:
		return "error"
	}
}

func truncateAuditValue(value string, maxBytes int) string {
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	return value[:maxBytes]
}

// bufferedResponseWriter lets the Broker withhold a response until its audit
// event has been accepted. Broker endpoints return finite JSON responses and
// do not need streaming response support.
type bufferedResponseWriter struct {
	header http.Header
	body   []byte
	status int
}

func newBufferedResponseWriter() *bufferedResponseWriter {
	return &bufferedResponseWriter{header: make(http.Header)}
}

func (w *bufferedResponseWriter) Header() http.Header {
	return w.header
}

func (w *bufferedResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *bufferedResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	w.body = append(w.body, data...)
	return len(data), nil
}

func (w *bufferedResponseWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func (w *bufferedResponseWriter) commit(dst http.ResponseWriter) {
	for key, values := range w.header {
		for _, value := range values {
			dst.Header().Add(key, value)
		}
	}
	dst.WriteHeader(w.statusCode())
	_, _ = dst.Write(w.body)
}
