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
	"fmt"
	"math"
	"time"
)

const (
	BaseReadonlyRoleName    = "internal-user-base-readonly-v1"
	ClusterOpsWriteRoleName = "internal-user-cluster-ops-write-v1"

	MinimumCredentialTTL  = 10 * time.Minute
	BaseReadonlyMaxTTL    = 24 * time.Hour
	ClusterOpsWriteMaxTTL = time.Hour
)

// Profile is the server-owned policy metadata for a credential profile.
// Callers can select Name, but never provide RoleName or permission rules.
type Profile struct {
	Name              string
	RoleName          string
	MaxTTL            time.Duration
	Elevated          bool
	UsesStableAccount bool
}

func ProfileFor(name string) (Profile, bool) {
	switch name {
	case "base-readonly.v1":
		return Profile{
			Name:              "base-readonly.v1",
			RoleName:          BaseReadonlyRoleName,
			MaxTTL:            BaseReadonlyMaxTTL,
			UsesStableAccount: true,
		}, true
	case "cluster-ops-write.v1":
		return Profile{
			Name:     "cluster-ops-write.v1",
			RoleName: ClusterOpsWriteRoleName,
			MaxTTL:   ClusterOpsWriteMaxTTL,
			Elevated: true,
		}, true
	default:
		return Profile{}, false
	}
}

func ValidateRequestedTTL(profileName string, requestedSeconds int64) (time.Duration, error) {
	profile, ok := ProfileFor(profileName)
	if !ok {
		return 0, fmt.Errorf("unsupported credential profile %q", profileName)
	}
	if requestedSeconds <= 0 {
		return 0, fmt.Errorf("requested TTL must be positive")
	}
	if requestedSeconds > math.MaxInt64/int64(time.Second) {
		return 0, fmt.Errorf("requested TTL is too large")
	}
	requested := time.Duration(requestedSeconds) * time.Second
	if requested < MinimumCredentialTTL {
		return 0, fmt.Errorf("requested TTL must be at least %s", MinimumCredentialTTL)
	}
	if requested > profile.MaxTTL {
		return 0, fmt.Errorf("requested TTL exceeds %s maximum", profile.MaxTTL)
	}
	return requested, nil
}
