package googleapi

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/99designs/keyring"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"

	"github.com/steipete/gogcli/internal/authclient"
	"github.com/steipete/gogcli/internal/config"
	"github.com/steipete/gogcli/internal/googleauth"
	"github.com/steipete/gogcli/internal/secrets"
)

const (
	// responseHeaderTimeout limits the time waiting for the server to begin
	// responding (send response headers). Once headers arrive and the body
	// starts streaming, there is no hard cap — large file downloads are not
	// cut short. This replaces the former http.Client.Timeout which applied
	// to the entire request lifecycle and caused timeouts on large Drive
	// file downloads.
	responseHeaderTimeout = 30 * time.Second

	// tokenExchangeTimeout is applied to the short-lived HTTP client used
	// for OAuth2 token refresh exchanges, which should always be fast.
	tokenExchangeTimeout = 30 * time.Second
)

var (
	readClientCredentials = config.ReadClientCredentialsFor
	openSecretsStore      = secrets.OpenDefault
)

type persistingTokenSource struct {
	base   oauth2.TokenSource
	store  secrets.Store
	client string
	email  string

	mu  sync.Mutex
	tok secrets.Token
}

func newPersistingTokenSource(base oauth2.TokenSource, store secrets.Store, client string, email string, tok secrets.Token) oauth2.TokenSource {
	return &persistingTokenSource{
		base:   base,
		store:  store,
		client: client,
		email:  email,
		tok:    tok,
	}
}

func (p *persistingTokenSource) Token() (*oauth2.Token, error) {
	t, err := p.base.Token()
	if err != nil {
		return nil, fmt.Errorf("base token source: %w", err)
	}

	refreshToken := strings.TrimSpace(t.RefreshToken)
	if refreshToken == "" {
		return t, nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if refreshToken == p.tok.RefreshToken {
		return t, nil
	}

	updated := p.tok
	updated.RefreshToken = refreshToken

	if err := p.store.SetToken(p.client, p.email, updated); err != nil {
		slog.Warn("persist rotated refresh token failed", "email", p.email, "client", p.client, "err", err)
		return t, nil
	}

	p.tok = updated
	slog.Debug("persisted rotated refresh token", "email", p.email, "client", p.client)

	return t, nil
}

func tokenSourceForAccount(ctx context.Context, service googleauth.Service, email string) (oauth2.TokenSource, error) {
	// If GOG_ACCESS_TOKEN is set, use it directly as a static token source.
	// This bypasses the normal OAuth2 refresh flow, useful when an external
	// auth provider (e.g. Composio) manages token lifecycle.
	if accessToken := os.Getenv("GOG_ACCESS_TOKEN"); accessToken != "" {
		slog.Debug("using static access token from GOG_ACCESS_TOKEN env var", "email", email)

		return oauth2.StaticTokenSource(&oauth2.Token{
			AccessToken: accessToken,
			TokenType:   "Bearer",
		}), nil
	}

	client, err := authclient.ResolveClient(ctx, email)
	if err != nil {
		return nil, fmt.Errorf("resolve client: %w", err)
	}

	creds, err := readClientCredentials(client)
	if err != nil {
		return nil, fmt.Errorf("read credentials: %w", err)
	}

	var requiredScopes []string

	if scopes, err := googleauth.Scopes(service); err != nil {
		return nil, fmt.Errorf("resolve scopes: %w", err)
	} else {
		requiredScopes = scopes
	}

	return tokenSourceForAccountScopes(ctx, string(service), email, client, creds.ClientID, creds.ClientSecret, requiredScopes)
}

func tokenSourceForAccountScopes(ctx context.Context, serviceLabel string, email string, client string, clientID string, clientSecret string, requiredScopes []string) (oauth2.TokenSource, error) {
	var store secrets.Store

	if s, err := openSecretsStore(); err != nil {
		return nil, fmt.Errorf("open secrets store: %w", err)
	} else {
		store = s
	}

	var tok secrets.Token

	if t, err := store.GetToken(client, email); err != nil {
		if errors.Is(err, keyring.ErrKeyNotFound) {
			return nil, &AuthRequiredError{Service: serviceLabel, Email: email, Client: client, Cause: err}
		}

		return nil, fmt.Errorf("get token for %s: %w", email, err)
	} else {
		tok = t
	}

	cfg := oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     google.Endpoint,
		Scopes:       requiredScopes,
	}

	// Ensure refresh-token exchanges don't hang forever.
	ctx = context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Timeout: tokenExchangeTimeout})

	baseSource := cfg.TokenSource(ctx, &oauth2.Token{RefreshToken: tok.RefreshToken})

	return newPersistingTokenSource(baseSource, store, client, email, tok), nil
}

func optionsForAccount(ctx context.Context, service googleauth.Service, email string) ([]option.ClientOption, error) {
	scopes, err := googleauth.Scopes(service)
	if err != nil {
		return nil, fmt.Errorf("resolve scopes: %w", err)
	}

	return optionsForAccountScopes(ctx, string(service), email, scopes)
}

func optionsForAccountScopes(ctx context.Context, serviceLabel string, email string, scopes []string) ([]option.ClientOption, error) {
	slog.Debug("creating client options with custom scopes", "serviceLabel", serviceLabel, "email", email)

	if composioProxyEnabled() {
		c, err := newComposioProxyHTTPClient(ctx, serviceLabel, email)
		if err != nil {
			return nil, fmt.Errorf("composio proxy client: %w", err)
		}

		slog.Debug("using Composio proxy execute for Google API client", "serviceLabel", serviceLabel, "email", email)

		return []option.ClientOption{option.WithHTTPClient(c)}, nil
	}

	// If GOG_ACCESS_TOKEN is set, build client options with static token source.
	if accessToken := os.Getenv("GOG_ACCESS_TOKEN"); accessToken != "" {
		slog.Debug("using static access token from GOG_ACCESS_TOKEN env var", "serviceLabel", serviceLabel, "email", email)
		ts := oauth2.StaticTokenSource(&oauth2.Token{
			AccessToken: accessToken,
			TokenType:   "Bearer",
		})
		baseTransport := newBaseTransport()
		retryTransport := NewRetryTransport(&oauth2.Transport{
			Source: ts,
			Base:   baseTransport,
		})
		c := &http.Client{Transport: retryTransport}

		return []option.ClientOption{option.WithHTTPClient(c)}, nil
	}

	var creds config.ClientCredentials

	var ts oauth2.TokenSource

	if serviceAccountTS, saPath, ok, err := tokenSourceForServiceAccountScopes(ctx, serviceLabel, email, scopes); err != nil {
		return nil, fmt.Errorf("service account token source: %w", err)
	} else if ok {
		slog.Debug("using service account credentials", "email", email, "path", saPath)
		ts = serviceAccountTS
	} else {
		client, err := authclient.ResolveClient(ctx, email)
		if err != nil {
			return nil, fmt.Errorf("resolve client: %w", err)
		}

		if c, err := readClientCredentials(client); err != nil {
			return nil, fmt.Errorf("read credentials: %w", err)
		} else {
			creds = c
		}

		if tokenSource, err := tokenSourceForAccountScopes(ctx, serviceLabel, email, client, creds.ClientID, creds.ClientSecret, scopes); err != nil {
			return nil, fmt.Errorf("token source: %w", err)
		} else {
			ts = tokenSource
		}
	}
	baseTransport := newBaseTransport()
	// Wrap with retry logic for 429 and 5xx errors
	retryTransport := NewRetryTransport(&oauth2.Transport{
		Source: ts,
		Base:   baseTransport,
	})
	c := &http.Client{
		Transport: retryTransport,
		// No Timeout set: large file downloads (Drive videos, etc.) must not
		// be cut short. Server responsiveness is guarded by the transport's
		// ResponseHeaderTimeout instead.
	}

	slog.Debug("client options with custom scopes created successfully", "serviceLabel", serviceLabel, "email", email)

	return []option.ClientOption{option.WithHTTPClient(c)}, nil
}

func newBaseTransport() *http.Transport {
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok || defaultTransport == nil {
		return &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
			},
			ResponseHeaderTimeout: responseHeaderTimeout,
		}
	}

	// Clone() deep-copies TLSClientConfig, so no additional clone needed.
	transport := defaultTransport.Clone()
	transport.ResponseHeaderTimeout = responseHeaderTimeout

	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		return transport
	}

	if transport.TLSClientConfig.MinVersion < tls.VersionTLS12 {
		transport.TLSClientConfig.MinVersion = tls.VersionTLS12
	}

	return transport
}
