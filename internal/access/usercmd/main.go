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
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	"syscall"
	"time"
	"unicode"

	"github.com/labring-sigs/sealos-access/pkg/broker"
)

const (
	defaultCallbackPath = "/callback"
	callbackTimeout     = 5 * time.Minute
	elevatedProfile     = "cluster-ops-write.v1"
)

type cliOperation string

const (
	operationBase           cliOperation = "base"
	operationElevatedRedeem cliOperation = "elevated-redeem"
)

type cliConfig struct {
	Operation          cliOperation
	ConfigFile         string
	BrokerURL          string
	ClusterID          string
	BrokerCAFile       string
	BrokerServerName   string
	OIDCIssuer         string
	OIDCCAFile         string
	OIDCServerName     string
	ClientID           string
	Output             string
	RequestedTTL       int64
	ApprovalReference  string
	NoBrowser          bool
	LoginHint          string
	HTTPClient         *http.Client
	CallbackTimeout    time.Duration
	BrowserCommandFunc func(string) error
	Stdout             io.Writer
	Stderr             io.Writer
	outputFromConfig   bool
	outputFromCLI      bool
}

type oidcDiscovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	IDToken     string `json:"id_token"`
}

type brokerResponse struct {
	ClusterID           string    `json:"clusterID"`
	LeaseID             string    `json:"leaseID"`
	Profile             string    `json:"profile"`
	Username            string    `json:"username"`
	ExpirationTimestamp time.Time `json:"expirationTimestamp"`
	Kubeconfig          string    `json:"kubeconfig"`
}

type authorizationState struct {
	State        string
	Nonce        string
	CodeVerifier string
	RedirectURI  string
}

type callbackResult struct {
	Code        string
	State       string
	Error       string
	Description string
}

func Main(args []string, stdout, stderr io.Writer) int {
	config, err := parseCLIArgs(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
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

func parseCLIArgs(args []string) (cliConfig, error) {
	config := cliConfig{Operation: operationBase}
	flagArgs := args
	if len(args) > 0 && args[0] == "elevated" {
		if len(args) < 2 {
			return cliConfig{}, errors.New("elevated requires the redeem subcommand")
		}
		switch args[1] {
		case "redeem":
			config.Operation = operationElevatedRedeem
		default:
			return cliConfig{}, fmt.Errorf("unknown elevated subcommand %q", args[1])
		}
		flagArgs = args[2:]
	}
	configPath, _, err := extractCLIConfigPath(flagArgs)
	if err != nil {
		return cliConfig{}, err
	}
	var fileConfig cliFileConfig
	if !cliArgsRequestHelp(flagArgs) {
		fileConfig, configPath, err = loadCLIConfig(flagArgs)
		if err != nil {
			return cliConfig{}, err
		}
	}
	config.ConfigFile = configPath
	fileConfig.apply(&config)
	flags := newCLIFlagSet(&config)
	if err := flags.Parse(flagArgs); err != nil {
		return cliConfig{}, err
	}
	if flags.NArg() != 0 {
		return cliConfig{}, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	flags.Visit(func(visited *flag.Flag) {
		if visited.Name == "output" {
			config.outputFromCLI = true
			config.outputFromConfig = false
		}
	})
	return config, nil
}

func newCLIFlagSet(config *cliConfig) *flag.FlagSet {
	flags := flag.NewFlagSet("sealos-access", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.StringVar(&config.ConfigFile, "config", config.ConfigFile, "YAML config file; defaults to the standard user config path when present.")
	flags.StringVar(&config.BrokerURL, "broker-url", config.BrokerURL, "HTTPS URL of the credential Broker.")
	flags.StringVar(&config.ClusterID, "cluster-id", config.ClusterID, "Immutable target Kubernetes cluster ID.")
	flags.StringVar(&config.BrokerCAFile, "broker-ca-file", config.BrokerCAFile, "CA file used to verify the Broker certificate.")
	flags.StringVar(&config.BrokerServerName, "broker-server-name", config.BrokerServerName, "TLS server name used for the Broker connection.")
	flags.StringVar(&config.OIDCIssuer, "oidc-issuer", config.OIDCIssuer, "HTTPS OIDC issuer URL.")
	flags.StringVar(&config.OIDCCAFile, "oidc-ca-file", config.OIDCCAFile, "CA file used to verify the OIDC issuer certificate.")
	flags.StringVar(&config.OIDCServerName, "oidc-server-name", config.OIDCServerName, "TLS server name used for the OIDC connection.")
	flags.StringVar(&config.ClientID, "client-id", config.ClientID, "Public OIDC client ID.")
	flags.StringVar(&config.Output, "output", config.Output, "Output path for the one-time kubeconfig.")
	flags.BoolVar(&config.NoBrowser, "no-browser", config.NoBrowser, "Print the authorization URL instead of opening a browser.")
	flags.StringVar(&config.LoginHint, "login-hint", config.LoginHint, "Optional OIDC login hint for the configured test or identity provider.")
	if config.Operation == operationBase {
		flags.Int64Var(&config.RequestedTTL, "requested-ttl-seconds", config.RequestedTTL, "Requested credential lifetime in seconds.")
	} else {
		flags.StringVar(&config.ApprovalReference, "approval-reference", config.ApprovalReference, "Approved one-time reference for elevated redemption.")
	}
	return flags
}

func run(ctx context.Context, config cliConfig) error {
	if err := validateConfig(config); err != nil {
		return err
	}
	if config.Stdout == nil {
		config.Stdout = io.Discard
	}
	if config.Stderr == nil {
		config.Stderr = io.Discard
	}
	if config.CallbackTimeout <= 0 {
		config.CallbackTimeout = callbackTimeout
	}
	if config.BrowserCommandFunc == nil {
		config.BrowserCommandFunc = openBrowser
	}

	oidcClient, err := newHTTPClient(config.OIDCCAFile, config.OIDCServerName)
	if err != nil {
		return fmt.Errorf("create OIDC HTTP client: %w", err)
	}
	brokerClient, err := newHTTPClient(config.BrokerCAFile, config.BrokerServerName)
	if err != nil {
		return fmt.Errorf("create Broker HTTP client: %w", err)
	}
	discovery, err := fetchDiscovery(ctx, oidcClient, config.OIDCIssuer)
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("start loopback callback: %w", err)
	}
	defer listener.Close()
	state, err := newAuthorizationState("http://" + listener.Addr().String() + defaultCallbackPath)
	if err != nil {
		return fmt.Errorf("create authorization state: %w", err)
	}
	authorizationURL, err := buildAuthorizationURL(discovery.AuthorizationEndpoint, config.ClientID, config.LoginHint, state)
	if err != nil {
		return fmt.Errorf("build authorization URL: %w", err)
	}

	callback := make(chan callbackResult, 1)
	callbackServer := &http.Server{Handler: callbackHandler(state.State, callback), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if serveErr := callbackServer.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			select {
			case callback <- callbackResult{Error: "callback server failed"}:
			default:
			}
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = callbackServer.Shutdown(shutdownCtx)
	}()

	if config.NoBrowser {
		fmt.Fprintln(config.Stdout, authorizationURL)
	} else if err := config.BrowserCommandFunc(authorizationURL); err != nil {
		fmt.Fprintf(config.Stderr, "open the authorization URL manually: %s\n", authorizationURL)
	}

	callbackCtx, cancel := context.WithTimeout(ctx, config.CallbackTimeout)
	defer cancel()
	var result callbackResult
	select {
	case result = <-callback:
	case <-callbackCtx.Done():
		return errors.New("OIDC authorization timed out")
	}
	if result.Error != "" {
		if result.Description != "" {
			return fmt.Errorf("OIDC authorization failed: %s", result.Description)
		}
		return fmt.Errorf("OIDC authorization failed: %s", result.Error)
	}
	if result.State != state.State {
		return errors.New("OIDC authorization state validation failed")
	}
	if result.Code == "" {
		return errors.New("OIDC authorization did not return a code")
	}
	tokens, err := exchangeAuthorizationCode(ctx, oidcClient, discovery.TokenEndpoint, config.ClientID, result.Code, state)
	if err != nil {
		return err
	}
	if err := validateIDTokenNonce(tokens.IDToken, state.Nonce); err != nil {
		return err
	}
	switch config.Operation {
	case operationElevatedRedeem:
		response, err := redeemElevatedCredentialForCluster(ctx, brokerClient, config.BrokerURL, config.ClusterID, tokens.AccessToken, config.ApprovalReference)
		if err != nil {
			return err
		}
		return writeCredentialResponse(config, response)
	case operationBase:
		response, err := issueBaseCredentialForCluster(ctx, brokerClient, config.BrokerURL, config.ClusterID, tokens.AccessToken, config.RequestedTTL)
		if err != nil {
			return err
		}
		return writeCredentialResponse(config, response)
	default:
		return errors.New("unsupported CLI operation")
	}
}

func validateConfig(config cliConfig) error {
	for _, required := range []struct {
		name  string
		value string
	}{
		{name: "broker URL", value: config.BrokerURL},
		{name: "OIDC issuer", value: config.OIDCIssuer},
		{name: "client ID", value: config.ClientID},
		{name: "target cluster ID", value: config.ClusterID},
	} {
		if strings.TrimSpace(required.value) == "" {
			return fmt.Errorf("%s is required", required.name)
		}
	}
	if err := validateHTTPSURL(config.BrokerURL); err != nil {
		return fmt.Errorf("invalid Broker URL: %w", err)
	}
	if err := validateHTTPSURL(config.OIDCIssuer); err != nil {
		return fmt.Errorf("invalid OIDC issuer: %w", err)
	}
	if err := broker.ValidateTargetID(config.ClusterID); err != nil {
		return fmt.Errorf("invalid target cluster ID: %w", err)
	}
	switch config.Operation {
	case operationBase, operationElevatedRedeem:
		if strings.TrimSpace(config.Output) == "" {
			return errors.New("output path is required")
		}
		if config.Output == "-" {
			return errors.New("output path must name a local file")
		}
	default:
		return errors.New("unsupported CLI operation")
	}
	if config.Operation == operationElevatedRedeem {
		if err := validateCLIApprovalReference(config.ApprovalReference); err != nil {
			return err
		}
	} else if config.ApprovalReference != "" {
		return errors.New("approval reference is only used by elevated redeem")
	}
	if config.RequestedTTL < 0 {
		return errors.New("requested TTL must not be negative")
	}
	return nil
}

func validateCLIApprovalReference(reference string) error {
	if reference == "" || strings.TrimSpace(reference) != reference || strings.IndexFunc(reference, unicode.IsSpace) >= 0 || len(reference) > 512 {
		return errors.New("approval reference is invalid")
	}
	return nil
}

func validateHTTPSURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("must be an absolute HTTPS URL without query or fragment")
	}
	return nil
}

func newHTTPClient(caFile, serverName string) (*http.Client, error) {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		transport = &http.Transport{}
	}
	transport = transport.Clone()
	if caFile != "" || serverName != "" {
		config := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}
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
				return nil, errors.New("CA file contains no certificates")
			}
			config.RootCAs = pool
		}
		transport.TLSClientConfig = config
	}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second}, nil
}

func fetchDiscovery(ctx context.Context, httpClient *http.Client, issuer string) (oidcDiscovery, error) {
	endpoint := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
	var discovery oidcDiscovery
	if err := getJSON(ctx, httpClient, endpoint, &discovery); err != nil {
		return oidcDiscovery{}, fmt.Errorf("fetch OIDC discovery: %w", err)
	}
	if discovery.Issuer != strings.TrimRight(issuer, "/") {
		return oidcDiscovery{}, errors.New("OIDC discovery issuer does not match configuration")
	}
	if err := validateHTTPSURL(discovery.AuthorizationEndpoint); err != nil {
		return oidcDiscovery{}, fmt.Errorf("invalid OIDC authorization endpoint: %w", err)
	}
	if err := validateHTTPSURL(discovery.TokenEndpoint); err != nil {
		return oidcDiscovery{}, fmt.Errorf("invalid OIDC token endpoint: %w", err)
	}
	return discovery, nil
}

func getJSON(ctx context.Context, httpClient *http.Client, endpoint string, target interface{}) error {
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

func newAuthorizationState(redirectURI string) (authorizationState, error) {
	state, err := randomURLValue(32)
	if err != nil {
		return authorizationState{}, err
	}
	nonce, err := randomURLValue(32)
	if err != nil {
		return authorizationState{}, err
	}
	verifier, err := randomURLValue(32)
	if err != nil {
		return authorizationState{}, err
	}
	return authorizationState{State: state, Nonce: nonce, CodeVerifier: verifier, RedirectURI: redirectURI}, nil
}

func randomURLValue(size int) (string, error) {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func buildAuthorizationURL(endpoint, clientID, loginHint string, state authorizationState) (string, error) {
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
	query.Set("code_challenge", codeChallenge(state.CodeVerifier))
	query.Set("code_challenge_method", "S256")
	if loginHint != "" {
		query.Set("login_hint", loginHint)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func codeChallenge(verifier string) string {
	digest := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func callbackHandler(expectedState string, result chan<- callbackResult) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(defaultCallbackPath, func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		query := request.URL.Query()
		callback := callbackResult{Code: query.Get("code"), State: query.Get("state"), Error: query.Get("error"), Description: query.Get("error_description")}
		if callback.State != expectedState {
			callback = callbackResult{Error: "state validation failed", State: callback.State}
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

func exchangeAuthorizationCode(ctx context.Context, httpClient *http.Client, endpoint, clientID, code string, state authorizationState) (tokenResponse, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"redirect_uri":  {state.RedirectURI},
		"code_verifier": {state.CodeVerifier},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, fmt.Errorf("create OIDC token request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := httpClient.Do(request)
	if err != nil {
		return tokenResponse{}, fmt.Errorf("exchange OIDC authorization code: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return tokenResponse{}, fmt.Errorf("OIDC token endpoint returned HTTP %d", response.StatusCode)
	}
	var tokens tokenResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64*1024))
	if err := decoder.Decode(&tokens); err != nil {
		return tokenResponse{}, fmt.Errorf("decode OIDC token response: %w", err)
	}
	if tokens.AccessToken == "" || !strings.EqualFold(tokens.TokenType, "Bearer") {
		return tokenResponse{}, errors.New("OIDC token response did not contain a bearer access token")
	}
	return tokens, nil
}

func validateIDTokenNonce(idToken, expectedNonce string) error {
	if idToken == "" {
		return errors.New("OIDC token response did not contain an ID token")
	}
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return errors.New("OIDC ID token is malformed")
	}
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return errors.New("OIDC ID token is malformed")
	}
	var claims struct {
		Nonce string `json:"nonce"`
	}
	if err := json.Unmarshal(claimsJSON, &claims); err != nil || claims.Nonce != expectedNonce {
		return errors.New("OIDC ID token nonce validation failed")
	}
	return nil
}

func issueBaseCredentialForCluster(ctx context.Context, httpClient *http.Client, brokerURL, clusterID, accessToken string, requestedTTL int64) (brokerResponse, error) {
	if err := broker.ValidateTargetID(clusterID); err != nil {
		return brokerResponse{}, fmt.Errorf("invalid target cluster: %w", err)
	}
	body, err := json.Marshal(struct {
		RequestedTTLSeconds int64 `json:"requestedTTLSeconds"`
	}{RequestedTTLSeconds: requestedTTL})
	if err != nil {
		return brokerResponse{}, errors.New("encode Broker request")
	}
	endpoint := brokerEndpoint(brokerURL, clusterID, "/credentials/base")
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return brokerResponse{}, fmt.Errorf("create Broker request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := httpClient.Do(request)
	if err != nil {
		return brokerResponse{}, fmt.Errorf("request base credential: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return brokerResponse{}, fmt.Errorf("Broker returned HTTP %d", response.StatusCode)
	}
	var result brokerResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, 2*1024*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return brokerResponse{}, fmt.Errorf("decode Broker response: %w", err)
	}
	return result, nil
}

func redeemElevatedCredentialForCluster(ctx context.Context, httpClient *http.Client, brokerURL, clusterID, accessToken, approvalReference string) (brokerResponse, error) {
	if err := broker.ValidateTargetID(clusterID); err != nil {
		return brokerResponse{}, fmt.Errorf("invalid target cluster: %w", err)
	}
	if err := validateCLIApprovalReference(approvalReference); err != nil {
		return brokerResponse{}, err
	}
	body, err := json.Marshal(struct {
		ApprovalReference string `json:"approvalReference"`
	}{ApprovalReference: approvalReference})
	if err != nil {
		return brokerResponse{}, errors.New("encode Broker request")
	}
	endpoint := brokerEndpoint(brokerURL, clusterID, "/credentials/elevated/redeem")
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return brokerResponse{}, fmt.Errorf("create Broker request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := httpClient.Do(request)
	if err != nil {
		return brokerResponse{}, fmt.Errorf("redeem elevated credential: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return brokerResponse{}, fmt.Errorf("Broker returned HTTP %d", response.StatusCode)
	}
	var result brokerResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, 2*1024*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return brokerResponse{}, fmt.Errorf("decode Broker response: %w", err)
	}
	return result, nil
}

func validateBrokerResponse(response brokerResponse) error {
	if response.LeaseID == "" || response.Profile == "" || response.Username == "" || response.Kubeconfig == "" || response.ExpirationTimestamp.IsZero() {
		return errors.New("Broker returned an incomplete credential response")
	}
	return nil
}

func brokerEndpoint(brokerURL, clusterID, suffix string) string {
	base := strings.TrimRight(brokerURL, "/")
	return base + "/v1/clusters/" + url.PathEscape(clusterID) + suffix
}

func writeCredentialResponse(config cliConfig, response brokerResponse) error {
	if err := validateBrokerResponse(response); err != nil {
		return err
	}
	if response.ClusterID != config.ClusterID {
		return errors.New("Broker returned a credential for an unexpected target cluster")
	}
	if err := writeKubeconfig(config.Output, []byte(response.Kubeconfig)); err != nil {
		return fmt.Errorf("write kubeconfig: %w", err)
	}
	fmt.Fprintf(config.Stdout, "issuer: %s\ncluster ID: %s\nusername: %s\nprofile: %s\nlease ID: %s\nexpiration: %s\n", config.OIDCIssuer, config.ClusterID, response.Username, response.Profile, response.LeaseID, response.ExpirationTimestamp.UTC().Format(time.RFC3339))
	return nil
}

func writeKubeconfig(path string, data []byte) error {
	if path == "" {
		return errors.New("output path is required")
	}
	if len(data) == 0 {
		return errors.New("kubeconfig is empty")
	}
	info, err := os.Lstat(path)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing symlink output path")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return errors.New("refusing symlink output path")
		}
		return err
	}
	defer file.Close()
	if err := file.Chmod(0600); err != nil {
		return err
	}
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		if err != nil {
			return err
		}
		return errors.New("output path is not a regular file")
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	return file.Sync()
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
