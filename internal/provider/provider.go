// Package provider models an upstream LLM endpoint: per-protocol base URLs plus
// an authenticator that injects credentials into the outbound request.
package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/aws-samples/sample-llm-gateway/internal/config"
)

// Authenticator injects upstream credentials into req. body is the exact payload
// that will be sent (needed for SigV4 payload hashing).
type Authenticator interface {
	Authenticate(ctx context.Context, req *http.Request, body []byte) error
}

// Provider is a configured upstream.
type Provider struct {
	Code      string
	endpoints map[string]*url.URL // keyed by config.Endpoint* protocol names
	auth      Authenticator
}

// Registry maps providerCode to Provider.
type Registry map[string]*Provider

// Build constructs providers from config. aws_iam providers resolve credentials via
// the default AWS chain (env, shared config, IRSA web identity, IMDS...).
func Build(ctx context.Context, cfgs map[string]config.ProviderConfig) (Registry, error) {
	reg := Registry{}
	for code, pc := range cfgs {
		p := &Provider{Code: code, endpoints: map[string]*url.URL{}}
		for name, raw := range pc.Endpoints {
			u, err := url.Parse(strings.TrimRight(raw, "/"))
			if err != nil || u.Scheme == "" || u.Host == "" {
				return nil, fmt.Errorf("provider %q endpoint %q: invalid url %q", code, name, raw)
			}
			p.endpoints[name] = u
		}
		switch pc.Auth {
		case config.AuthBearer:
			p.auth = headerAuth{name: "Authorization", value: "Bearer " + pc.APIKey}
		case config.AuthXAPIKey:
			p.auth = headerAuth{name: "x-api-key", value: pc.APIKey}
		case config.AuthNone:
			p.auth = noAuth{}
		case config.AuthAWSIAM:
			a, err := newSigV4(ctx, code, pc)
			if err != nil {
				return nil, fmt.Errorf("provider %q: %w", code, err)
			}
			p.auth = a
		default:
			return nil, fmt.Errorf("provider %q: unsupported auth %q", code, pc.Auth)
		}
		reg[code] = p
	}
	return reg, nil
}

// Endpoint returns the base URL for a protocol, or false if this provider lacks it.
func (p *Provider) Endpoint(protocol string) (*url.URL, bool) {
	u, ok := p.endpoints[protocol]
	return u, ok
}

// Authenticate delegates to the configured authenticator.
func (p *Provider) Authenticate(ctx context.Context, req *http.Request, body []byte) error {
	return p.auth.Authenticate(ctx, req, body)
}

type headerAuth struct{ name, value string }

func (h headerAuth) Authenticate(_ context.Context, req *http.Request, _ []byte) error {
	req.Header.Set(h.name, h.value)
	return nil
}

type noAuth struct{}

func (noAuth) Authenticate(context.Context, *http.Request, []byte) error { return nil }
