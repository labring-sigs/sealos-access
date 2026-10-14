package internalcredentials

import (
	"testing"
	"time"
)

func TestValidateRequestedTTL(t *testing.T) {
	tests := []struct {
		name    string
		profile string
		seconds int64
		wantErr bool
	}{
		{name: "base at minimum", profile: "base-readonly.v1", seconds: int64(MinimumCredentialTTL / time.Second)},
		{name: "base at maximum", profile: "base-readonly.v1", seconds: int64(BaseReadonlyMaxTTL / time.Second)},
		{name: "elevated at maximum", profile: "cluster-ops-write.v1", seconds: int64(ClusterOpsWriteMaxTTL / time.Second)},
		{name: "too short", profile: "base-readonly.v1", seconds: int64((MinimumCredentialTTL - time.Second) / time.Second), wantErr: true},
		{name: "elevated too long", profile: "cluster-ops-write.v1", seconds: int64((ClusterOpsWriteMaxTTL + time.Second) / time.Second), wantErr: true},
		{name: "unknown profile", profile: "unknown.v1", seconds: 600, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ValidateRequestedTTL(test.profile, test.seconds)
			if (err != nil) != test.wantErr {
				t.Fatalf("ValidateRequestedTTL() error = %v, wantErr %t", err, test.wantErr)
			}
		})
	}
}

func TestDeriveInternalUserNameIsDeterministicAndSeparated(t *testing.T) {
	first := DeriveInternalUserName("https://issuer.example", "alice")
	second := DeriveInternalUserName("https://issuer.example", "alice")
	if first != second {
		t.Fatalf("derived name is not deterministic: %q != %q", first, second)
	}
	if first == DeriveInternalUserName("https://issuer.examplea", "lice") {
		t.Fatal("different issuer/subject pair produced the same derived name")
	}
	if len(first) != len("iu-")+24 {
		t.Fatalf("derived name length = %d, want %d", len(first), len("iu-")+24)
	}
}
