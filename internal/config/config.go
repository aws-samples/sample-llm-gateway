// Package config loads and validates the gateway configuration.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Protocol names used as keys under providers.<code>.endpoints.
const (
	EndpointOpenAIChat      = "openai_chat"
	EndpointOpenAIResponses = "openai_responses"
	EndpointAnthropic       = "anthropic"
)

// Auth modes for providers.
const (
	AuthBearer  = "bearer"
	AuthXAPIKey = "x-api-key"
	AuthAWSIAM  = "aws_iam"
	AuthNone    = "none"
)

type Config struct {
	Server       Server                    `yaml:"server"`
	ControlPlane ControlPlane              `yaml:"control_plane"`
	Metering     Metering                  `yaml:"metering"`
	Secrets      Secrets                   `yaml:"secrets"`
	Providers    map[string]ProviderConfig `yaml:"providers"`
}

type Server struct {
	Listen                        string        `yaml:"listen"`
	ReadTimeout                   time.Duration `yaml:"read_timeout"`     // headers + body of one inbound request
	ShutdownTimeout               time.Duration `yaml:"shutdown_timeout"` // wait for in-flight requests after SIGTERM
	RequestTimeout                time.Duration `yaml:"request_timeout"`
	UpstreamConnectTimeout        time.Duration `yaml:"upstream_connect_timeout"`
	UpstreamResponseHeaderTimeout time.Duration `yaml:"upstream_response_header_timeout"`
	MaxRequestBodyBytes           int64         `yaml:"max_request_body_bytes"`
	MaxFailoverAttempts           int           `yaml:"max_failover_attempts"`
	LogLevel                      string        `yaml:"log_level"`
}

type ControlPlane struct {
	BaseURL            string        `yaml:"base_url"`
	Token              string        `yaml:"token"`
	TokenHeader        string        `yaml:"token_header"`
	AllowInsecure      bool          `yaml:"allow_insecure"` // permit a plaintext http:// base_url (e.g. in-cluster mock); off by default
	KeyAuthTimeout     time.Duration `yaml:"key_auth_timeout"`
	ReportTimeout      time.Duration `yaml:"report_timeout"`
	RoutesPollInterval time.Duration `yaml:"routes_poll_interval"`
}

type Metering struct {
	QueueSize  int `yaml:"queue_size"`
	Workers    int `yaml:"workers"`
	MaxRetries int `yaml:"max_retries"`
}

// Secrets configures how secret-valued fields (control_plane.token, providers.*.api_key,
// providers.*.external_id) are resolved. A field written as a plain value (or ${ENV}) is
// used as-is; a field written as "secretsmanager://<secret-id>[#<json-key>]" is fetched
// from AWS Secrets Manager at startup with the default AWS credential chain (IRSA on EKS).
type Secrets struct {
	Region   string        `yaml:"region"`   // Secrets Manager region; defaults to AWS_REGION, then AWS_DEFAULT_REGION
	Endpoint string        `yaml:"endpoint"` // optional custom endpoint URL (VPC endpoint DNS name when private DNS is off)
	Timeout  time.Duration `yaml:"timeout"`  // per GetSecretValue call; default 10s
}

type ProviderConfig struct {
	Auth       string            `yaml:"auth"`
	APIKey     string            `yaml:"api_key"`
	Region     string            `yaml:"region"`      // aws_iam: SigV4 signing region (where the Bedrock endpoint lives)
	RoleARN    string            `yaml:"role_arn"`    // aws_iam, optional: assume this role (e.g. cross-account) before signing
	ExternalID string            `yaml:"external_id"` // aws_iam, optional: ExternalId for role_arn
	STSRegion  string            `yaml:"sts_region"`  // aws_iam, optional: region for STS calls (web identity + assume-role); defaults to AWS_REGION, then region
	Endpoints  map[string]string `yaml:"endpoints"`
}

// Load reads a YAML file, expands ${ENV} references, applies defaults and validates.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	expanded := os.Expand(string(raw), func(k string) string { return os.Getenv(k) })
	var c Config
	if err := yaml.Unmarshal([]byte(expanded), &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Server.Listen == "" {
		c.Server.Listen = ":8080"
	}
	if c.Server.ReadTimeout == 0 {
		c.Server.ReadTimeout = 60 * time.Second
	}
	if c.Server.ShutdownTimeout == 0 {
		c.Server.ShutdownTimeout = 280 * time.Second
	}
	if c.Server.RequestTimeout == 0 {
		c.Server.RequestTimeout = 10 * time.Minute
	}
	if c.Server.UpstreamConnectTimeout == 0 {
		c.Server.UpstreamConnectTimeout = 5 * time.Second
	}
	if c.Server.UpstreamResponseHeaderTimeout == 0 {
		c.Server.UpstreamResponseHeaderTimeout = 5 * time.Minute
	}
	if c.Server.MaxRequestBodyBytes == 0 {
		c.Server.MaxRequestBodyBytes = 32 << 20
	}
	if c.Server.MaxFailoverAttempts == 0 {
		c.Server.MaxFailoverAttempts = 3
	}
	if c.Server.LogLevel == "" {
		c.Server.LogLevel = "info"
	}
	if c.ControlPlane.TokenHeader == "" {
		c.ControlPlane.TokenHeader = "X-HIGRESS-Token"
	}
	if c.ControlPlane.KeyAuthTimeout == 0 {
		c.ControlPlane.KeyAuthTimeout = 2 * time.Second
	}
	if c.ControlPlane.ReportTimeout == 0 {
		c.ControlPlane.ReportTimeout = 5 * time.Second
	}
	if c.ControlPlane.RoutesPollInterval == 0 {
		c.ControlPlane.RoutesPollInterval = 30 * time.Second
	}
	if c.Metering.QueueSize == 0 {
		c.Metering.QueueSize = 10000
	}
	if c.Metering.Workers == 0 {
		c.Metering.Workers = 4
	}
	if c.Metering.MaxRetries == 0 {
		c.Metering.MaxRetries = 5
	}
	if c.Secrets.Timeout == 0 {
		c.Secrets.Timeout = 10 * time.Second
	}
	for code, p := range c.Providers {
		if p.Auth == "" {
			p.Auth = AuthBearer
		}
		c.Providers[code] = p
	}
}

func (c *Config) validate() error {
	if c.ControlPlane.BaseURL == "" {
		return fmt.Errorf("control_plane.base_url is required")
	}
	if c.ControlPlane.Token == "" {
		return fmt.Errorf("control_plane.token is required")
	}
	// The control-plane token and customer api keys travel on every control-plane call, so
	// a plaintext base_url would leak them. Require https unless allow_insecure is set.
	if u, err := url.Parse(c.ControlPlane.BaseURL); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("control_plane.base_url must be an absolute http(s) URL, got %q", c.ControlPlane.BaseURL)
	} else if u.Scheme == "http" && !c.ControlPlane.AllowInsecure {
		return fmt.Errorf("control_plane.base_url uses plaintext http (%s): the control-plane token and customer api keys would be sent in cleartext; use https, or set control_plane.allow_insecure: true to override (e.g. an in-cluster mock)", c.ControlPlane.BaseURL)
	}
	// secrets.endpoint (when set) fetches the token and provider keys at startup; it is an
	// AWS Secrets Manager (VPC) endpoint and is always https. Reject a plaintext value so a
	// typo cannot pull secrets over the wire in the clear. No allow_insecure escape hatch:
	// unlike the control plane there is no legitimate plaintext Secrets Manager endpoint.
	if e := c.Secrets.Endpoint; e != "" {
		if u, err := url.Parse(e); err != nil || u.Host == "" || u.Scheme != "https" {
			return fmt.Errorf("secrets.endpoint must be an https URL, got %q", e)
		}
	}
	if len(c.Providers) == 0 {
		return fmt.Errorf("at least one provider must be configured")
	}
	for code, p := range c.Providers {
		switch p.Auth {
		case AuthBearer, AuthXAPIKey:
			if p.APIKey == "" {
				return fmt.Errorf("provider %q: api_key is required for auth=%s", code, p.Auth)
			}
		case AuthAWSIAM:
			if p.Region == "" {
				return fmt.Errorf("provider %q: region is required for auth=aws_iam", code)
			}
			if p.RoleARN != "" && !strings.HasPrefix(p.RoleARN, "arn:aws") {
				return fmt.Errorf("provider %q: role_arn %q is not an IAM role ARN", code, p.RoleARN)
			}
		case AuthNone:
		default:
			return fmt.Errorf("provider %q: unknown auth %q", code, p.Auth)
		}
		if len(p.Endpoints) == 0 {
			return fmt.Errorf("provider %q: at least one endpoint is required", code)
		}
		for name := range p.Endpoints {
			switch name {
			case EndpointOpenAIChat, EndpointOpenAIResponses, EndpointAnthropic:
			default:
				return fmt.Errorf("provider %q: unknown endpoint %q", code, name)
			}
		}
	}
	return nil
}
