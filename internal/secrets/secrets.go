// Package secrets resolves secret-valued configuration fields from AWS Secrets Manager.
//
// Two storage modes are supported for every secret field (control_plane.token,
// providers.*.api_key, providers.*.external_id):
//
//   - plaintext: the YAML value (after ${ENV} expansion) is used as-is;
//   - Secrets Manager: the YAML value is a reference of the form
//     "secretsmanager://<secret-id>[#<json-key>]" where <secret-id> is a secret name or
//     ARN and the optional <json-key> selects one key of a JSON-object secret. Without a
//     key the whole SecretString is the value.
//
// Resolution happens once at startup with the default AWS credential chain (IRSA on
// EKS). Any failure aborts startup; a rotated secret takes effect on the next restart.
package secrets

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/aws-samples/sample-llm-gateway/internal/config"
)

// Scheme prefixes a Secrets Manager reference.
const Scheme = "secretsmanager://"

// IsRef reports whether v is a Secrets Manager reference rather than a plaintext value.
func IsRef(v string) bool { return strings.HasPrefix(v, Scheme) }

// Ref is a parsed "secretsmanager://<secret-id>[#<json-key>]" reference.
type Ref struct {
	SecretID string // name or ARN
	JSONKey  string // empty: whole SecretString
}

// ParseRef parses a reference. Secret names never contain '#', and ARNs do not either, so
// the first '#' separates the secret id from the JSON key.
func ParseRef(v string) (Ref, error) {
	if !IsRef(v) {
		return Ref{}, fmt.Errorf("not a %s reference", Scheme)
	}
	rest := strings.TrimPrefix(v, Scheme)
	id, key, _ := strings.Cut(rest, "#")
	if id == "" {
		return Ref{}, fmt.Errorf("%q: empty secret id", v)
	}
	if strings.Contains(rest, "#") && key == "" {
		return Ref{}, fmt.Errorf("%q: empty json key after '#'", v)
	}
	return Ref{SecretID: id, JSONKey: key}, nil
}

// API is the subset of the Secrets Manager client used here (swap in a fake for tests).
type API interface {
	GetSecretValue(ctx context.Context, in *secretsmanager.GetSecretValueInput, opts ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
}

// Resolver fetches secrets and caches each secret id for the lifetime of one Resolve call,
// so several fields pointing at different keys of one JSON secret cost one API call.
type Resolver struct {
	api     API
	timeout time.Duration
	log     *slog.Logger
	cache   map[string]fetched
}

type fetched struct {
	value   string
	version string
}

// New builds a Resolver over a real Secrets Manager client. The region comes from
// cfg.Secrets.Region, then AWS_REGION, then AWS_DEFAULT_REGION.
func New(ctx context.Context, sc config.Secrets, log *slog.Logger) (*Resolver, error) {
	region := sc.Region
	if region == "" {
		region = os.Getenv("AWS_REGION")
	}
	if region == "" {
		region = os.Getenv("AWS_DEFAULT_REGION")
	}
	if region == "" {
		return nil, fmt.Errorf("secrets.region is required (AWS_REGION / AWS_DEFAULT_REGION not set)")
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	client := secretsmanager.NewFromConfig(awsCfg, func(o *secretsmanager.Options) {
		if sc.Endpoint != "" {
			o.BaseEndpoint = aws.String(sc.Endpoint)
		}
	})
	log.Info("secrets manager resolver ready", "region", region, "endpoint", sc.Endpoint)
	return NewWithAPI(client, sc.Timeout, log), nil
}

// NewWithAPI builds a Resolver over any API implementation.
func NewWithAPI(api API, timeout time.Duration, log *slog.Logger) *Resolver {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if log == nil {
		log = slog.Default()
	}
	return &Resolver{api: api, timeout: timeout, log: log, cache: map[string]fetched{}}
}

// Resolve replaces every Secrets Manager reference in cfg with the fetched value. Plaintext
// fields are left untouched. It is a no-op (and never touches AWS) when cfg has no references.
// Callers should construct the Resolver only when HasRefs(cfg) is true.
func (r *Resolver) Resolve(ctx context.Context, cfg *config.Config) error {
	if err := r.resolveField(ctx, "control_plane.token", &cfg.ControlPlane.Token); err != nil {
		return err
	}
	for code, p := range cfg.Providers {
		if err := r.resolveField(ctx, "providers."+code+".api_key", &p.APIKey); err != nil {
			return err
		}
		if err := r.resolveField(ctx, "providers."+code+".external_id", &p.ExternalID); err != nil {
			return err
		}
		cfg.Providers[code] = p
	}
	return nil
}

// HasRefs reports whether any secret field in cfg is a Secrets Manager reference.
func HasRefs(cfg *config.Config) bool {
	if IsRef(cfg.ControlPlane.Token) {
		return true
	}
	for _, p := range cfg.Providers {
		if IsRef(p.APIKey) || IsRef(p.ExternalID) {
			return true
		}
	}
	return false
}

func (r *Resolver) resolveField(ctx context.Context, field string, v *string) error {
	if !IsRef(*v) {
		return nil
	}
	ref, err := ParseRef(*v)
	if err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	f, err := r.fetch(ctx, ref.SecretID)
	if err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	val := f.value
	if ref.JSONKey != "" {
		val, err = jsonKey(f.value, ref.JSONKey)
		if err != nil {
			return fmt.Errorf("%s: secret %q: %w", field, ref.SecretID, err)
		}
	}
	val = strings.TrimSpace(val)
	if val == "" {
		return fmt.Errorf("%s: secret %q resolved to an empty value", field, ref.SecretID)
	}
	*v = val
	r.log.Info("secret resolved", "field", field, "secret_id", ref.SecretID, "json_key", ref.JSONKey, "version_id", f.version)
	return nil
}

func (r *Resolver) fetch(ctx context.Context, id string) (fetched, error) {
	if f, ok := r.cache[id]; ok {
		return f, nil
	}
	cctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	out, err := r.api.GetSecretValue(cctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(id)})
	if err != nil {
		return fetched{}, fmt.Errorf("get secret %q: %w", id, err)
	}
	if out.SecretString == nil {
		return fetched{}, fmt.Errorf("secret %q has no SecretString (binary secrets are not supported)", id)
	}
	f := fetched{value: *out.SecretString, version: aws.ToString(out.VersionId)}
	r.cache[id] = f
	return f, nil
}

// jsonKey extracts a string-valued key from a JSON-object secret.
func jsonKey(secret, key string) (string, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(secret), &m); err != nil {
		return "", fmt.Errorf("SecretString is not a JSON object, cannot select key %q", key)
	}
	raw, ok := m[key]
	if !ok {
		return "", fmt.Errorf("json key %q not found", key)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("json key %q is not a string", key)
	}
	return s, nil
}
