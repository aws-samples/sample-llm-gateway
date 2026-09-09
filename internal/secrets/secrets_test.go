package secrets

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/aws-samples/sample-llm-gateway/internal/config"
)

type fakeAPI struct {
	secrets map[string]*string // nil pointer = binary-only secret
	calls   int
}

func (f *fakeAPI) GetSecretValue(_ context.Context, in *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	f.calls++
	s, ok := f.secrets[aws.ToString(in.SecretId)]
	if !ok {
		return nil, errors.New("ResourceNotFoundException")
	}
	return &secretsmanager.GetSecretValueOutput{SecretString: s, VersionId: aws.String("v1")}, nil
}

func newCfg() *config.Config {
	return &config.Config{
		ControlPlane: config.ControlPlane{Token: "secretsmanager://llm-gateway/poc#CP_GATEWAY_TOKEN"},
		Providers: map[string]config.ProviderConfig{
			"openai":  {Auth: config.AuthBearer, APIKey: "secretsmanager://llm-gateway/poc#OPENAI_API_KEY"},
			"moon":    {Auth: config.AuthBearer, APIKey: "secretsmanager://arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:moonshot-AbCdEf"},
			"bedrock": {Auth: config.AuthAWSIAM, Region: "ap-northeast-1", ExternalID: "plain-external-id"},
		},
	}
}

func TestParseRef(t *testing.T) {
	cases := []struct {
		in      string
		id, key string
		wantErr bool
	}{
		{"secretsmanager://llm-gateway/poc", "llm-gateway/poc", "", false},
		{"secretsmanager://llm-gateway/poc#token", "llm-gateway/poc", "token", false},
		{"secretsmanager://arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:x-AbCdEf#k", "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:x-AbCdEf", "k", false},
		{"secretsmanager://", "", "", true},
		{"secretsmanager://name#", "", "", true},
		{"plain-value", "", "", true},
	}
	for _, c := range cases {
		got, err := ParseRef(c.in)
		if (err != nil) != c.wantErr {
			t.Fatalf("%q: err=%v wantErr=%v", c.in, err, c.wantErr)
		}
		if !c.wantErr && (got.SecretID != c.id || got.JSONKey != c.key) {
			t.Fatalf("%q: got %+v", c.in, got)
		}
	}
}

func TestResolve(t *testing.T) {
	api := &fakeAPI{secrets: map[string]*string{
		"llm-gateway/poc": aws.String(`{"CP_GATEWAY_TOKEN":"tok-123","OPENAI_API_KEY":"sk-abc","N":1}`),
		"arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:moonshot-AbCdEf": aws.String("sk-moon\n"),
	}}
	cfg := newCfg()
	if !HasRefs(cfg) {
		t.Fatal("HasRefs should be true")
	}
	r := NewWithAPI(api, 0, slog.Default())
	if err := r.Resolve(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.ControlPlane.Token != "tok-123" {
		t.Fatalf("token=%q", cfg.ControlPlane.Token)
	}
	if cfg.Providers["openai"].APIKey != "sk-abc" {
		t.Fatalf("openai=%q", cfg.Providers["openai"].APIKey)
	}
	if cfg.Providers["moon"].APIKey != "sk-moon" {
		t.Fatalf("moon=%q (whole-string secret should be trimmed)", cfg.Providers["moon"].APIKey)
	}
	if cfg.Providers["bedrock"].ExternalID != "plain-external-id" {
		t.Fatalf("plaintext field must be untouched, got %q", cfg.Providers["bedrock"].ExternalID)
	}
	if api.calls != 2 {
		t.Fatalf("expected 2 GetSecretValue calls (one per secret id), got %d", api.calls)
	}
	if HasRefs(cfg) {
		t.Fatal("HasRefs should be false after Resolve")
	}
}

func TestResolveErrors(t *testing.T) {
	api := &fakeAPI{secrets: map[string]*string{
		"json":   aws.String(`{"a":"x","n":1,"empty":" "}`),
		"plain":  aws.String("not-json"),
		"binary": nil,
	}}
	cases := map[string]string{
		"secretsmanager://missing":    "get secret",
		"secretsmanager://json#nokey": "not found",
		"secretsmanager://json#n":     "not a string",
		"secretsmanager://json#empty": "empty value",
		"secretsmanager://plain#k":    "not a JSON object",
		"secretsmanager://binary":     "no SecretString",
		"secretsmanager://":           "empty secret id",
	}
	for ref, want := range cases {
		cfg := &config.Config{ControlPlane: config.ControlPlane{Token: ref}}
		err := NewWithAPI(api, 0, nil).Resolve(context.Background(), cfg)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%q: err=%v, want containing %q", ref, err, want)
		}
		if !strings.HasPrefix(err.Error(), "control_plane.token") {
			t.Fatalf("%q: error should name the field, got %v", ref, err)
		}
	}
}

func TestNoRefsIsNoop(t *testing.T) {
	cfg := &config.Config{ControlPlane: config.ControlPlane{Token: "plain"},
		Providers: map[string]config.ProviderConfig{"p": {APIKey: "sk-plain"}}}
	if HasRefs(cfg) {
		t.Fatal("HasRefs should be false")
	}
	api := &fakeAPI{}
	if err := NewWithAPI(api, 0, nil).Resolve(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if api.calls != 0 || cfg.ControlPlane.Token != "plain" || cfg.Providers["p"].APIKey != "sk-plain" {
		t.Fatalf("plaintext config must not touch the API or change values: calls=%d", api.calls)
	}
}
