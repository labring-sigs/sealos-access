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
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	stderrors "errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
	"unicode"

	userv1 "github.com/labring-sigs/sealos-access/api/v1"
	"github.com/labring-sigs/sealos-access/pkg/broker"
	"github.com/labring-sigs/sealos-access/pkg/internalcredentials"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sRuntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type adminOperation string

const (
	operationCreate  adminOperation = "create"
	operationApprove adminOperation = "approve"
)

type adminConfig struct {
	Operation        adminOperation
	ConfigFile       string
	Kubeconfig       string
	Issuer           string
	Subject          string
	BrokerURL        string
	ClusterID        string
	BrokerCAFile     string
	BrokerServerName string
	OIDCIssuer       string
	OIDCCAFile       string
	ClientID         string
	NoBrowser        bool
	ApprovalID       string
	ApprovalReason   string
	RequestedTTL     int64
	Client           client.Client
	ConfigLoader     func(string) (*rest.Config, error)
	HTTPClient       *http.Client
	Stdout           io.Writer
	Stderr           io.Writer
}

type adminOIDCDiscovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
}

type adminTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	IDToken     string `json:"id_token"`
}

type adminAuthorizationState struct {
	State        string
	Nonce        string
	CodeVerifier string
	RedirectURI  string
}

type adminCallbackResult struct {
	Code        string
	State       string
	Error       string
	Description string
}

func Main(args []string, stdout, stderr io.Writer) int {
	config, err := parseAdminCLIArgs(args)
	if err != nil {
		if stderrors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, err)
		return 2
	}
	config.Stdout = stdout
	config.Stderr = stderr
	if err := run(context.Background(), config); err != nil {
		fmt.Fprintln(config.Stderr, err)
		return 1
	}
	return 0
}

func parseAdminCLIArgs(args []string) (adminConfig, error) {
	config := adminConfig{Operation: operationCreate}
	if len(args) == 0 {
		return adminConfig{}, stderrors.New("a create subcommand is required")
	}
	if args[0] != string(operationCreate) && args[0] != string(operationApprove) {
		flags := newAdminFlagSet(&config)
		if err := flags.Parse(args); err != nil {
			return adminConfig{}, err
		}
		if flags.NArg() > 0 && flags.Arg(0) != string(operationCreate) && flags.Arg(0) != string(operationApprove) {
			return adminConfig{}, fmt.Errorf("unknown subcommand %q", flags.Arg(0))
		}
		return adminConfig{}, fmt.Errorf("unknown subcommand %q", args[0])
	}
	config.Operation = adminOperation(args[0])
	flagArgs := args[1:]
	configPath, _, err := extractAdminConfigPath(flagArgs)
	if err != nil {
		return adminConfig{}, err
	}
	if !adminArgsRequestHelp(flagArgs) {
		fileConfig, discoveredPath, err := loadAdminConfig(flagArgs)
		if err != nil {
			return adminConfig{}, err
		}
		if discoveredPath != "" {
			configPath = discoveredPath
		}
		fileConfig.apply(&config)
	}
	config.ConfigFile = configPath
	flags := newAdminFlagSet(&config)
	if err := flags.Parse(flagArgs); err != nil {
		return adminConfig{}, err
	}
	if flags.NArg() != 0 {
		return adminConfig{}, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	return config, nil
}

func newAdminFlagSet(config *adminConfig) *flag.FlagSet {
	flags := flag.NewFlagSet("sealos-access admin", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.StringVar(&config.ConfigFile, "config", config.ConfigFile, "YAML config file; defaults to the standard admin config path when present.")
	flags.StringVar(&config.Issuer, "issuer", config.Issuer, "Allowlisted HTTPS OIDC issuer for the InternalUser.")
	flags.StringVar(&config.Subject, "subject", config.Subject, "Opaque immutable OIDC subject for the InternalUser.")
	if config.Operation == operationCreate {
		flags.StringVar(&config.Kubeconfig, "kubeconfig", config.Kubeconfig, "Path to the administrator kubeconfig; defaults to the standard client-go loading rules.")
	} else {
		flags.StringVar(&config.BrokerURL, "broker-url", config.BrokerURL, "HTTPS URL of the internal credential Broker.")
		flags.StringVar(&config.ClusterID, "cluster-id", config.ClusterID, "Immutable target Kubernetes cluster ID.")
		flags.StringVar(&config.BrokerCAFile, "broker-ca-file", config.BrokerCAFile, "CA file used to verify the Broker certificate.")
		flags.StringVar(&config.BrokerServerName, "broker-server-name", config.BrokerServerName, "TLS server name used for the Broker connection.")
		flags.StringVar(&config.OIDCIssuer, "oidc-issuer", config.OIDCIssuer, "HTTPS OIDC issuer for admin authentication (required for approve).")
		flags.StringVar(&config.OIDCCAFile, "oidc-ca-file", config.OIDCCAFile, "Optional CA file used to verify the OIDC issuer certificate.")
		flags.StringVar(&config.ClientID, "client-id", config.ClientID, "Public OIDC client ID for admin authentication.")
		flags.BoolVar(&config.NoBrowser, "no-browser", config.NoBrowser, "Print the OIDC authorization URL instead of opening a browser.")
		flags.StringVar(&config.ApprovalID, "approval-id", config.ApprovalID, "Immutable external approval instance ID.")
		flags.StringVar(&config.ApprovalReason, "approval-reason", config.ApprovalReason, "Short operator reason recorded with the approval record.")
		flags.Int64Var(&config.RequestedTTL, "requested-ttl-seconds", config.RequestedTTL, "Requested credential lifetime in seconds.")
	}
	return flags
}

func run(ctx context.Context, config adminConfig) error {
	if err := validateAdminConfig(config); err != nil {
		return err
	}
	switch config.Operation {
	case operationCreate:
		return createInternalUser(ctx, config)
	case operationApprove:
		return approveElevated(ctx, config)
	default:
		return fmt.Errorf("unsupported operation: %s", config.Operation)
	}
}

func validateAdminConfig(config adminConfig) error {
	if config.Operation != operationCreate && config.Operation != operationApprove {
		return fmt.Errorf("unsupported operation: %s", config.Operation)
	}
	if config.Operation == operationApprove {
		for _, required := range []struct {
			name  string
			value string
		}{
			{name: "broker URL", value: config.BrokerURL},
			{name: "OIDC issuer", value: config.OIDCIssuer},
			{name: "client ID", value: config.ClientID},
			{name: "approval ID", value: config.ApprovalID},
			{name: "approval reason", value: config.ApprovalReason},
			{name: "issuer", value: config.Issuer},
			{name: "subject", value: config.Subject},
		} {
			if strings.TrimSpace(required.value) == "" {
				return fmt.Errorf("%s is required for approve", required.name)
			}
		}
		if err := validateHTTPSURL(config.BrokerURL); err != nil {
			return fmt.Errorf("invalid broker URL: %w", err)
		}
		if err := validateHTTPSURL(config.OIDCIssuer); err != nil {
			return fmt.Errorf("invalid OIDC issuer: %w", err)
		}
		if err := broker.ValidateTargetID(config.ClusterID); err != nil {
			return fmt.Errorf("invalid target cluster ID: %w", err)
		}
		if err := internalcredentials.ValidateIdentity(config.Issuer, config.Subject); err != nil {
			return fmt.Errorf("invalid target identity: %w", err)
		}
		if _, err := internalcredentials.ValidateRequestedTTL(userv1.ClusterOpsWriteProfile, config.RequestedTTL); err != nil {
			return fmt.Errorf("invalid requested TTL: %w", err)
		}
	} else {
		if config.Issuer == "" {
			return stderrors.New("issuer is required")
		}
		if config.Subject == "" {
			return stderrors.New("subject is required")
		}
		if err := internalcredentials.ValidateIdentity(config.Issuer, config.Subject); err != nil {
			return err
		}
	}
	return nil
}

func approveElevated(ctx context.Context, config adminConfig) error {
	oidcClient, err := newAdminHTTPClient(config.OIDCCAFile, "") // No server name needed for OIDC
	if err != nil {
		return fmt.Errorf("create OIDC HTTP client: %w", err)
	}

	// OIDC login flow
	discovery, err := fetchAdminDiscovery(ctx, oidcClient, config.OIDCIssuer)
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("start loopback callback: %w", err)
	}
	defer listener.Close()

	const adminCallbackPath = "/callback"
	state, err := newAdminAuthorizationState("http://" + listener.Addr().String() + adminCallbackPath)
	if err != nil {
		return fmt.Errorf("create authorization state: %w", err)
	}

	authorizationURL, err := buildAdminAuthorizationURL(discovery.AuthorizationEndpoint, config.ClientID, state)
	if err != nil {
		return fmt.Errorf("build authorization URL: %w", err)
	}

	callback := make(chan adminCallbackResult, 1)
	callbackServer := &http.Server{Handler: adminCallbackHandler(state.State, callback), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if serveErr := callbackServer.Serve(listener); serveErr != nil && !stderrors.Is(serveErr, http.ErrServerClosed) {
			select {
			case callback <- adminCallbackResult{Error: "callback server failed"}:
			default:
			}
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = callbackServer.Shutdown(shutdownCtx)
	}()

	const adminCallbackTimeout = 5 * time.Minute
	if config.NoBrowser {
		fmt.Fprintln(config.Stdout, authorizationURL)
	} else if err := openBrowser(authorizationURL); err != nil {
		fmt.Fprintf(config.Stderr, "open the authorization URL manually: %s\n", authorizationURL)
	}

	callbackCtx, cancel := context.WithTimeout(ctx, adminCallbackTimeout)
	defer cancel()
	var result adminCallbackResult
	select {
	case result = <-callback:
	case <-callbackCtx.Done():
		return stderrors.New("OIDC authorization timed out")
	}
	if result.Error != "" {
		if result.Description != "" {
			return fmt.Errorf("OIDC authorization failed: %s", result.Description)
		}
		return fmt.Errorf("OIDC authorization failed: %s", result.Error)
	}
	if result.State != state.State {
		return stderrors.New("OIDC authorization state validation failed")
	}
	if result.Code == "" {
		return stderrors.New("OIDC authorization did not return a code")
	}

	tokens, err := exchangeAdminAuthorizationCode(ctx, oidcClient, discovery.TokenEndpoint, config.ClientID, result.Code, state)
	if err != nil {
		return err
	}

	return submitAdminApproval(ctx, config, tokens.AccessToken)
}

// submitAdminApproval sends an approval record to the broker using the given access token.
// This is extracted for testability; callers should use approveElevated which handles OIDC login.
func submitAdminApproval(ctx context.Context, config adminConfig, accessToken string) error {
	brokerClient := config.HTTPClient
	if brokerClient == nil {
		var err error
		brokerClient, err = newAdminHTTPClient(config.BrokerCAFile, config.BrokerServerName)
		if err != nil {
			return fmt.Errorf("create Broker HTTP client: %w", err)
		}
	}

	body, err := json.Marshal(struct {
		ClusterID           string          `json:"clusterID"`
		ApprovalID          string          `json:"approvalID"`
		Target              userv1.Identity `json:"target"`
		RequestedTTLSeconds int64           `json:"requestedTTLSeconds"`
		ApprovalReason      string          `json:"approvalReason"`
	}{config.ClusterID, config.ApprovalID, userv1.Identity{Issuer: config.Issuer, Subject: config.Subject}, config.RequestedTTL, config.ApprovalReason})
	if err != nil {
		return fmt.Errorf("encode approval record: %w", err)
	}
	endpoint := strings.TrimRight(config.BrokerURL, "/") + "/v1/clusters/" + url.PathEscape(config.ClusterID) + "/internal/credentials/approve"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("create Broker request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+accessToken)
	response, err := brokerClient.Do(request)
	if err != nil {
		return fmt.Errorf("submit approval record: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		return fmt.Errorf("Broker returned HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64*1024))
	decoder.DisallowUnknownFields()
	var resultData struct {
		ClusterID           string    `json:"clusterID"`
		ApprovalID          string    `json:"approvalID"`
		ApprovalReference   string    `json:"approvalReference"`
		Profile             string    `json:"profile"`
		RequestedTTLSeconds int64     `json:"requestedTTLSeconds"`
		ApprovalExpiresAt   time.Time `json:"approvalExpiresAt"`
	}
	if err := decoder.Decode(&resultData); err != nil {
		return fmt.Errorf("decode Broker approval response: %w", err)
	}
	if resultData.ClusterID != config.ClusterID || resultData.ApprovalID != config.ApprovalID || resultData.Profile != userv1.ClusterOpsWriteProfile || resultData.RequestedTTLSeconds != config.RequestedTTL || !validAdminReference(resultData.ApprovalReference) || resultData.ApprovalExpiresAt.IsZero() {
		return stderrors.New("Broker returned an incomplete approval response")
	}
	_, err = fmt.Fprintf(config.Stdout, "approval ID: %s\napproval reference: %s\nprofile: %s\nrequested TTL: %ds\nexpires: %s\n", resultData.ApprovalID, resultData.ApprovalReference, resultData.Profile, resultData.RequestedTTLSeconds, resultData.ApprovalExpiresAt.UTC().Format(time.RFC3339))
	return err
}

func createInternalUser(ctx context.Context, config adminConfig) error {
	if config.Client == nil && config.Kubeconfig != "" {
		if config.ConfigLoader == nil {
			config.ConfigLoader = loadKubernetesConfig
		}
		restConfig, err := config.ConfigLoader(config.Kubeconfig)
		if err != nil {
			return fmt.Errorf("load kubeconfig: %w", err)
		}
		if config.HTTPClient != nil {
			restConfig.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
				return config.HTTPClient.Transport
			}
		}
		config.Client, err = newInternalUserClient(restConfig)
		if err != nil {
			return fmt.Errorf("create Kubernetes client: %w", err)
		}
	}
	if config.Client == nil {
		return stderrors.New("Kubernetes client is required")
	}
	user := newInternalUser(config.Issuer, config.Subject)
	if err := config.Client.Create(ctx, user); err != nil {
		if apierrors.IsAlreadyExists(err) {
			fmt.Fprintln(config.Stdout, "InternalUser already exists, updating profile")
			existing := &userv1.InternalUser{}
			if err := config.Client.Get(ctx, types.NamespacedName{Name: user.Name, Namespace: user.Namespace}, existing); err != nil {
				return fmt.Errorf("load existing InternalUser: %w", err)
			}
			patch := user.DeepCopy()
			patch.ResourceVersion = existing.ResourceVersion
			if err := config.Client.Update(ctx, patch); err != nil {
				return fmt.Errorf("update existing InternalUser: %w", err)
			}
			fmt.Fprintf(config.Stdout, "InternalUser %s updated with profile %s\n", patch.Name, userv1.BaseReadonlyProfile)
			return nil
		}
		return fmt.Errorf("create InternalUser: %w", err)
	}
	fmt.Fprintf(config.Stdout, "created InternalUser \"%s\" with profile %s\n", user.Name, userv1.BaseReadonlyProfile)
	return nil
}

func validAdminReference(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && strings.IndexFunc(value, unicode.IsSpace) < 0 && len(value) <= 512
}

func validateHTTPSURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return stderrors.New("must be an absolute HTTPS URL without query or fragment")
	}
	return nil
}

func newAdminHTTPClient(caFile, serverName string) (*http.Client, error) {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		transport = &http.Transport{}
	}
	transport = transport.Clone()
	if caFile != "" || serverName != "" {
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
				return nil, stderrors.New("CA file contains no certificates")
			}
			tlsConfig.RootCAs = pool
		}
		transport.TLSClientConfig = tlsConfig
	}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second}, nil
}

func newInternalUser(issuer, subject string) *userv1.InternalUser {
	return &userv1.InternalUser{
		ObjectMeta: metav1.ObjectMeta{
			Name: internalcredentials.DeriveInternalUserName(issuer, subject),
		},
		Spec: userv1.InternalUserSpec{
			Identity: userv1.Identity{
				Issuer:  issuer,
				Subject: subject,
			},
			RoleProfile: userv1.BaseReadonlyProfile,
		},
	}
}

func loadKubernetesConfig(kubeconfig string) (*rest.Config, error) {
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}

func newInternalUserClient(config *rest.Config) (client.Client, error) {
	scheme := k8sRuntime.NewScheme()
	if err := userv1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	return client.New(config, client.Options{Scheme: scheme})
}

// OIDC helper functions

func fetchAdminDiscovery(ctx context.Context, httpClient *http.Client, issuer string) (adminOIDCDiscovery, error) {
	endpoint := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
	var discovery adminOIDCDiscovery
	if err := adminGetJSON(ctx, httpClient, endpoint, &discovery); err != nil {
		return adminOIDCDiscovery{}, fmt.Errorf("fetch OIDC discovery: %w", err)
	}
	if discovery.Issuer != strings.TrimRight(issuer, "/") {
		return adminOIDCDiscovery{}, stderrors.New("OIDC discovery issuer does not match configuration")
	}
	if err := validateHTTPSURL(discovery.AuthorizationEndpoint); err != nil {
		return adminOIDCDiscovery{}, fmt.Errorf("invalid OIDC authorization endpoint: %w", err)
	}
	if err := validateHTTPSURL(discovery.TokenEndpoint); err != nil {
		return adminOIDCDiscovery{}, fmt.Errorf("invalid OIDC token endpoint: %w", err)
	}
	return discovery, nil
}

func adminGetJSON(ctx context.Context, httpClient *http.Client, endpoint string, target interface{}) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("endpoint returned HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64*1024))
	return decoder.Decode(target)
}

func newAdminAuthorizationState(redirectURI string) (adminAuthorizationState, error) {
	state, err := adminRandomURLValue(32)
	if err != nil {
		return adminAuthorizationState{}, err
	}
	nonce, err := adminRandomURLValue(32)
	if err != nil {
		return adminAuthorizationState{}, err
	}
	verifier, err := adminRandomURLValue(32)
	if err != nil {
		return adminAuthorizationState{}, err
	}
	return adminAuthorizationState{State: state, Nonce: nonce, CodeVerifier: verifier, RedirectURI: redirectURI}, nil
}

func adminRandomURLValue(size int) (string, error) {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func buildAdminAuthorizationURL(endpoint, clientID string, state adminAuthorizationState) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	query := parsed.Query()
	query.Set("client_id", clientID)
	query.Set("redirect_uri", state.RedirectURI)
	query.Set("response_type", "code")
	query.Set("scope", "openid profile email groups")
	query.Set("state", state.State)
	query.Set("nonce", state.Nonce)
	query.Set("code_challenge", adminCodeChallenge(state.CodeVerifier))
	query.Set("code_challenge_method", "S256")
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func adminCodeChallenge(verifier string) string {
	digest := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func adminCallbackHandler(expectedState string, result chan<- adminCallbackResult) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		query := request.URL.Query()
		callback := adminCallbackResult{Code: query.Get("code"), State: query.Get("state"), Error: query.Get("error"), Description: query.Get("error_description")}
		if callback.State != expectedState {
			callback = adminCallbackResult{Error: "state validation failed", State: callback.State}
		}
		select {
		case result <- callback:
		default:
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "Authentication complete. You may close this window.\n")
	})
	return mux
}

func exchangeAdminAuthorizationCode(ctx context.Context, httpClient *http.Client, endpoint, clientID, code string, state adminAuthorizationState) (adminTokenResponse, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"redirect_uri":  {state.RedirectURI},
		"code_verifier": {state.CodeVerifier},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return adminTokenResponse{}, fmt.Errorf("create OIDC token request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := httpClient.Do(request)
	if err != nil {
		return adminTokenResponse{}, fmt.Errorf("exchange OIDC authorization code: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return adminTokenResponse{}, fmt.Errorf("OIDC token endpoint returned HTTP %d", response.StatusCode)
	}
	var tokens adminTokenResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64*1024))
	if err := decoder.Decode(&tokens); err != nil {
		return adminTokenResponse{}, fmt.Errorf("decode OIDC token response: %w", err)
	}
	if tokens.AccessToken == "" || !strings.EqualFold(tokens.TokenType, "Bearer") {
		return adminTokenResponse{}, stderrors.New("OIDC token response did not contain a bearer access token")
	}
	return tokens, nil
}

func openBrowser(target string) error {
	var command string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		command, args = "open", []string{target}
	case "windows":
		command, args = "rundll32", []string{"url.dll,FileProtocolHandler", target}
	default:
		command, args = "xdg-open", []string{target}
	}
	return exec.Command(command, args...).Start()
}
