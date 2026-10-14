// Command mock-approval-server provides a deployable in-memory mock of an
// external approval system API for end-to-end testing.
//
// It implements the minimal HTTP API expected by an ApprovalSource:
//
//	POST /v1/approvals              — create an approval (returns instanceCode)
//	GET  /v1/approvals/{id}         — get approval status
//	POST /v1/approvals/{id}/approve — approve
//	POST /v1/approvals/{id}/reject  — reject
//	GET  /healthz                   — health check
//
// The server is stateless with respect to disk; all state lives in memory
// and is lost on restart. It generates a new instance code for each creation
// and stores approval metadata exactly as submitted.
//
// Use the optional SEALOS_MOCK_APPROVAL_SEED environment variable to pre-seed
// one or more approved approvals on startup. The format is a JSON array of
// CreateApprovalRequest objects. The server approves each seeded request
// automatically after creation.
//
// Usage:
//
//	mock-approval-server [-listen-address :8080]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
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

// approvalStore is a thread-safe in-memory store of approvals.
type approvalStore struct {
	mu        sync.RWMutex
	approvals map[string]*MockApproval
	nextCode  int
}

func newStore() *approvalStore {
	return &approvalStore{approvals: make(map[string]*MockApproval)}
}

func (s *approvalStore) create(req CreateApprovalRequest) string {
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

func (s *approvalStore) get(id string) (*MockApproval, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	app, ok := s.approvals[id]
	if !ok {
		return nil, false
	}
	copied := *app
	return &copied, true
}

func (s *approvalStore) approve(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	app, ok := s.approvals[id]
	if !ok {
		return fmt.Errorf("approval %q not found", id)
	}
	app.Status = StatusApproved
	app.UpdatedAt = time.Now().UTC()
	return nil
}

func (s *approvalStore) reject(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	app, ok := s.approvals[id]
	if !ok {
		return fmt.Errorf("approval %q not found", id)
	}
	app.Status = StatusRejected
	app.UpdatedAt = time.Now().UTC()
	return nil
}

// — HTTP handlers —

func handleCreate(store *approvalStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req CreateApprovalRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.ClusterID == "" || req.InitiatorID == "" || req.TargetSubject == "" {
			writeJSONError(w, http.StatusBadRequest, "clusterID, initiatorID, and targetSubject are required")
			return
		}
		instanceCode := store.create(req)
		writeJSON(w, http.StatusCreated, map[string]string{"instanceCode": instanceCode})
	}
}

func handleGet(store *approvalStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		app, ok := store.get(id)
		if !ok {
			writeJSONError(w, http.StatusNotFound, "approval not found")
			return
		}
		writeJSON(w, http.StatusOK, app)
	}
}

func handleApprove(store *approvalStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := store.approve(id); err != nil {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "approved"})
	}
}

func handleReject(store *approvalStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := store.reject(id); err != nil {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "rejected"})
	}
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"status":"ok"}`)
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

// seedPreApproved parses SEALOS_MOCK_APPROVAL_SEED and creates then auto-approves
// each entry. This lets CI/CD pipelines pre-populate approved approvals without
// an extra HTTP call.
func seedPreApproved(store *approvalStore) {
	raw := os.Getenv("SEALOS_MOCK_APPROVAL_SEED")
	if raw == "" {
		return
	}
	var requests []CreateApprovalRequest
	if err := json.Unmarshal([]byte(raw), &requests); err != nil {
		log.Printf("warning: SEALOS_MOCK_APPROVAL_SEED is not a valid JSON array: %v", err)
		return
	}
	for _, req := range requests {
		code := store.create(req)
		if err := store.approve(code); err != nil {
			log.Printf("warning: failed to auto-approve seed %q: %v", code, err)
		} else {
			log.Printf("seeded and approved %q for initiatorID=%q targetSubject=%q", code, req.InitiatorID, req.TargetSubject)
		}
	}
}

func main() {
	var listenAddress string
	flag.StringVar(&listenAddress, "listen-address", envOr("MOCK_APPROVAL_LISTEN_ADDRESS", ":8080"), "HTTP listen address.")
	flag.Parse()

	store := newStore()
	seedPreApproved(store)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/approvals", handleCreate(store))
	mux.HandleFunc("GET /v1/approvals/{id}", handleGet(store))
	mux.HandleFunc("POST /v1/approvals/{id}/approve", handleApprove(store))
	mux.HandleFunc("POST /v1/approvals/{id}/reject", handleReject(store))
	mux.HandleFunc("/healthz", handleHealthz)

	server := &http.Server{
		Addr:              listenAddress,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("mock approval server listening on %s", listenAddress)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
