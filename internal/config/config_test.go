package config

import "testing"

// minimalValid returns a config that passes validate() except for control_plane.base_url,
// which each test sets explicitly.
func minimalValid() *Config {
	return &Config{
		ControlPlane: ControlPlane{Token: "t"},
		Providers: map[string]ProviderConfig{
			"p": {Auth: AuthNone, Endpoints: map[string]string{EndpointAnthropic: "https://api.example.com/anthropic/v1"}},
		},
	}
}

func TestValidateSecretsEndpoint(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		wantErr  bool
	}{
		{"empty ok (default)", "", false},
		{"https ok", "https://vpce-abc.secretsmanager.ap-northeast-1.vpce.amazonaws.com", false},
		{"http rejected", "http://vpce-abc.secretsmanager.ap-northeast-1.vpce.amazonaws.com", true},
		{"no scheme rejected", "vpce-abc.secretsmanager.ap-northeast-1.vpce.amazonaws.com", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := minimalValid()
			c.ControlPlane.BaseURL = "https://cp.example.com"
			c.Secrets.Endpoint = tc.endpoint
			err := c.validate()
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for secrets.endpoint=%q, got nil", tc.endpoint)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for secrets.endpoint=%q: %v", tc.endpoint, err)
			}
		})
	}
}

func TestValidateBaseURLScheme(t *testing.T) {
	cases := []struct {
		name          string
		baseURL       string
		allowInsecure bool
		wantErr       bool
	}{
		{"https ok", "https://cp.example.com", false, false},
		{"http without allow_insecure rejected", "http://cp.example.com", false, true},
		{"http with allow_insecure ok", "http://cp.example.com", true, false},
		{"no scheme rejected", "cp.example.com:8080", false, true},
		{"ftp scheme rejected", "ftp://cp.example.com", false, true},
		{"empty rejected", "", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := minimalValid()
			c.ControlPlane.BaseURL = tc.baseURL
			c.ControlPlane.AllowInsecure = tc.allowInsecure
			err := c.validate()
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for base_url=%q allow_insecure=%v, got nil", tc.baseURL, tc.allowInsecure)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for base_url=%q allow_insecure=%v: %v", tc.baseURL, tc.allowInsecure, err)
			}
		})
	}
}
