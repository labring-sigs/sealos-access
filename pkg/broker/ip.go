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
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
)

func (p SourceIPPolicy) Validate() error {
	if len(p.TrustedProxyCIDRs) == 0 {
		return errors.New("broker trusted proxy CIDR allowlist is required")
	}
	if len(p.AllowedClientCIDRs) == 0 {
		return errors.New("broker client source CIDR allowlist is required")
	}
	if p.TrustedClientIPHeader == "" {
		return errors.New("broker trusted client IP header is required")
	}
	if strings.TrimSpace(p.TrustedClientIPHeader) != p.TrustedClientIPHeader || strings.ContainsAny(p.TrustedClientIPHeader, " \t\r\n") {
		return errors.New("broker trusted client IP header is invalid")
	}
	return nil
}

func (p SourceIPPolicy) Authorize(r *http.Request) (net.IP, error) {
	peer, err := remoteIP(r.RemoteAddr)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy peer address: %w", err)
	}
	if !containsIP(p.TrustedProxyCIDRs, peer) {
		return nil, errors.New("request peer is not a trusted Higress egress")
	}

	values := r.Header.Values(p.TrustedClientIPHeader)
	if len(values) != 1 || values[0] == "" || strings.TrimSpace(values[0]) != values[0] {
		return nil, errors.New("missing or ambiguous trusted client IP")
	}
	clientIP := net.ParseIP(values[0])
	if clientIP == nil {
		return nil, errors.New("trusted client IP is malformed")
	}
	if !containsIP(p.AllowedClientCIDRs, clientIP) {
		return nil, errors.New("client IP is not allowed")
	}
	return clientIP, nil
}

func remoteIP(address string) (net.IP, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, errors.New("address is not an IP address")
	}
	return ip, nil
}

func containsIP(cidrs []*net.IPNet, ip net.IP) bool {
	for _, cidr := range cidrs {
		if cidr != nil && cidr.Contains(ip) {
			return true
		}
	}
	return false
}

func ParseCIDRs(values []string) ([]*net.IPNet, error) {
	result := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		_, network, err := net.ParseCIDR(value)
		if err != nil {
			return nil, fmt.Errorf("parse source CIDR %q: %w", value, err)
		}
		result = append(result, network)
	}
	return result, nil
}
