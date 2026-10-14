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

package internalcredentials

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ValidateIssuer enforces the identity issuer constraints that can be checked
// without the deployment's issuer allowlist.
func ValidateIssuer(issuer string) error {
	if issuer == "" || strings.TrimSpace(issuer) != issuer {
		return errors.New("issuer must be non-empty and must not contain surrounding whitespace")
	}
	parsed, err := url.Parse(issuer)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return fmt.Errorf("issuer must be an HTTPS URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("issuer must not contain userinfo, query, or fragment")
	}
	return nil
}

// ValidateIdentity validates the shape of an issuer and opaque subject pair.
func ValidateIdentity(issuer, subject string) error {
	if err := ValidateIssuer(issuer); err != nil {
		return err
	}
	if subject == "" || strings.TrimSpace(subject) != subject {
		return errors.New("subject must be non-empty and must not contain surrounding whitespace")
	}
	if len(subject) > 512 {
		return errors.New("subject must be at most 512 bytes")
	}
	return nil
}

// DeriveInternalUserName returns the stable Kubernetes-safe name for an
// issuer/subject pair. The NUL separator prevents concatenation ambiguity.
func DeriveInternalUserName(issuer, subject string) string {
	digest := sha256.Sum256([]byte(issuer + "\x00" + subject))
	return "iu-" + hex.EncodeToString(digest[:])[:24]
}

// ResourceName returns a deterministic, DNS-safe name for a lease-owned
// ServiceAccount or Binding without trusting a caller-provided object name.
func ResourceName(prefix, namespace, leaseName string) string {
	digest := sha256.Sum256([]byte(namespace + "\x00" + leaseName))
	return prefix + "-" + hex.EncodeToString(digest[:])[:24]
}
