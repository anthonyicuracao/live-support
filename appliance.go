package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// secContext is the trusted security context the platform stamps as field 0 of
// an appliance token ("SEC|payload"). The appliance trusts ONLY this — never the
// payload — for tenant, identity, and role.
type secContext struct {
	Ref  string `json:"ref"`
	User string `json:"user"`
	Role string `json:"role"`
	Exp  int64  `json:"exp"`
}

var errBadAppliance = errors.New("invalid appliance token")

// signRaw / decryptRaw sign/verify an arbitrary plaintext with the same recipe as
// the SSO tokens (key = SHA-256(secret), 12-byte nonce prepended, base64url).
func signRaw(secret, plaintext string) (string, error) {
	gcm, err := ssoGCM(secret)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, ssoNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(plaintext), nil)), nil
}

func decryptRaw(secret, tok string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil || len(raw) < ssoNonceSize {
		return "", errBadAppliance
	}
	gcm, err := ssoGCM(secret)
	if err != nil {
		return "", err
	}
	pt, err := gcm.Open(nil, raw[:ssoNonceSize], raw[ssoNonceSize:], nil)
	if err != nil {
		return "", errBadAppliance
	}
	return string(pt), nil
}

// parseAppliance verifies a platform appliance token and returns its trusted SEC
// context plus the remaining (untrusted) payload. The plaintext is
// "<sec-json>|<payload>"; SEC is JSON (never contains '|'), so it is everything
// before the first '|'.
func parseAppliance(secret, tok string) (secContext, string, error) {
	pt, err := decryptRaw(secret, tok)
	if err != nil {
		return secContext{}, "", err
	}
	secJSON, payload, _ := strings.Cut(pt, "|")
	var sec secContext
	if err := json.Unmarshal([]byte(secJSON), &sec); err != nil || sec.Ref == "" {
		return secContext{}, "", errBadAppliance
	}
	if sec.Exp != 0 && time.Now().Unix() > sec.Exp {
		return secContext{}, "", errBadAppliance
	}
	return sec, payload, nil
}

// mintApplianceToken builds a SEC|payload token — used by tests; the platform's
// signer mints the identical shape in Node.
func mintApplianceToken(secret, ref, user, role, payload string, ttl time.Duration) (string, error) {
	b, _ := json.Marshal(secContext{Ref: ref, User: user, Role: role, Exp: time.Now().Add(ttl).Unix()})
	return signRaw(secret, string(b)+"|"+payload)
}
