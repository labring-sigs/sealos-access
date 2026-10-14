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
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	MultiClusterConfigVersion = 1
	maxBrokerConfigBytes      = 256 * 1024
)

var targetIDPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,61}[a-z0-9])?$`)

func ValidateTargetID(value string) error {
	if !targetIDPattern.MatchString(value) {
		return errors.New("target ID must be a lowercase DNS-like value")
	}
	return nil
}

// OIDCFileConfig is the server-owned OIDC verifier configuration.
type OIDCFileConfig struct {
	Issuer     string `yaml:"issuer"`
	Audience   string `yaml:"audience"`
	JWKSURL    string `yaml:"jwksURL"`
	CAFile     string `yaml:"caFile"`
	ServerName string `yaml:"serverName"`
}

// TargetFileConfig is the non-secret target declaration. credentialFile and
// caFile point to mounted files; their contents never belong in this file.
type TargetFileConfig struct {
	ID                 string `yaml:"id"`
	APIServer          string `yaml:"apiServer"`
	ClusterServer      string `yaml:"clusterServer"`
	CAFile             string `yaml:"caFile"`
	CredentialFile     string `yaml:"credentialFile"`
	KubernetesAudience string `yaml:"kubernetesAudience"`
	UserNamespace      string `yaml:"userNamespace"`
	BrokerNamespace    string `yaml:"brokerNamespace"`
	ClusterName        string `yaml:"clusterName"`
	Enabled            bool   `yaml:"enabled"`
}

// MultiClusterFileConfig is a strict, versioned Broker configuration.
type MultiClusterFileConfig struct {
	Version int                `yaml:"version"`
	OIDC    OIDCFileConfig     `yaml:"oidc"`
	Targets []TargetFileConfig `yaml:"targets"`
}

func LoadMultiClusterConfig(path string) (MultiClusterFileConfig, error) {
	if strings.TrimSpace(path) == "" {
		return MultiClusterFileConfig{}, errors.New("Broker config path is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return MultiClusterFileConfig{}, fmt.Errorf("read Broker config: %w", err)
	}
	if len(data) > maxBrokerConfigBytes {
		return MultiClusterFileConfig{}, fmt.Errorf("Broker config exceeds %d bytes", maxBrokerConfigBytes)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var config MultiClusterFileConfig
	if err := decoder.Decode(&config); err != nil {
		if errors.Is(err, io.EOF) {
			return MultiClusterFileConfig{}, errors.New("Broker config is empty")
		}
		return MultiClusterFileConfig{}, fmt.Errorf("decode Broker config: %w", err)
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return MultiClusterFileConfig{}, errors.New("Broker config must contain one YAML document")
		}
		return MultiClusterFileConfig{}, fmt.Errorf("decode Broker config: %w", err)
	}
	if err := config.Validate(); err != nil {
		return MultiClusterFileConfig{}, err
	}
	return config, nil
}

func (c MultiClusterFileConfig) Validate() error {
	if c.Version != MultiClusterConfigVersion {
		return fmt.Errorf("unsupported Broker config version %d", c.Version)
	}
	if err := validateConfigHTTPSURL(c.OIDC.Issuer); err != nil {
		return fmt.Errorf("oidc.issuer: %w", err)
	}
	if strings.TrimSpace(c.OIDC.Audience) == "" || strings.IndexFunc(c.OIDC.Audience, unicodeSpace) >= 0 {
		return errors.New("oidc.audience must be non-empty and contain no whitespace")
	}
	if c.OIDC.JWKSURL != "" {
		if err := validateConfigHTTPSURL(c.OIDC.JWKSURL); err != nil {
			return fmt.Errorf("oidc.jwksURL: %w", err)
		}
	}
	if err := validateOptionalFile(c.OIDC.CAFile); err != nil {
		return fmt.Errorf("oidc.caFile: %w", err)
	}
	if c.OIDC.ServerName != "" && strings.TrimSpace(c.OIDC.ServerName) != c.OIDC.ServerName {
		return errors.New("oidc.serverName must not have surrounding whitespace")
	}
	if len(c.Targets) == 0 {
		return errors.New("at least one target is required")
	}
	seen := make(map[string]struct{}, len(c.Targets))
	enabled := 0
	for index, target := range c.Targets {
		if err := target.Validate(); err != nil {
			return fmt.Errorf("targets[%d]: %w", index, err)
		}
		if _, ok := seen[target.ID]; ok {
			return fmt.Errorf("duplicate target ID %q", target.ID)
		}
		seen[target.ID] = struct{}{}
		if target.Enabled {
			enabled++
		}
	}
	if enabled == 0 {
		return errors.New("at least one target must be enabled")
	}
	return nil
}

func (c TargetFileConfig) Validate() error {
	if err := ValidateTargetID(c.ID); err != nil {
		return errors.New("id must be a lowercase DNS-like target ID")
	}
	if err := validateConfigHTTPSURL(c.APIServer); err != nil {
		return fmt.Errorf("apiServer: %w", err)
	}
	if err := validateConfigHTTPSURL(c.ClusterServer); err != nil {
		return fmt.Errorf("clusterServer: %w", err)
	}
	for name, value := range map[string]string{
		"kubernetesAudience": c.KubernetesAudience,
		"userNamespace":      c.UserNamespace,
		"brokerNamespace":    c.BrokerNamespace,
	} {
		if strings.TrimSpace(value) == "" || strings.IndexFunc(value, unicodeSpace) >= 0 {
			return fmt.Errorf("%s must be non-empty and contain no whitespace", name)
		}
	}
	if err := validateRequiredFile(c.CAFile); err != nil {
		return fmt.Errorf("caFile: %w", err)
	}
	if err := validateRequiredFile(c.CredentialFile); err != nil {
		return fmt.Errorf("credentialFile: %w", err)
	}
	return nil
}

func (c TargetFileConfig) Settings(caData []byte) TargetSettings {
	clusterName := c.ClusterName
	if clusterName == "" {
		clusterName = c.ID
	}
	return TargetSettings{
		ID:                 c.ID,
		APIServer:          c.APIServer,
		ClusterServer:      c.ClusterServer,
		ClusterCAData:      append([]byte(nil), caData...),
		KubernetesAudience: c.KubernetesAudience,
		UserNamespace:      c.UserNamespace,
		BrokerNamespace:    c.BrokerNamespace,
		ClusterName:        clusterName,
		Enabled:            c.Enabled,
	}
}

func validateTargetSettings(settings TargetSettings) error {
	if err := ValidateTargetID(settings.ID); err != nil {
		return err
	}
	if settings.APIServer != "" {
		if err := validateConfigHTTPSURL(settings.APIServer); err != nil {
			return fmt.Errorf("target %q API server: %w", settings.ID, err)
		}
	}
	if err := validateConfigHTTPSURL(settings.ClusterServer); err != nil {
		return fmt.Errorf("target %q cluster server: %w", settings.ID, err)
	}
	if strings.TrimSpace(settings.KubernetesAudience) == "" || strings.IndexFunc(settings.KubernetesAudience, unicodeSpace) >= 0 {
		return fmt.Errorf("target %q Kubernetes audience is invalid", settings.ID)
	}
	if strings.TrimSpace(settings.UserNamespace) == "" || strings.TrimSpace(settings.BrokerNamespace) == "" {
		return fmt.Errorf("target %q namespaces are required", settings.ID)
	}
	return nil
}

func validateConfigHTTPSURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("must be an absolute HTTPS URL without query or fragment")
	}
	return nil
}

func validateRequiredFile(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("file path is required")
	}
	return validateOptionalFile(path)
}

func validateOptionalFile(path string) error {
	if path == "" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("file is unavailable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("file must be regular")
	}
	return nil
}

func unicodeSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r'
}
