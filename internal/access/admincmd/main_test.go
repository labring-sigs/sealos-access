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

package admincmd

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	userv1 "github.com/labring-sigs/sealos-access/api/v1"
	"github.com/labring-sigs/sealos-access/pkg/internalcredentials"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestParseAdminCLIArgs(t *testing.T) {
	config, err := parseAdminCLIArgs([]string{
		"create",
		"--kubeconfig", "/path/to/admin.kubeconfig",
		"--issuer", "https://issuer.example.internal",
		"--subject", "oidc-subject-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if config.Operation != operationCreate || config.Kubeconfig != "/path/to/admin.kubeconfig" || config.Issuer != "https://issuer.example.internal" || config.Subject != "oidc-subject-1" {
		t.Fatalf("config = %#v", config)
	}
}

func TestParseAdminCLIArgsSupportsApprove(t *testing.T) {
	config, err := parseAdminCLIArgs([]string{
		"approve",
		"--broker-url", "https://broker.example",
		"--oidc-issuer", "https://oidc.example",
		"--client-id", "admin-cli",
		"--approval-id", "approval-1",
		"--approval-reason", "incident remediation",
		"--issuer", "https://issuer.example",
		"--subject", "alice",
		"--requested-ttl-seconds", "1800",
	})
	if err != nil {
		t.Fatal(err)
	}
	if config.Operation != operationApprove || config.BrokerURL != "https://broker.example" || config.ApprovalID != "approval-1" || config.RequestedTTL != 1800 {
		t.Fatalf("config = %#v", config)
	}
}

func TestParseAdminCLIArgsLoadsConfigAndCLIFlagsOverrideIt(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "admin.yaml")
	configData := []byte(`kubeconfig: /secure/from-config.kubeconfig
issuer: https://issuer.from-config.example
subject: subject-from-config
`)
	if err := os.WriteFile(configPath, configData, 0600); err != nil {
		t.Fatal(err)
	}

	config, err := parseAdminCLIArgs([]string{
		"create",
		"--config", configPath,
		"--issuer", "https://issuer.override.example",
		"--subject", "subject-override",
	})
	if err != nil {
		t.Fatal(err)
	}
	if config.ConfigFile != configPath || config.Kubeconfig != "/secure/from-config.kubeconfig" || config.Issuer != "https://issuer.override.example" || config.Subject != "subject-override" {
		t.Fatalf("config = %#v", config)
	}
}

func TestParseAdminCLIArgsDiscoversDefaultConfig(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	configPath := filepath.Join(configHome, "sealos", "access", "admin.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`issuer: https://issuer.example
subject: subject-1
`), 0600); err != nil {
		t.Fatal(err)
	}

	config, err := parseAdminCLIArgs([]string{"create"})
	if err != nil {
		t.Fatal(err)
	}
	if config.ConfigFile != configPath || config.Issuer != "https://issuer.example" || config.Subject != "subject-1" {
		t.Fatalf("config = %#v", config)
	}
}

func TestParseAdminCLIArgsDoesNotReadUserDefaultConfig(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	userConfigPath := filepath.Join(configHome, "sealos", "access", "user.yaml")
	if err := os.MkdirAll(filepath.Dir(userConfigPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userConfigPath, []byte("broker-url: https://broker.example\noutput: ./user.kubeconfig\n"), 0600); err != nil {
		t.Fatal(err)
	}

	config, err := parseAdminCLIArgs([]string{"create"})
	if err != nil {
		t.Fatal(err)
	}
	if config.ConfigFile != "" || config.Kubeconfig != "" {
		t.Fatalf("administrator command consumed user defaults: %#v", config)
	}
	if err := validateAdminConfig(config); err == nil || !strings.Contains(err.Error(), "issuer is required") {
		t.Fatalf("validateAdminConfig() error = %v, want missing administrator defaults", err)
	}
}

func TestParseAdminCLIArgsRejectsUserConfigSchema(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "user.yaml")
	if err := os.WriteFile(configPath, []byte("broker-url: https://broker.example\noutput: ./user.kubeconfig\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := parseAdminCLIArgs([]string{"create", "--config", configPath})
	if err == nil || !strings.Contains(err.Error(), "output") {
		t.Fatalf("parseAdminCLIArgs() error = %v, want user schema rejection", err)
	}
}

func TestAdminFlagsAreOperationScoped(t *testing.T) {
	create := adminConfig{Operation: operationCreate}
	createFlags := newAdminFlagSet(&create)
	if createFlags.Lookup("kubeconfig") == nil {
		t.Fatal("create command does not expose kubeconfig flag")
	}
	if createFlags.Lookup("approval-id") != nil || createFlags.Lookup("broker-url") != nil {
		t.Fatal("create command exposes approve-only flags")
	}

	approve := adminConfig{Operation: operationApprove}
	approveFlags := newAdminFlagSet(&approve)
	if approveFlags.Lookup("kubeconfig") != nil {
		t.Fatal("approve command exposes create-only kubeconfig flag")
	}
	if approveFlags.Lookup("approval-id") == nil || approveFlags.Lookup("broker-url") == nil {
		t.Fatal("approve command does not expose approval flags")
	}
}

func TestParseAdminCLIArgsRejectsInvalidConfigDocuments(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "unknown field", data: "issuer: https://issuer.example\nsecret: value\n", want: "secret"},
		{name: "multiple documents", data: "issuer: https://issuer.example\n---\nissuer: https://other.example\n", want: "one YAML document"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "invalid.yaml")
			if err := os.WriteFile(configPath, []byte(test.data), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := parseAdminCLIArgs([]string{"create", "--config", configPath})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("parseAdminCLIArgs() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestParseAdminCLIArgsHelpDoesNotLoadConfig(t *testing.T) {
	_, err := parseAdminCLIArgs([]string{"create", "--config", filepath.Join(t.TempDir(), "missing.yaml"), "--help"})
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("parseAdminCLIArgs() error = %v, want help result", err)
	}
}

func TestParseAdminCLIArgsRejectsUnexpectedIdentityControls(t *testing.T) {
	for _, args := range [][]string{
		{"create", "--issuer", "https://issuer.example", "--subject", "alice", "--name", "custom"},
		{"create", "--issuer", "https://issuer.example", "--subject", "alice", "--profile", "base-readonly.v1"},
		{"create", "--issuer", "https://issuer.example", "--subject", "alice", "--role-profile", "cluster-ops-write.v1"},
	} {
		if _, err := parseAdminCLIArgs(args); err == nil {
			t.Fatalf("parseAdminCLIArgs(%v) unexpectedly succeeded", args)
		}
	}
}

func TestParseAdminCLIArgsKeepsOperationFlagsSeparate(t *testing.T) {
	if _, err := parseAdminCLIArgs([]string{
		"create",
		"--issuer", "https://issuer.example",
		"--subject", "alice",
		"--approval-id", "approval-1",
	}); err == nil {
		t.Fatal("create accepted an approve-only flag")
	}
	if _, err := parseAdminCLIArgs([]string{
		"approve",
		"--issuer", "https://issuer.example",
		"--subject", "alice",
		"--kubeconfig", "/secure/admin.kubeconfig",
	}); err == nil {
		t.Fatal("approve accepted a create-only flag")
	}
}

func TestParseAdminCLIArgsRejectsUnknownSubcommandAndTrailingArguments(t *testing.T) {
	if _, err := parseAdminCLIArgs([]string{"delete"}); err == nil || !strings.Contains(err.Error(), "unknown subcommand") {
		t.Fatalf("parseAdminCLIArgs() error = %v, want unknown subcommand error", err)
	}
	if _, err := parseAdminCLIArgs([]string{"create", "trailing"}); err == nil || !strings.Contains(err.Error(), "unexpected argument") {
		t.Fatalf("parseAdminCLIArgs() error = %v, want trailing argument error", err)
	}
}

func TestValidateAdminConfig(t *testing.T) {
	tests := []struct {
		name   string
		config adminConfig
		want   string
	}{
		{
			name:   "missing issuer",
			config: adminConfig{Operation: operationCreate, Subject: "alice"},
			want:   "issuer is required",
		},
		{
			name:   "missing subject",
			config: adminConfig{Operation: operationCreate, Issuer: "https://issuer.example"},
			want:   "subject is required",
		},
		{
			name:   "non HTTPS issuer",
			config: adminConfig{Operation: operationCreate, Issuer: "http://issuer.example", Subject: "alice"},
			want:   "issuer must be an HTTPS URL",
		},
		{
			name:   "subject whitespace",
			config: adminConfig{Operation: operationCreate, Issuer: "https://issuer.example", Subject: " alice"},
			want:   "subject must be non-empty",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateAdminConfig(test.config)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateAdminConfig() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestNewInternalUserUsesDerivedNameAndBaseProfile(t *testing.T) {
	issuer := "https://issuer.example.internal"
	subject := "oidc-subject-1"
	user := newInternalUser(issuer, subject)

	if user.Name != internalcredentials.DeriveInternalUserName(issuer, subject) {
		t.Fatalf("name = %q, want derived name", user.Name)
	}
	if user.Spec.Identity.Issuer != issuer || user.Spec.Identity.Subject != subject {
		t.Fatalf("identity = %#v", user.Spec.Identity)
	}
	if user.Spec.RoleProfile != userv1.BaseReadonlyProfile || user.Spec.Suspend {
		t.Fatalf("spec = %#v, want active base profile", user.Spec)
	}
}

func TestRunCreatesInternalUser(t *testing.T) {
	kubeClient := newTestAdminClient(t)
	issuer := "https://issuer.example.internal"
	subject := "oidc-subject-1"
	var output bytes.Buffer

	err := run(context.Background(), adminConfig{
		Operation: operationCreate,
		Issuer:    issuer,
		Subject:   subject,
		Client:    kubeClient,
		Stdout:    &output,
	})
	if err != nil {
		t.Fatal(err)
	}

	created := &userv1.InternalUser{}
	name := types.NamespacedName{Name: internalcredentials.DeriveInternalUserName(issuer, subject)}
	if err := kubeClient.Get(context.Background(), name, created); err != nil {
		t.Fatal(err)
	}
	if created.Spec.RoleProfile != userv1.BaseReadonlyProfile || created.Spec.Identity.Subject != subject {
		t.Fatalf("created InternalUser = %#v", created)
	}
	wantOutput := "created InternalUser \"" + name.Name + "\""
	if !strings.Contains(output.String(), wantOutput) || !strings.Contains(output.String(), userv1.BaseReadonlyProfile) {
		t.Fatalf("output = %q, want creation metadata", output.String())
	}
}

func TestRunUpdatesExistingInternalUser(t *testing.T) {
	issuer := "https://issuer.example.internal"
	subject := "oidc-subject-1"
	existing := newInternalUser(issuer, subject)
	kubeClient := newTestAdminClient(t, existing)

	var output bytes.Buffer
	err := run(context.Background(), adminConfig{
		Operation: operationCreate,
		Issuer:    issuer,
		Subject:   subject,
		Client:    kubeClient,
		Stdout:    &output,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "already exists") {
		t.Fatalf("output = %q, want update message", output.String())
	}
}

func TestRunDoesNotCreateForInvalidIdentity(t *testing.T) {
	kubeClient := newTestAdminClient(t)
	err := run(context.Background(), adminConfig{
		Operation: operationCreate,
		Issuer:    "http://issuer.example.internal",
		Subject:   "oidc-subject-1",
		Client:    kubeClient,
	})
	if err == nil {
		t.Fatal("run() unexpectedly succeeded")
	}

	var users userv1.InternalUserList
	if err := kubeClient.List(context.Background(), &users); err != nil {
		t.Fatal(err)
	}
	if len(users.Items) != 0 {
		t.Fatalf("invalid identity created %d users", len(users.Items))
	}
}

func TestRunCreateRequiresKubernetesClient(t *testing.T) {
	err := run(context.Background(), adminConfig{
		Operation: operationCreate,
		Issuer:    "https://issuer.example.internal",
		Subject:   "oidc-subject-1",
	})
	if err == nil || !strings.Contains(err.Error(), "Kubernetes client is required") {
		t.Fatalf("run() error = %v, want Kubernetes client requirement", err)
	}
}

func TestRunApproveCallsInternalBrokerEndpoint(t *testing.T) {
	var output bytes.Buffer
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/clusters/cluster-a/internal/credentials/approve" {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer test-access-token" {
			t.Fatalf("approval request missing bearer token, got Authorization: %q", request.Header.Get("Authorization"))
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		for _, expected := range []string{`"clusterID":"cluster-a"`, `"approvalID":"approval-1"`, `"subject":"alice"`, `"requestedTTLSeconds":1800`, `"approvalReason":"incident remediation"`} {
			if !strings.Contains(string(body), expected) {
				t.Fatalf("request body %q does not contain %q", body, expected)
			}
		}
		return &http.Response{StatusCode: http.StatusCreated, Body: io.NopCloser(strings.NewReader(`{"approvalID":"approval-1","clusterID":"cluster-a","approvalReference":"r1.approval-00000000000000000000000000000000.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","profile":"cluster-ops-write.v1","requestedTTLSeconds":1800,"approvalExpiresAt":"2026-09-10T00:00:00Z"}`)), Request: request}, nil
	})
	config := adminConfig{
		Operation:      operationApprove,
		BrokerURL:      "https://broker.example",
		BrokerCAFile:   "",
		ClusterID:      "cluster-a",
		Issuer:         "https://issuer.example",
		Subject:        "alice",
		ApprovalID:     "approval-1",
		ApprovalReason: "incident remediation",
		RequestedTTL:   1800,
		HTTPClient:     &http.Client{Transport: transport},
		Stdout:         &output,
	}
	// Test submitAdminApproval directly (bypasses OIDC login)
	err := submitAdminApproval(context.Background(), config, "test-access-token")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "approval reference:") || strings.Contains(output.String(), "kubeconfig") || strings.Contains(output.String(), "token") {
		t.Fatalf("output = %q", output.String())
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func newTestAdminClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := userv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}
