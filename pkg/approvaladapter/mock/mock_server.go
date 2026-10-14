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

// Package mock provides a mock external approval system and a mock
// ApprovalSource for testing the approval adapter interfaces end-to-end.
//
// Use MockServer to simulate an external approval API, then wire
// MockSource (or a custom source) with approvaladapter.BrokerHTTPClient
// and any approvaladapter.Notifier implementation to exercise the full
// approval flow without a real external system or notification service.
package mock

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// ApprovalStatus represents the lifecycle of a mock approval.
type ApprovalStatus string

const (
	StatusPending  ApprovalStatus = "pending"
	StatusApproved ApprovalStatus = "approved"
	StatusRejected ApprovalStatus = "rejected"
)

// MockApproval is the data model stored by the mock external approval system.
type MockApproval struct {
	ID             string         `json:"id"`
	ApprovalCode   string         `json:"approvalCode"`
	ClusterID      string         `json:"clusterID"`
	Status         ApprovalStatus `json:"status"`
	InitiatorID    string         `json:"initiatorID"`
	InitiatorEmail string         `json:"initiatorEmail"`
	Form           map[string]any `json:"form"`
	RequestedTTL   int64          `json:"requestedTTLSeconds"`
	Reason         string         `json:"reason"`
	TargetSubject  string         `json:"targetSubject"`
	CreatedAt      time.Time      `json:"createdAt"`
	UpdatedAt      time.Time      `json:"updatedAt"`
}

// CreateApprovalRequest is the request body for creating a mock approval.
type CreateApprovalRequest struct {
	ApprovalCode   string         `json:"approvalCode"`
	ClusterID      string         `json:"clusterID"`
	InitiatorID    string         `json:"initiatorID"`
	InitiatorEmail string         `json:"initiatorEmail"`
	Form           map[string]any `json:"form"`
	TargetSubject  string         `json:"targetSubject"`
	Reason         string         `json:"reason"`
	RequestedTTL   int64          `json:"requestedTTLSeconds"`
}

// MockServer is an in-memory HTTP server that simulates an external approval
// system. Create one with NewMockServer, start it with Start(), and interact
// with it via HTTP or the direct CreateApproval/ApproveApproval methods.
type MockServer struct {
	mu        sync.RWMutex
	approvals map[string]*MockApproval
	nextCode  int
	server    *httptest.Server
}

// NewMockServer creates an unstarted MockServer.
func NewMockServer() *MockServer {
	return &MockServer{approvals: make(map[string]*MockApproval)}
}

// Start starts the mock server and returns the base URL.
func (s *MockServer) Start() string {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/approvals", s.handleCreate)
	mux.HandleFunc("GET /v1/approvals/{id}", s.handleGet)
	mux.HandleFunc("POST /v1/approvals/{id}/approve", s.handleApprove)
	mux.HandleFunc("POST /v1/approvals/{id}/reject", s.handleReject)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	s.server = httptest.NewServer(mux)
	return s.server.URL
}

// Close shuts down the mock server.
func (s *MockServer) Close() {
	if s.server != nil {
		s.server.Close()
	}
}

// URL returns the base URL of the running server, or empty if not started.
func (s *MockServer) URL() string {
	if s.server == nil {
		return ""
	}
	return s.server.URL
}

// CreateApproval creates a mock approval programmatically (without HTTP).
// Returns the generated instance code.
func (s *MockServer) CreateApproval(req CreateApprovalRequest) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	code := fmt.Sprintf("mock-instance-%04d", s.nextCode)
	s.nextCode++
	now := time.Now().UTC()
	s.approvals[code] = &MockApproval{
		ID:             code,
		ApprovalCode:   req.ApprovalCode,
		ClusterID:      req.ClusterID,
		Status:         StatusPending,
		InitiatorID:    req.InitiatorID,
		InitiatorEmail: req.InitiatorEmail,
		Form:           req.Form,
		RequestedTTL:   req.RequestedTTL,
		Reason:         req.Reason,
		TargetSubject:  req.TargetSubject,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	return code
}

// ApproveApproval transitions an existing approval to approved.
func (s *MockServer) ApproveApproval(instanceCode string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	app, ok := s.approvals[instanceCode]
	if !ok {
		return fmt.Errorf("approval %q not found", instanceCode)
	}
	app.Status = StatusApproved
	app.UpdatedAt = time.Now().UTC()
	return nil
}

// GetApproval returns a copy of the stored approval by instance code.
func (s *MockServer) GetApproval(instanceCode string) (*MockApproval, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	app, ok := s.approvals[instanceCode]
	if !ok {
		return nil, false
	}
	copied := *app
	return &copied, true
}

// ListApprovalsByStatus returns all approvals matching the given status.
func (s *MockServer) ListApprovalsByStatus(status ApprovalStatus) []*MockApproval {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*MockApproval
	for _, app := range s.approvals {
		if app.Status == status {
			copied := *app
			result = append(result, &copied)
		}
	}
	return result
}

// — HTTP handlers —

func (s *MockServer) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req CreateApprovalRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ClusterID == "" || req.InitiatorID == "" || req.TargetSubject == "" {
		writeJSONError(w, http.StatusBadRequest, "clusterID, initiatorID, and targetSubject are required")
		return
	}
	instanceCode := s.CreateApproval(req)
	_, _ = io.WriteString(w, fmt.Sprintf(`{"instanceCode":"%s"}`, instanceCode))
}

func (s *MockServer) handleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.RLock()
	app, ok := s.approvals[id]
	s.mu.RUnlock()
	if !ok {
		writeJSONError(w, http.StatusNotFound, "approval not found")
		return
	}
	writeJSON(w, http.StatusOK, app)
}

func (s *MockServer) handleApprove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.ApproveApproval(id); err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "approved"})
}

func (s *MockServer) handleReject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	app, ok := s.approvals[id]
	if !ok {
		s.mu.Unlock()
		writeJSONError(w, http.StatusNotFound, "approval not found")
		return
	}
	app.Status = StatusRejected
	app.UpdatedAt = time.Now().UTC()
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"status": "rejected"})
}

// — helpers —

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// ApprovalEvent is what the mock server returns when a new approval is
// created or updated — it simulates the webhook event or poll result that
// an ApprovalSource would receive.
type ApprovalEvent struct {
	InstanceCode string `json:"instanceCode"`
	EventType    string `json:"eventType"`
	Status       string `json:"status"`
}

// String ensures ApprovalEvent fits the expected interface.
func (e ApprovalEvent) String() string {
	data, _ := json.Marshal(e)
	return strings.TrimSpace(strings.ReplaceAll(string(data), "\\", ""))
}
