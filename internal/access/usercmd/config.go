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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const maxCLIConfigBytes = 64 * 1024

// cliFileConfig contains only non-secret CLI defaults. In particular, it does
// not contain an OIDC secret, token, kubeconfig, or one-time approval handle.
type cliFileConfig struct {
	BrokerURL        string `yaml:"broker-url"`
	ClusterID        string `yaml:"cluster-id"`
	BrokerCAFile     string `yaml:"broker-ca-file"`
	BrokerServerName string `yaml:"broker-server-name"`
	OIDCIssuer       string `yaml:"oidc-issuer"`
	OIDCCAFile       string `yaml:"oidc-ca-file"`
	OIDCServerName   string `yaml:"oidc-server-name"`
	ClientID         string `yaml:"client-id"`
	Output           string `yaml:"output"`
	RequestedTTL     int64  `yaml:"requested-ttl-seconds"`
	NoBrowser        bool   `yaml:"no-browser"`
	LoginHint        string `yaml:"login-hint"`
}

func (f cliFileConfig) apply(config *cliConfig) {
	config.BrokerURL = f.BrokerURL
	config.ClusterID = f.ClusterID
	config.BrokerCAFile = f.BrokerCAFile
	config.BrokerServerName = f.BrokerServerName
	config.OIDCIssuer = f.OIDCIssuer
	config.OIDCCAFile = f.OIDCCAFile
	config.OIDCServerName = f.OIDCServerName
	config.ClientID = f.ClientID
	config.Output = f.Output
	config.outputFromConfig = f.Output != ""
	config.RequestedTTL = f.RequestedTTL
	config.NoBrowser = f.NoBrowser
	config.LoginHint = f.LoginHint
}

func loadCLIFileConfig(path string) (cliFileConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return cliFileConfig{}, fmt.Errorf("read config file: %w", err)
	}
	if len(data) > maxCLIConfigBytes {
		return cliFileConfig{}, fmt.Errorf("config file exceeds %d bytes", maxCLIConfigBytes)
	}

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var config cliFileConfig
	if err := decoder.Decode(&config); err != nil {
		if errors.Is(err, io.EOF) {
			return cliFileConfig{}, errors.New("config file is empty")
		}
		return cliFileConfig{}, fmt.Errorf("decode config file: %w", err)
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return cliFileConfig{}, errors.New("config file must contain one YAML document")
		}
		return cliFileConfig{}, fmt.Errorf("decode config file: %w", err)
	}
	return config, nil
}

func extractCLIConfigPath(args []string) (string, bool, error) {
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

func cliArgsRequestHelp(args []string) bool {
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

func defaultCLIConfigPath() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "sealos", "access", "user.yaml"), nil
}

func loadCLIConfig(args []string) (cliFileConfig, string, error) {
	path, explicit, err := extractCLIConfigPath(args)
	if err != nil {
		return cliFileConfig{}, "", err
	}
	if !explicit {
		path, err = defaultCLIConfigPath()
		if err != nil {
			return cliFileConfig{}, "", fmt.Errorf("find default config path: %w", err)
		}
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return cliFileConfig{}, "", nil
			}
			return cliFileConfig{}, "", fmt.Errorf("check default config file: %w", err)
		}
	}
	config, err := loadCLIFileConfig(path)
	if err != nil {
		return cliFileConfig{}, path, fmt.Errorf("load config %q: %w", path, err)
	}
	return config, path, nil
}
