package repository

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func TestNewIdempotencyClaimAllowsRequestsWithoutAKey(t *testing.T) {
	claim, err := NewIdempotencyClaim(CreateOptions{Scope: "ignored", Fingerprint: "ignored"})
	if err != nil || claim != nil {
		t.Fatalf("claim=%#v err=%v, want nil claim and error", claim, err)
	}
}

func TestNewIdempotencyClaimHashesOpaqueScope(t *testing.T) {
	fingerprint := sha256.Sum256([]byte("request"))
	scope := "alice\x00uid-1\x00default"
	claim, err := NewIdempotencyClaim(CreateOptions{
		IdempotencyKey: "request-1",
		Scope:          scope,
		Fingerprint:    hex.EncodeToString(fingerprint[:]),
	})
	if err != nil {
		t.Fatal(err)
	}
	if claim.Key != "request-1" {
		t.Fatalf("key=%q, want request-1", claim.Key)
	}
	if claim.ScopeDigest != sha256.Sum256([]byte(scope)) {
		t.Fatal("scope digest does not match the opaque scope")
	}
	if claim.RequestFingerprint != fingerprint {
		t.Fatal("request fingerprint was not decoded")
	}
}

func TestNewIdempotencyClaimRejectsInvalidInput(t *testing.T) {
	validFingerprint := strings.Repeat("a", sha256.Size*2)
	tests := map[string]CreateOptions{
		"unsupported key character": {IdempotencyKey: "bad key", Scope: "scope", Fingerprint: validFingerprint},
		"oversized key":             {IdempotencyKey: strings.Repeat("k", maxIdempotencyKeyLength+1), Scope: "scope", Fingerprint: validFingerprint},
		"missing scope":             {IdempotencyKey: "key", Fingerprint: validFingerprint},
		"oversized scope":           {IdempotencyKey: "key", Scope: strings.Repeat("s", maxIdempotencyScopeLength+1), Fingerprint: validFingerprint},
		"short fingerprint":         {IdempotencyKey: "key", Scope: "scope", Fingerprint: "abcd"},
		"uppercase fingerprint":     {IdempotencyKey: "key", Scope: "scope", Fingerprint: strings.Repeat("A", sha256.Size*2)},
		"non-hex fingerprint":       {IdempotencyKey: "key", Scope: "scope", Fingerprint: strings.Repeat("z", sha256.Size*2)},
	}
	for name, options := range tests {
		t.Run(name, func(t *testing.T) {
			claim, err := NewIdempotencyClaim(options)
			if claim != nil || !errors.Is(err, ErrInvalidIdempotency) {
				t.Fatalf("claim=%#v err=%v, want ErrInvalidIdempotency", claim, err)
			}
		})
	}
}
