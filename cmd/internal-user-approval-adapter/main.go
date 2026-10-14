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

// Command internal-user-approval-adapter is a skeleton entry point for
// implementing a custom external approval adapter. It wires the reusable
// BrokerHTTPClient (approval submitter), then starts an HTTPS server that
// exposes a /healthz endpoint.
//
// To build a functioning adapter, implement approvaladapter.ApprovalSource
// (GetInstance and GetUserEmail), choose an approvaladapter.Notifier, and
// inject both into the run() function below.
// The ApprovalSource is responsible for polling or receiving events from
// the external approval system, fetching authoritative approval data, and
// resolving user email addresses.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/labring-sigs/sealos-access/pkg/approvaladapter"
	"github.com/labring-sigs/sealos-access/pkg/broker"
)

type config struct {
	ListenAddress string
	TLSCertFile   string
	TLSKeyFile    string

	BrokerURL        string
	BrokerClusterID  string
	BrokerCAFile     string
	BrokerServerName string
	BearerToken      string
	BearerTokenFile  string
}

// dependencies are deliberately supplied by the concrete adapter
// integration. The command has no default ApprovalSource or Notifier.
type dependencies struct {
	Source        approvaladapter.ApprovalSource
	Notifier      approvaladapter.Notifier
	InstanceCodes <-chan string
}

func main() {
	config := config{
		ListenAddress:    envOr("INTERNAL_USER_APPROVAL_ADAPTER_LISTEN_ADDRESS", ":8443"),
		BrokerURL:        os.Getenv("BROKER_URL"),
		BrokerClusterID:  os.Getenv("BROKER_CLUSTER_ID"),
		BrokerCAFile:     os.Getenv("BROKER_CA_FILE"),
		BrokerServerName: os.Getenv("BROKER_SERVER_NAME"),
		BearerToken:      os.Getenv("BROKER_BEARER_TOKEN"),
		BearerTokenFile:  os.Getenv("BROKER_BEARER_TOKEN_FILE"),
	}
	flags := flag.NewFlagSet("internal-user-approval-adapter", flag.ExitOnError)
	flags.StringVar(&config.ListenAddress, "listen-address", config.ListenAddress, "HTTPS listen address.")
	flags.StringVar(&config.TLSCertFile, "tls-cert-file", os.Getenv("ADAPTER_TLS_CERT_FILE"), "HTTPS server certificate file.")
	flags.StringVar(&config.TLSKeyFile, "tls-key-file", os.Getenv("ADAPTER_TLS_KEY_FILE"), "HTTPS server private key file.")
	flags.StringVar(&config.BrokerURL, "broker-url", config.BrokerURL, "HTTPS URL of the internal credential Broker.")
	flags.StringVar(&config.BrokerClusterID, "cluster-id", config.BrokerClusterID, "Immutable target Kubernetes cluster ID.")
	flags.StringVar(&config.BrokerCAFile, "broker-ca-file", config.BrokerCAFile, "CA file used to verify the Broker certificate.")
	flags.StringVar(&config.BrokerServerName, "broker-server-name", config.BrokerServerName, "TLS server name used for the Broker connection.")
	flags.StringVar(&config.BearerToken, "bearer-token", config.BearerToken, "OIDC Bearer token for broker approval authentication.")
	flags.StringVar(&config.BearerTokenFile, "bearer-token-file", config.BearerTokenFile, "Path to a file containing the OIDC Bearer token.")
	_ = flags.Parse(os.Args[1:])
	if err := config.validate(); err != nil {
		fatal(err)
	}
	if err := run(context.Background(), config, dependencies{}); err != nil {
		fatal(err)
	}
}

func (c config) validate() error {
	if c.TLSCertFile == "" || c.TLSKeyFile == "" {
		return fmt.Errorf("adapter TLS certificate and key are required")
	}
	if err := validateHTTPSURL(c.BrokerURL); err != nil {
		return fmt.Errorf("invalid Broker URL: %w", err)
	}
	if err := broker.ValidateTargetID(c.BrokerClusterID); err != nil {
		return fmt.Errorf("invalid target cluster ID: %w", err)
	}
	if c.BrokerCAFile == "" {
		return fmt.Errorf("Broker CA file is required")
	}
	return nil
}

func run(ctx context.Context, config config, deps dependencies) error {
	// Create the Broker HTTP client with Bearer token authentication.
	brokerHTTPClient, err := newHTTPClient(config.BrokerCAFile, config.BrokerServerName)
	if err != nil {
		return fmt.Errorf("create Broker HTTP client: %w", err)
	}
	brokerHTTPClient = withBearerToken(brokerHTTPClient, config.BearerToken, config.BearerTokenFile)
	approver, err := approvaladapter.NewBrokerHTTPClient(config.BrokerURL, config.BrokerClusterID, brokerHTTPClient)
	if err != nil {
		return err
	}

	if (deps.Source == nil) != (deps.Notifier == nil) || (deps.InstanceCodes == nil) != (deps.Source == nil) {
		return fmt.Errorf("approval adapter dependencies must provide source, notifier, and instance code channel together")
	}
	if deps.Source != nil {
		pipeline, err := approvaladapter.NewPipeline(deps.Source, approver, deps.Notifier)
		if err != nil {
			return err
		}
		go processInstances(ctx, pipeline, deps.InstanceCodes)
	}

	// Serve a health check endpoint. Add custom HTTP handlers as needed.
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	server := &http.Server{
		Addr: config.ListenAddress, Handler: mux,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.ListenAndServeTLS(config.TLSCertFile, config.TLSKeyFile)
	}()
	signalContext, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	select {
	case <-signalContext.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownContext)
	case err := <-serverErr:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

func processInstances(ctx context.Context, pipeline *approvaladapter.Pipeline, instanceCodes <-chan string) {
	for {
		select {
		case <-ctx.Done():
			return
		case instanceCode, ok := <-instanceCodes:
			if !ok {
				return
			}
			if err := pipeline.Process(ctx, instanceCode); err != nil {
				// Do not log references or credential responses.
				fmt.Fprintf(os.Stderr, "approval adapter processing failed: %v\n", err)
			}
		}
	}
}

func newHTTPClient(caFile, serverName string) (*http.Client, error) {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		transport = &http.Transport{}
	}
	transport = transport.Clone()
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}
	if caFile != "" {
		data, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read CA file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(data) {
			return nil, fmt.Errorf("CA file contains no certificates")
		}
		tlsConfig.RootCAs = pool
	}
	transport.TLSClientConfig = tlsConfig
	return &http.Client{Transport: transport, Timeout: 30 * time.Second}, nil
}

func validateHTTPSURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("must be an absolute HTTPS URL without query or fragment")
	}
	return nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func withBearerToken(client *http.Client, token, tokenFile string) *http.Client {
	if token == "" && tokenFile != "" {
		data, err := os.ReadFile(tokenFile)
		if err == nil {
			token = strings.TrimSpace(string(data))
		}
	}
	if token == "" {
		return client
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		transport = http.DefaultTransport.(*http.Transport).Clone()
	}
	wrapped := &bearerTransport{base: transport, token: token}
	clone := *client
	clone.Transport = wrapped
	return &clone
}

type bearerTransport struct {
	base  http.RoundTripper
	token string
}

func (t *bearerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	request.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(request)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
