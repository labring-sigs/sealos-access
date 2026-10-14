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

package approvaladapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/labring-sigs/sealos-access/pkg/broker"
)

// BrokerHTTPClient submits an already-approved record to the Broker's
// dedicated approval endpoint. It does not expose a generic Broker API client.
type BrokerHTTPClient struct {
	Endpoint   string
	ClusterID  string
	HTTPClient *http.Client
}

func NewBrokerHTTPClient(endpoint, clusterID string, httpClient *http.Client) (*BrokerHTTPClient, error) {
	if err := broker.ValidateTargetID(clusterID); err != nil {
		return nil, fmt.Errorf("invalid target cluster: %w", err)
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" || strings.ContainsAny(endpoint, "\r\n") {
		return nil, errors.New("Broker endpoint must be an HTTPS URL")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	path := "/v1/clusters/" + url.PathEscape(clusterID) + "/internal/credentials/approve"
	return &BrokerHTTPClient{Endpoint: strings.TrimRight(endpoint, "/") + path, ClusterID: clusterID, HTTPClient: httpClient}, nil
}

func (c *BrokerHTTPClient) Approve(ctx context.Context, record broker.ApprovalRecord) (broker.ApprovalResponse, error) {
	if c == nil || c.HTTPClient == nil || c.Endpoint == "" {
		return broker.ApprovalResponse{}, errors.New("Broker HTTP client is not configured")
	}
	if record.ClusterID != c.ClusterID {
		return broker.ApprovalResponse{}, errors.New("approval target cluster does not match Broker client")
	}
	body, err := json.Marshal(record)
	if err != nil {
		return broker.ApprovalResponse{}, fmt.Errorf("encode Broker approval record: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return broker.ApprovalResponse{}, fmt.Errorf("create Broker approval request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	response, err := c.HTTPClient.Do(request)
	if err != nil {
		return broker.ApprovalResponse{}, fmt.Errorf("submit Broker approval record: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return broker.ApprovalResponse{}, fmt.Errorf("Broker returned HTTP %d", response.StatusCode)
	}
	var result broker.ApprovalResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return broker.ApprovalResponse{}, fmt.Errorf("decode Broker approval response: %w", err)
	}
	if result.ClusterID != c.ClusterID {
		return broker.ApprovalResponse{}, errors.New("Broker approval response target cluster does not match Broker client")
	}
	return result, nil
}
