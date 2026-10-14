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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const maxAdminConfigBytes = 64 * 1024

// adminFileConfig contains only non-secret CLI defaults. Kubeconfig is a path,
// never kubeconfig content or an embedded credential.
type adminFileConfig struct {
	Kubeconfig       string `yaml:"kubeconfig"`
	Issuer           string `yaml:"issuer"`
	Subject          string `yaml:"subject"`
	BrokerURL        string `yaml:"broker-url"`
	ClusterID        string `yaml:"cluster-id"`
	BrokerCAFile     string `yaml:"broker-ca-file"`
	BrokerServerName string `yaml:"broker-server-name"`
	OIDCIssuer       string `yaml:"oidc-issuer"`
	OIDCCAFile       string `yaml:"oidc-ca-file"`
	ClientID         string `yaml:"client-id"`
	NoBrowser        bool   `yaml:"no-browser"`
	ApprovalID       string `yaml:"approval-id"`
	ApprovalReason   string `yaml:"approval-reason"`
	RequestedTTL     int64  `yaml:"requested-ttl-seconds"`
}

func (f adminFileConfig) apply(config *adminConfig) {
	config.Kubeconfig = f.Kubeconfig
	config.Issuer = f.Issuer
	config.Subject = f.Subject
	config.BrokerURL = f.BrokerURL
	config.ClusterID = f.ClusterID
	config.BrokerCAFile = f.BrokerCAFile
	config.BrokerServerName = f.BrokerServerName
	config.OIDCIssuer = f.OIDCIssuer
	config.OIDCCAFile = f.OIDCCAFile
	config.ClientID = f.ClientID
	config.NoBrowser = f.NoBrowser
	config.ApprovalID = f.ApprovalID
	config.ApprovalReason = f.ApprovalReason
	config.RequestedTTL = f.RequestedTTL
}

func loadAdminFileConfig(path string) (adminFileConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return adminFileConfig{}, fmt.Errorf("read config file: %w", err)
	}
	if len(data) > maxAdminConfigBytes {
		return adminFileConfig{}, fmt.Errorf("config file exceeds %d bytes", maxAdminConfigBytes)
	}

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var config adminFileConfig
	if err := decoder.Decode(&config); err != nil {
		if errors.Is(err, io.EOF) {
			return adminFileConfig{}, errors.New("config file is empty")
		}
		return adminFileConfig{}, fmt.Errorf("decode config file: %w", err)
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return adminFileConfig{}, errors.New("config file must contain one YAML document")
		}
		return adminFileConfig{}, fmt.Errorf("decode config file: %w", err)
	}
	return config, nil
}

func extractAdminConfigPath(args []string) (string, bool, error) {
	var path string
	found := false
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			break
		}
		switch {
		case argument == "--config" || argument == "-config":
			if index+1 >= len(args) {
				return "", false, errors.New("config path is required after --config")
			}
			index++
			path = args[index]
			found = true
		case strings.HasPrefix(argument, "--config="):
			path = strings.TrimPrefix(argument, "--config=")
			found = true
		case strings.HasPrefix(argument, "-config="):
			path = strings.TrimPrefix(argument, "-config=")
			found = true
		}
	}
	if found && strings.TrimSpace(path) == "" {
		return "", false, errors.New("config path must not be empty")
	}
	return path, found, nil
}

func adminArgsRequestHelp(args []string) bool {
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			return false
		}
		if argument == "--config" || argument == "-config" {
			index++
			continue
		}
		if strings.HasPrefix(argument, "--config=") || strings.HasPrefix(argument, "-config=") {
			continue
		}
		if argument == "--help" || argument == "-h" {
			return true
		}
	}
	return false
}

func defaultAdminConfigPath() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "sealos", "access", "admin.yaml"), nil
}

func loadAdminConfig(args []string) (adminFileConfig, string, error) {
	path, explicit, err := extractAdminConfigPath(args)
	if err != nil {
		return adminFileConfig{}, "", err
	}
	if !explicit {
		path, err = defaultAdminConfigPath()
		if err != nil {
			return adminFileConfig{}, "", fmt.Errorf("find default config path: %w", err)
		}
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return adminFileConfig{}, "", nil
			}
			return adminFileConfig{}, "", fmt.Errorf("check default config file: %w", err)
		}
	}
	config, err := loadAdminFileConfig(path)
	if err != nil {
		return adminFileConfig{}, path, fmt.Errorf("load config %q: %w", path, err)
	}
	return config, path, nil
}
