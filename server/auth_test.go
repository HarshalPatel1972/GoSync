package server_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/HarshalPatel1972/GoSync/server"
)

// jwksServer publishes key as an identity provider would.
func jwksServer(t *testing.T, kid string, key *rsa.PublicKey) *httptest.Server {
	t.Helper()
	b64 := base64.RawURLEncoding.EncodeToString
	body, _ := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "kid": kid, "alg": "RS256", "use": "sig",
		"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
	}}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestJWKSAuthenticator(t *testing.T) {
	idpKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	attackerKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	idp := jwksServer(t, "key-1", &idpKey.PublicKey)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	auth, err := server.NewJWTAuthenticator(ctx, server.JWTConfig{
		JWKSURL:        idp.URL,
		Issuer:         "https://idp.example",
		Audience:       "gosync-api",
		NamespaceClaim: "org_id",
	})
	if err != nil {
		t.Fatal(err)
	}

	sign := func(method jwt.SigningMethod, key any, kid string, claims jwt.MapClaims) string {
		tok := jwt.NewWithClaims(method, claims)
		tok.Header["kid"] = kid
		s, err := tok.SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	claims := func(mod func(jwt.MapClaims)) jwt.MapClaims {
		c := jwt.MapClaims{
			"sub": "user-1", "org_id": "acme", "iss": "https://idp.example",
			"aud": "gosync-api", "exp": time.Now().Add(time.Hour).Unix(),
		}
		if mod != nil {
			mod(c)
		}
		return c
	}

	p, err := auth.Authenticate(ctx, sign(jwt.SigningMethodRS256, idpKey, "key-1", claims(nil)))
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if p.Subject != "user-1" || p.Namespace != "acme" || p.ExpiresAt.IsZero() {
		t.Fatalf("unexpected principal %+v", p)
	}

	bad := map[string]string{
		"wrong signing key": sign(jwt.SigningMethodRS256, attackerKey, "key-1", claims(nil)),
		"unknown kid":       sign(jwt.SigningMethodRS256, idpKey, "key-2", claims(nil)),
		"wrong issuer":      sign(jwt.SigningMethodRS256, idpKey, "key-1", claims(func(c jwt.MapClaims) { c["iss"] = "https://evil" })),
		"wrong audience":    sign(jwt.SigningMethodRS256, idpKey, "key-1", claims(func(c jwt.MapClaims) { c["aud"] = "other" })),
		"expired":           sign(jwt.SigningMethodRS256, idpKey, "key-1", claims(func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Hour).Unix() })),
		"no expiry":         sign(jwt.SigningMethodRS256, idpKey, "key-1", claims(func(c jwt.MapClaims) { delete(c, "exp") })),
		"missing namespace": sign(jwt.SigningMethodRS256, idpKey, "key-1", claims(func(c jwt.MapClaims) { delete(c, "org_id") })),
		// Classic downgrade: HS256 "signed" with the public key must not pass.
		"alg confusion": sign(jwt.SigningMethodHS256, idpKey.PublicKey.N.Bytes(), "key-1", claims(nil)),
		"alg none":      sign(jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, "key-1", claims(nil)),
	}
	for name, tok := range bad {
		if _, err := auth.Authenticate(ctx, tok); err == nil {
			t.Errorf("%s: token accepted", name)
		}
	}
}

func TestJWTConfigValidation(t *testing.T) {
	ctx := context.Background()
	cases := map[string]server.JWTConfig{
		"neither":      {},
		"both":         {Secret: make([]byte, 32), JWKSURL: "https://x"},
		"short secret": {Secret: []byte("too-short")},
	}
	for name, cfg := range cases {
		if _, err := server.NewJWTAuthenticator(ctx, cfg); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
