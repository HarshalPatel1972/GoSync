package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
)

// Principal is an authenticated caller.
type Principal struct {
	// Subject identifies the user (used in logs).
	Subject string
	// Namespace is the data partition the connection reads and writes.
	// Usually equal to Subject (per-user data); use a tenant or workspace
	// claim to share data between users.
	Namespace string
	// ExpiresAt, when set, is when the server will drop the connection so the
	// client reconnects with a fresh token.
	ExpiresAt time.Time
}

// Authenticator turns the token from a client's hello into a Principal.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (Principal, error)
}

// AuthenticatorFunc adapts a function to Authenticator.
type AuthenticatorFunc func(ctx context.Context, token string) (Principal, error)

func (f AuthenticatorFunc) Authenticate(ctx context.Context, token string) (Principal, error) {
	return f(ctx, token)
}

// JWTConfig configures JWT authentication. Set exactly one of Secret (HS256)
// or JWKSURL (RS256/ES256/EdDSA keys published by an identity provider such as
// Auth0, Clerk, Supabase, Firebase or Cognito).
type JWTConfig struct {
	Secret   []byte
	JWKSURL  string
	Issuer   string // required "iss" when non-empty
	Audience string // required "aud" when non-empty
	// NamespaceClaim names the claim holding the namespace. Default "sub".
	NamespaceClaim string
}

type jwtAuth struct {
	cfg     JWTConfig
	keyFunc jwt.Keyfunc
	parser  *jwt.Parser
}

// NewJWTAuthenticator builds a JWT Authenticator. With JWKSURL the key set is
// fetched immediately and refreshed in the background until ctx is done.
func NewJWTAuthenticator(ctx context.Context, cfg JWTConfig) (Authenticator, error) {
	if (len(cfg.Secret) == 0) == (cfg.JWKSURL == "") {
		return nil, errors.New("jwt: set exactly one of Secret or JWKSURL")
	}
	if cfg.NamespaceClaim == "" {
		cfg.NamespaceClaim = "sub"
	}
	opts := []jwt.ParserOption{jwt.WithExpirationRequired(), jwt.WithLeeway(30 * time.Second)}
	if cfg.Issuer != "" {
		opts = append(opts, jwt.WithIssuer(cfg.Issuer))
	}
	if cfg.Audience != "" {
		opts = append(opts, jwt.WithAudience(cfg.Audience))
	}
	a := &jwtAuth{cfg: cfg}
	if len(cfg.Secret) > 0 {
		if len(cfg.Secret) < 32 {
			return nil, errors.New("jwt: secret must be at least 32 bytes")
		}
		opts = append(opts, jwt.WithValidMethods([]string{"HS256", "HS384", "HS512"}))
		a.keyFunc = func(*jwt.Token) (any, error) { return cfg.Secret, nil }
	} else {
		kf, err := keyfunc.NewDefaultCtx(ctx, []string{cfg.JWKSURL})
		if err != nil {
			return nil, fmt.Errorf("jwt: load JWKS: %w", err)
		}
		opts = append(opts, jwt.WithValidMethods([]string{"RS256", "RS384", "RS512", "PS256", "ES256", "ES384", "EdDSA"}))
		a.keyFunc = kf.Keyfunc
	}
	a.parser = jwt.NewParser(opts...)
	return a, nil
}

func (a *jwtAuth) Authenticate(_ context.Context, token string) (Principal, error) {
	claims := jwt.MapClaims{}
	if _, err := a.parser.ParseWithClaims(token, claims, a.keyFunc); err != nil {
		return Principal{}, err
	}
	sub, _ := claims.GetSubject()
	ns, _ := claims[a.cfg.NamespaceClaim].(string)
	if ns == "" {
		return Principal{}, fmt.Errorf("jwt: missing %q claim", a.cfg.NamespaceClaim)
	}
	p := Principal{Subject: sub, Namespace: ns}
	if exp, err := claims.GetExpirationTime(); err == nil && exp != nil {
		p.ExpiresAt = exp.Time
	}
	return p, nil
}

// InsecureDevAuthenticator trusts the token as the user ID. It exists so the
// demo runs without an identity provider. Never use it in production: anyone
// can read and write any user's data.
func InsecureDevAuthenticator() Authenticator {
	return AuthenticatorFunc(func(_ context.Context, token string) (Principal, error) {
		if token == "" || len(token) > 128 {
			return Principal{}, errors.New("dev auth: token must be a 1-128 byte user id")
		}
		return Principal{Subject: token, Namespace: token}, nil
	})
}
