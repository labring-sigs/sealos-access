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

package usercmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteKubeconfigRefusesSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(directory, "kubeconfig")
	if err := os.Symlink(target, output); err != nil {
		t.Fatal(err)
	}
	if err := writeKubeconfig(output, []byte("replacement")); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("writeKubeconfig() error = %v, want symlink rejection", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "original" {
		t.Fatalf("symlink target changed to %q", data)
	}
}

func TestWriteKubeconfigUses0600(t *testing.T) {
	output := filepath.Join(t.TempDir(), "kubeconfig")
	if err := writeKubeconfig(output, []byte("apiVersion: v1\n")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("output mode = %o, want 600", info.Mode().Perm())
	}
}

func TestBuildAuthorizationURLIncludesStateNonceAndS256PKCE(t *testing.T) {
	state := authorizationState{
		State:        "state-value",
		Nonce:        "nonce-value",
		CodeVerifier: strings.Repeat("v", 43),
		RedirectURI:  "http://127.0.0.1:12345/callback",
	}
	value, err := buildAuthorizationURL("https://issuer.example/authorize", "internal-kc", "alice", state)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if query.Get("state") != state.State || query.Get("nonce") != state.Nonce || query.Get("login_hint") != "alice" {
		t.Fatalf("authorization query missing state/nonce/login hint: %s", parsed.RawQuery)
	}
	if query.Get("code_challenge_method") != "S256" {
		t.Fatalf("code challenge method = %q", query.Get("code_challenge_method"))
	}
	digest := sha256.Sum256([]byte(state.CodeVerifier))
	expectedChallenge := base64.RawURLEncoding.EncodeToString(digest[:])
	if query.Get("code_challenge") != expectedChallenge {
		t.Fatalf("code challenge = %q, want %q", query.Get("code_challenge"), expectedChallenge)
	}
}

func TestParseCLIArgsSupportsBaseAndElevatedRedeem(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tests := []struct {
		name      string
		args      []string
		operation cliOperation
		output    string
		reference string
	}{
		{
			name:      "base",
			args:      []string{"--broker-url", "https://broker.example", "--cluster-id", "cluster-a", "--oidc-issuer", "https://issuer.example", "--client-id", "internal-kc", "--requested-ttl-seconds", "1800", "--output", "base.kubeconfig"},
			operation: operationBase,
			output:    "base.kubeconfig",
		},
		{
			name:      "elevated redeem",
			args:      []string{"elevated", "redeem", "--broker-url", "https://broker.example", "--cluster-id", "cluster-a", "--oidc-issuer", "https://issuer.example", "--client-id", "internal-kc", "--approval-reference", "approval-1", "--output", "elevated.kubeconfig"},
			operation: operationElevatedRedeem,
			output:    "elevated.kubeconfig",
			reference: "approval-1",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config, err := parseCLIArgs(test.args)
			if err != nil {
				t.Fatal(err)
			}
			if config.Operation != test.operation || config.Output != test.output || config.ApprovalReference != test.reference {
				t.Fatalf("config = %#v", config)
			}
			if err := validateConfig(config); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestParseCLIArgsLoadsConfigAndCLIFlagsOverrideIt(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	configPath := filepath.Join(t.TempDir(), "user.yaml")
	configData := []byte(`broker-url: https://broker.from-config.example
cluster-id: cluster-a
broker-ca-file: ./pki/broker-ca.pem
broker-server-name: broker.internal.example
oidc-issuer: https://issuer.from-config.example
oidc-ca-file: ./pki/oidc-ca.pem
oidc-server-name: issuer.internal.example
client-id: internal-kc-from-config
output: ./from-config.kubeconfig
requested-ttl-seconds: 1800
no-browser: true
login-hint: alice@example.internal
`)
	if err := os.WriteFile(configPath, configData, 0600); err != nil {
		t.Fatal(err)
	}

	config, err := parseCLIArgs([]string{
		"--config", configPath,
		"--broker-url", "https://broker.override.example",
		"--cluster-id", "cluster-b",
		"--client-id", "internal-kc-override",
		"--output", "./override.kubeconfig",
		"--no-browser=false",
	})
	if err != nil {
		t.Fatal(err)
	}
	if config.ConfigFile != configPath {
		t.Fatalf("config file = %q, want %q", config.ConfigFile, configPath)
	}
	if config.BrokerURL != "https://broker.override.example" || config.ClusterID != "cluster-b" || config.ClientID != "internal-kc-override" || config.Output != "./override.kubeconfig" {
		t.Fatalf("CLI overrides were not applied: %#v", config)
	}
	if config.BrokerCAFile != "./pki/broker-ca.pem" || config.BrokerServerName != "broker.internal.example" || config.OIDCIssuer != "https://issuer.from-config.example" || config.OIDCCAFile != "./pki/oidc-ca.pem" || config.OIDCServerName != "issuer.internal.example" || config.RequestedTTL != 1800 || config.NoBrowser || config.LoginHint != "alice@example.internal" {
		t.Fatalf("config values were not loaded: %#v", config)
	}
	if err := validateConfig(config); err != nil {
		t.Fatal(err)
	}
}

func TestParseCLIArgsDiscoversDefaultConfig(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	configPath := filepath.Join(configHome, "sealos", "access", "user.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`broker-url: https://broker.example
cluster-id: cluster-a
oidc-issuer: https://issuer.example
client-id: internal-kc
output: ./default.kubeconfig
requested-ttl-seconds: 3600
`), 0600); err != nil {
		t.Fatal(err)
	}

	config, err := parseCLIArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	if config.ConfigFile != configPath || config.BrokerURL != "https://broker.example" || config.Output != "./default.kubeconfig" {
		t.Fatalf("default config = %#v", config)
	}
	if err := validateConfig(config); err != nil {
		t.Fatal(err)
	}
}

func TestParseCLIArgsDoesNotReadAdminDefaultConfig(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	adminConfigPath := filepath.Join(configHome, "sealos", "access", "admin.yaml")
	if err := os.MkdirAll(filepath.Dir(adminConfigPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(adminConfigPath, []byte("issuer: https://issuer.example\nsubject: subject-1\n"), 0600); err != nil {
		t.Fatal(err)
	}

	config, err := parseCLIArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	if config.ConfigFile != "" || config.OIDCIssuer != "" || config.ApprovalReference != "" {
		t.Fatalf("user command consumed administrator defaults: %#v", config)
	}
	if err := validateConfig(config); err == nil || !strings.Contains(err.Error(), "broker URL is required") {
		t.Fatalf("validateConfig() error = %v, want missing user defaults", err)
	}
}

func TestParseCLIArgsRejectsAdminConfigSchema(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "admin.yaml")
	if err := os.WriteFile(configPath, []byte("kubeconfig: /secure/admin.kubeconfig\nissuer: https://issuer.example\nsubject: alice\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := parseCLIArgs([]string{"--config", configPath})
	if err == nil || !strings.Contains(err.Error(), "kubeconfig") {
		t.Fatalf("parseCLIArgs() error = %v, want administrator schema rejection", err)
	}
}

func TestCLIFlagsAreOperationScoped(t *testing.T) {
	base := cliConfig{Operation: operationBase}
	baseFlags := newCLIFlagSet(&base)
	if baseFlags.Lookup("approval-reference") != nil {
		t.Fatal("base command exposes elevated-only approval-reference flag")
	}
	if baseFlags.Lookup("requested-ttl-seconds") == nil {
		t.Fatal("base command does not expose requested-ttl-seconds flag")
	}

	elevated := cliConfig{Operation: operationElevatedRedeem}
	elevatedFlags := newCLIFlagSet(&elevated)
	if elevatedFlags.Lookup("requested-ttl-seconds") != nil {
		t.Fatal("elevated command exposes base-only requested-ttl-seconds flag")
	}
	if elevatedFlags.Lookup("approval-reference") == nil {
		t.Fatal("elevated command does not expose approval-reference flag")
	}
}

func TestParseCLIArgsRejectsUnknownConfigFields(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "invalid.yaml")
	if err := os.WriteFile(configPath, []byte("broker-url: https://broker.example\nunknown-field: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := parseCLIArgs([]string{"--config", configPath})
	if err == nil || !strings.Contains(err.Error(), "unknown-field") {
		t.Fatalf("parseCLIArgs() error = %v, want unknown config field error", err)
	}
}

func TestParseCLIArgsRejectsMultipleConfigDocuments(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "multiple.yaml")
	if err := os.WriteFile(configPath, []byte("broker-url: https://broker.example\n---\nbroker-url: https://other.example\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := parseCLIArgs([]string{"--config", configPath})
	if err == nil || !strings.Contains(err.Error(), "one YAML document") {
		t.Fatalf("parseCLIArgs() error = %v, want multiple document error", err)
	}
}

func TestConfigDoesNotAcceptApprovalReference(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "approval.yaml")
	if err := os.WriteFile(configPath, []byte("approval-reference: approval-1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := parseCLIArgs([]string{"--config", configPath})
	if err == nil || !strings.Contains(err.Error(), "approval-reference") {
		t.Fatalf("parseCLIArgs() error = %v, want approval reference rejection", err)
	}
}

func TestParseCLIArgsHelpDoesNotLoadConfig(t *testing.T) {
	_, err := parseCLIArgs([]string{"--config", filepath.Join(t.TempDir(), "missing.yaml"), "--help"})
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("parseCLIArgs() error = %v, want help result", err)
	}
}

func TestParseCLIArgsRejectsElevatedRequest(t *testing.T) {
	if _, err := parseCLIArgs([]string{"elevated", "request"}); err == nil || !strings.Contains(err.Error(), "unknown elevated subcommand") {
		t.Fatalf("parseCLIArgs() error = %v, want removed request rejection", err)
	}
}

func TestParseCLIArgsRejectsBaseIdentityAndProfileInjection(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	baseArgs := []string{
		"--broker-url", "https://broker.example",
		"--cluster-id", "cluster-a",
		"--oidc-issuer", "https://issuer.example",
		"--client-id", "internal-kc",
		"--requested-ttl-seconds", "1800",
		"--output", filepath.Join(t.TempDir(), "base.kubeconfig"),
	}
	tests := []struct {
		name         string
		args         []string
		wantParseErr bool
	}{
		{name: "target flag", args: []string{"--target", "bob"}, wantParseErr: true},
		{name: "profile flag", args: []string{"--profile", "cluster-ops-write.v1"}, wantParseErr: true},
		{name: "base approval reference", args: []string{"--approval-reference", "reference-1"}, wantParseErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args := append(append([]string(nil), baseArgs...), test.args...)
			config, err := parseCLIArgs(args)
			if test.wantParseErr && err == nil {
				t.Fatalf("parseCLIArgs() succeeded with an unsupported flag: %#v", config)
			}
		})
	}
}

func TestIssueBaseCredentialSendsOnlyRequestedTTL(t *testing.T) {
	var requestCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if r.Method != http.MethodPost || r.URL.Path != "/v1/clusters/cluster-a/credentials/base" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer access-token" {
			t.Fatalf("authorization header = %q", r.Header.Get("Authorization"))
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body) != 1 {
			t.Fatalf("base request fields = %#v, want only requestedTTLSeconds", body)
		}
		var requestedTTL int64
		if err := json.Unmarshal(body["requestedTTLSeconds"], &requestedTTL); err != nil {
			t.Fatalf("requestedTTLSeconds = %s: %v", body["requestedTTLSeconds"], err)
		}
		if requestedTTL != 1800 {
			t.Fatalf("requestedTTLSeconds = %d, want 1800", requestedTTL)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"clusterID":"cluster-a","leaseID":"lease-1","profile":"base-readonly.v1","username":"iu-alice","expirationTimestamp":"2026-09-07T01:00:00Z","kubeconfig":"apiVersion: v1"}`))
	}))
	defer server.Close()

	response, err := issueBaseCredentialForCluster(context.Background(), server.Client(), server.URL, "cluster-a", "access-token", 1800)
	if err != nil {
		t.Fatal(err)
	}
	if response.Profile != "base-readonly.v1" || requestCount != 1 {
		t.Fatalf("response = %#v, requestCount = %d", response, requestCount)
	}
}

func TestCLIRejectsUntrustedTLSForOIDCAndBroker(t *testing.T) {
	tests := []struct {
		name   string
		broker bool
	}{
		{name: "OIDC"},
		{name: "Broker", broker: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requestCount int
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestCount++
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer server.Close()

			client, err := newHTTPClient("", "")
			if err != nil {
				t.Fatal(err)
			}
			if test.broker {
				_, err = issueBaseCredentialForCluster(context.Background(), client, server.URL, "cluster-a", "access-token", 1800)
			} else {
				_, err = fetchDiscovery(context.Background(), client, server.URL)
			}
			if err == nil {
				t.Fatal("TLS request succeeded with an untrusted server certificate")
			}
			if requestCount != 0 {
				t.Fatalf("server received %d HTTP requests after TLS rejection", requestCount)
			}
		})
	}
}

func TestCLIRejectsWrongTLSServerNameForOIDCAndBroker(t *testing.T) {
	tests := []struct {
		name   string
		broker bool
	}{
		{name: "OIDC"},
		{name: "Broker", broker: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requestCount int
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestCount++
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer server.Close()
			caFile := writeTestCertificate(t, server.Certificate())

			client, err := newHTTPClient(caFile, "wrong.example.invalid")
			if err != nil {
				t.Fatal(err)
			}
			if test.broker {
				_, err = issueBaseCredentialForCluster(context.Background(), client, server.URL, "cluster-a", "access-token", 1800)
			} else {
				_, err = fetchDiscovery(context.Background(), client, server.URL)
			}
			if err == nil {
				t.Fatal("TLS request succeeded with the wrong server name")
			}
			if requestCount != 0 {
				t.Fatalf("server received %d HTTP requests after server-name rejection", requestCount)
			}
		})
	}
}

func TestRunFailsClosedBeforeBrokerOnInvalidOIDCTLS(t *testing.T) {
	tests := []struct {
		name           string
		oidcCAFile     func(t *testing.T, server *httptest.Server) string
		oidcServerName string
	}{
		{name: "untrusted CA"},
		{
			name: "wrong server name",
			oidcCAFile: func(t *testing.T, server *httptest.Server) string {
				return writeTestCertificate(t, server.Certificate())
			},
			oidcServerName: "wrong.example.invalid",
		},
		{
			name: "invalid CA file",
			oidcCAFile: func(t *testing.T, _ *httptest.Server) string {
				path := filepath.Join(t.TempDir(), "invalid-ca.pem")
				if err := os.WriteFile(path, []byte("not a certificate"), 0600); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var brokerRequests int
			brokerServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				brokerRequests++
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer brokerServer.Close()
			oidcServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"issuer":"` + serverURLWithoutTrailingSlash(t, r) + `","authorization_endpoint":"https://auth.example/authorize","token_endpoint":"https://auth.example/token"}`))
			}))
			defer oidcServer.Close()

			oidcCAFile := ""
			if test.oidcCAFile != nil {
				oidcCAFile = test.oidcCAFile(t, oidcServer)
			}
			output := filepath.Join(t.TempDir(), "kubeconfig")
			var stdout, stderr bytes.Buffer
			err := run(context.Background(), cliConfig{
				BrokerURL:       brokerServer.URL,
				ClusterID:       "cluster-a",
				OIDCIssuer:      oidcServer.URL,
				OIDCCAFile:      oidcCAFile,
				OIDCServerName:  test.oidcServerName,
				ClientID:        "internal-kc",
				Output:          output,
				RequestedTTL:    1800,
				Stdout:          &stdout,
				Stderr:          &stderr,
				NoBrowser:       true,
				CallbackTimeout: 100 * time.Millisecond,
			})
			if err == nil {
				t.Fatal("run() succeeded despite invalid OIDC TLS configuration")
			}
			if brokerRequests != 0 {
				t.Fatalf("Broker received %d requests after OIDC TLS failure", brokerRequests)
			}
			if _, statErr := os.Stat(output); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("output file stat error = %v, want no output file", statErr)
			}
			if strings.Contains(stdout.String(), "kubeconfig") || strings.Contains(stderr.String(), "kubeconfig") {
				t.Fatalf("credential output appeared after TLS failure: stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

func writeTestCertificate(t *testing.T, certificate *x509.Certificate) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func serverURLWithoutTrailingSlash(t *testing.T, request *http.Request) string {
	t.Helper()
	if request.TLS == nil {
		t.Fatal("OIDC test request was not served over TLS")
	}
	return "https://" + request.Host
}

func TestRedeemElevatedCredentialDoesNotAcceptTargetOrProfile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/clusters/cluster-a/credentials/elevated/redeem" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer access-token" {
			t.Fatalf("authorization header = %q", r.Header.Get("Authorization"))
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body) != 1 || body["approvalReference"] != "approval-1" {
			t.Fatalf("request body = %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"clusterID":"cluster-a","leaseID":"lease-1","profile":"cluster-ops-write.v1","username":"iu-alice","expirationTimestamp":"2026-09-07T01:00:00Z","kubeconfig":"apiVersion: v1"}`))
	}))
	defer server.Close()
	result, err := redeemElevatedCredentialForCluster(context.Background(), server.Client(), server.URL, "cluster-a", "access-token", "approval-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Profile != "cluster-ops-write.v1" || result.Kubeconfig == "" || !result.ExpirationTimestamp.Equal(time.Date(2026, 9, 7, 1, 0, 0, 0, time.UTC)) {
		t.Fatalf("result = %#v", result)
	}
}

func TestWriteCredentialResponseKeepsKubeconfigOutOfOutput(t *testing.T) {
	outputPath := filepath.Join(t.TempDir(), "elevated-kubeconfig")
	var output bytes.Buffer
	config := cliConfig{OIDCIssuer: "https://issuer.example", ClusterID: "cluster-a", Output: outputPath, Stdout: &output}
	response := brokerResponse{
		ClusterID:           "cluster-a",
		LeaseID:             "lease-1",
		Profile:             elevatedProfile,
		Username:            "iu-alice",
		ExpirationTimestamp: time.Date(2026, 9, 7, 1, 0, 0, 0, time.UTC),
		Kubeconfig:          "apiVersion: v1\nusers:\n- token: token-value\n",
	}
	if err := writeCredentialResponse(config, response); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("output mode = %o, want 600", info.Mode().Perm())
	}
	if strings.Contains(output.String(), "token-value") || strings.Contains(output.String(), "apiVersion:") || strings.Contains(output.String(), "kubeconfig") {
		t.Fatalf("credential material appeared in CLI output: %q", output.String())
	}
}
