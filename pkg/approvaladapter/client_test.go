package approvaladapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	userv1 "github.com/labring-sigs/sealos-access/api/v1"
	"github.com/labring-sigs/sealos-access/pkg/broker"
)

func TestBrokerHTTPClientSubmitsOnlyApprovalRecord(t *testing.T) {
	const clusterID = "cluster-a"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/clusters/cluster-a/internal/credentials/approve" || r.Method != http.MethodPost {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"token", "kubeconfig", "secretData", "privateKey"} {
			if strings.Contains(strings.ToLower(string(body)), strings.ToLower(forbidden)) {
				t.Fatalf("request contains forbidden field %q: %s", forbidden, body)
			}
		}
		var record broker.ApprovalRecord
		if err := json.Unmarshal(body, &record); err != nil {
			t.Fatal(err)
		}
		if record.Target.Subject != "alice" || record.RequestedTTLSeconds != 1800 || record.ApprovalReason != "incident" {
			t.Fatalf("record = %#v", record)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"clusterID":"cluster-a","approvalID":"instance-1","approvalReference":"r1.reference","profile":"cluster-ops-write.v1","requestedTTLSeconds":1800,"approvalExpiresAt":"2026-09-10T00:00:00Z"}`)
	}))
	defer server.Close()
	client, err := NewBrokerHTTPClient(server.URL, clusterID, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Approve(context.Background(), broker.ApprovalRecord{
		ClusterID: clusterID, ApprovalID: "instance-1", Target: userv1.Identity{Issuer: "https://issuer.example", Subject: "alice"},
		RequestedTTLSeconds: 1800, ApprovalReason: "incident",
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.ApprovalReference != "r1.reference" || response.Profile != userv1.ClusterOpsWriteProfile {
		t.Fatalf("response = %#v", response)
	}
}

func TestBrokerHTTPClientRejectsInsecureEndpoint(t *testing.T) {
	if _, err := NewBrokerHTTPClient("http://broker.example", "cluster-a", nil); err == nil {
		t.Fatal("insecure Broker endpoint was accepted")
	}
	if _, err := NewBrokerHTTPClient("https://broker.example/path?x=1", "cluster-a", nil); err == nil {
		t.Fatal("Broker endpoint with query was accepted")
	}
}

func TestBrokerHTTPClientAcceptsCreatedResponse(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/clusters/cluster-a/internal/credentials/approve" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"clusterID":"cluster-a","approvalID":"instance-created","approvalReference":"r1.created-reference","profile":"cluster-ops-write.v1","requestedTTLSeconds":1800,"approvalExpiresAt":"2026-09-10T01:00:00Z"}`)
	}))
	defer server.Close()

	client, err := NewBrokerHTTPClient(server.URL, "cluster-a", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Approve(context.Background(), broker.ApprovalRecord{
		ClusterID:           "cluster-a",
		ApprovalID:          "instance-created",
		Target:              userv1.Identity{Issuer: "https://issuer.example", Subject: "alice"},
		RequestedTTLSeconds: 1800,
		ApprovalReason:      "created response",
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.ApprovalID != "instance-created" || response.ApprovalReference != "r1.created-reference" {
		t.Fatalf("response = %#v", response)
	}
}

func TestBrokerHTTPClientRejectsInvalidResponses(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "malformed JSON", body: `{"approvalID":`},
		{name: "unknown field", body: `{"approvalID":"id","unknown":"value"}`},
		{name: "oversized response", body: strings.Repeat("x", 64*1024+1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tt.body)
			}))
			defer server.Close()
			client, err := NewBrokerHTTPClient(server.URL, "cluster-a", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Approve(context.Background(), validBrokerApprovalRecord()); err == nil {
				t.Fatal("invalid response was accepted")
			}
		})
	}
}

func TestBrokerHTTPClientPropagatesCancellationAndTransportErrors(t *testing.T) {
	// NewBrokerHTTPClient deliberately requires HTTPS, so use a TLS server for
	// the cancellation case while keeping the handler behavior identical.
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer tlsServer.Close()
	client, err := NewBrokerHTTPClient(tlsServer.URL, "cluster-a", tlsServer.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Approve(ctx, validBrokerApprovalRecord()); err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled approval error = %v, want context.Canceled", err)
	}

	transportError := errors.New("transport unavailable")
	transportClient, err := NewBrokerHTTPClient("https://broker.example", "cluster-a", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, transportError
	})})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transportClient.Approve(context.Background(), validBrokerApprovalRecord()); err == nil || !errors.Is(err, transportError) {
		t.Fatalf("transport error = %v, want wrapped transport error", err)
	}
}

func validBrokerApprovalRecord() broker.ApprovalRecord {
	return broker.ApprovalRecord{
		ClusterID:           "cluster-a",
		ApprovalID:          "instance-1",
		Target:              userv1.Identity{Issuer: "https://issuer.example", Subject: "alice"},
		RequestedTTLSeconds: 1800,
		ApprovalReason:      "test approval",
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
