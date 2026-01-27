package deploymentrecord

import (
	"strings"
	"testing"
	"time"
)

func TestNewClient(t *testing.T) {
	tests := []struct {
		name        string
		baseURL     string
		org         string
		wantErr     bool
		errContains string
		wantBaseURL string
	}{
		{
			name:        "valid HTTPS URL",
			baseURL:     "https://api.github.com",
			org:         "my-org",
			wantErr:     false,
			wantBaseURL: "https://api.github.com",
		},
		{
			name:        "URL without scheme gets HTTPS prefix",
			baseURL:     "api.github.com",
			org:         "my-org",
			wantErr:     false,
			wantBaseURL: "https://api.github.com",
		},
		{
			name:        "HTTP URL rejected for non-local host",
			baseURL:     "http://api.github.com",
			org:         "my-org",
			wantErr:     true,
			errContains: "insecure URL not allowed",
		},
		{
			name:        "HTTP localhost allowed",
			baseURL:     "http://localhost:8080",
			org:         "my-org",
			wantErr:     false,
			wantBaseURL: "http://localhost:8080",
		},
		{
			name:        "HTTP localhost without port allowed",
			baseURL:     "http://localhost",
			org:         "my-org",
			wantErr:     false,
			wantBaseURL: "http://localhost",
		},
		{
			name:        "HTTP 127.0.0.1 allowed",
			baseURL:     "http://127.0.0.1:9090",
			org:         "my-org",
			wantErr:     false,
			wantBaseURL: "http://127.0.0.1:9090",
		},
		{
			name:        "HTTP Kubernetes service allowed",
			baseURL:     "http://my-service.my-namespace.svc.cluster.local:8080",
			org:         "my-org",
			wantErr:     false,
			wantBaseURL: "http://my-service.my-namespace.svc.cluster.local:8080",
		},
		{
			name:        "HTTPS Kubernetes service allowed",
			baseURL:     "https://my-service.my-namespace.svc.cluster.local",
			org:         "my-org",
			wantErr:     false,
			wantBaseURL: "https://my-service.my-namespace.svc.cluster.local",
		},
		{
			name:        "valid org with hyphens",
			baseURL:     "https://api.github.com",
			org:         "my-org-name",
			wantErr:     false,
			wantBaseURL: "https://api.github.com",
		},
		{
			name:        "valid org with underscores",
			baseURL:     "https://api.github.com",
			org:         "my_org_name",
			wantErr:     false,
			wantBaseURL: "https://api.github.com",
		},
		{
			name:        "valid org alphanumeric",
			baseURL:     "https://api.github.com",
			org:         "MyOrg123",
			wantErr:     false,
			wantBaseURL: "https://api.github.com",
		},
		{
			name:        "invalid org with spaces",
			baseURL:     "https://api.github.com",
			org:         "my org",
			wantErr:     true,
			errContains: "invalid organization name",
		},
		{
			name:        "invalid org with slash",
			baseURL:     "https://api.github.com",
			org:         "my-org/../other",
			wantErr:     true,
			errContains: "invalid organization name",
		},
		{
			name:        "invalid org with special characters",
			baseURL:     "https://api.github.com",
			org:         "my@org!",
			wantErr:     true,
			errContains: "invalid organization name",
		},
		{
			name:        "empty org",
			baseURL:     "https://api.github.com",
			org:         "",
			wantErr:     true,
			errContains: "invalid organization name",
		},
		{
			name:        "HTTP with external IP rejected",
			baseURL:     "http://192.168.1.1:8080",
			org:         "my-org",
			wantErr:     true,
			errContains: "insecure URL not allowed",
		},
		{
			name:        "HTTP with domain rejected",
			baseURL:     "http://example.com",
			org:         "my-org",
			wantErr:     true,
			errContains: "insecure URL not allowed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := NewClient(tt.baseURL, tt.org)

			if tt.wantErr {
				if err == nil {
					t.Errorf("NewClient(%q, %q) expected error containing %q, got nil",
						tt.baseURL, tt.org, tt.errContains)
					return
				}
				if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("NewClient(%q, %q) error = %q, want error containing %q",
						tt.baseURL, tt.org, err.Error(), tt.errContains)
				}
				return
			}

			if err != nil {
				t.Errorf("NewClient(%q, %q) unexpected error: %v",
					tt.baseURL, tt.org, err)
				return
			}

			if client.baseURL != tt.wantBaseURL {
				t.Errorf("NewClient(%q, %q) baseURL = %q, want %q",
					tt.baseURL, tt.org, client.baseURL, tt.wantBaseURL)
			}

			if client.org != tt.org {
				t.Errorf("NewClient(%q, %q) org = %q, want %q",
					tt.baseURL, tt.org, client.org, tt.org)
			}
		})
	}
}

func TestNewClientWithOptions(t *testing.T) {
	t.Run("WithTimeout option", func(t *testing.T) {
		client, err := NewClient("https://api.github.com", "my-org",
			WithTimeout(30))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if client.httpClient.Timeout != 30*time.Second {
			t.Errorf("timeout = %v, want %v", client.httpClient.Timeout, 30*time.Second)
		}
	})

	t.Run("WithRetries option", func(t *testing.T) {
		client, err := NewClient("https://api.github.com", "my-org",
			WithRetries(5))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if client.retries != 5 {
			t.Errorf("retries = %d, want %d", client.retries, 5)
		}
	})

	t.Run("WithAPIToken option", func(t *testing.T) {
		client, err := NewClient("https://api.github.com", "my-org",
			WithAPIToken("test-token"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if client.apiToken != "test-token" {
			t.Errorf("apiToken = %q, want %q", client.apiToken, "test-token")
		}
	})

	t.Run("multiple options", func(t *testing.T) {
		client, err := NewClient("https://api.github.com", "my-org",
			WithTimeout(60),
			WithRetries(10),
			WithAPIToken("multi-token"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if client.httpClient.Timeout != 60*time.Second {
			t.Errorf("timeout = %v, want %v", client.httpClient.Timeout, 60*time.Second)
		}
		if client.retries != 10 {
			t.Errorf("retries = %d, want %d", client.retries, 10)
		}
		if client.apiToken != "multi-token" {
			t.Errorf("apiToken = %q, want %q", client.apiToken, "multi-token")
		}
	})
}

func TestValidOrgPattern(t *testing.T) {
	validOrgs := []string{
		"github",
		"my-org",
		"my_org",
		"MyOrg123",
		"org-with-many-hyphens",
		"org_with_many_underscores",
		"MixedCase-and_underscores-123",
		"a",
		"A",
		"1",
	}

	for _, org := range validOrgs {
		if !validOrgPattern.MatchString(org) {
			t.Errorf("validOrgPattern should match %q", org)
		}
	}

	invalidOrgs := []string{
		"",
		"has space",
		"has/slash",
		"has\\backslash",
		"has@symbol",
		"has!exclaim",
		"has.dot",
		"../traversal",
		"org/../../../etc/passwd",
	}

	for _, org := range invalidOrgs {
		if validOrgPattern.MatchString(org) {
			t.Errorf("validOrgPattern should not match %q", org)
		}
	}
}
