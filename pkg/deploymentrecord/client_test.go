package deploymentrecord

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/github/deployment-tracker/pkg/dtmetrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
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

// testRecord returns a minimal valid DeploymentRecord for testing.
func testRecord() *DeploymentRecord {
	return NewDeploymentRecord(
		"ghcr.io/my-org/my-image",
		"sha256:abc123",
		"v1.0.0",
		"production",
		"us-east-1",
		"cluster-1",
		StatusDeployed,
		"my-deployment",
		nil,
		nil,
	)
}

// allCounters returns all PostDeploymentRecord counters for snapshotting.
func allCounters() []prometheus.Counter {
	return []prometheus.Counter{
		dtmetrics.PostDeploymentRecordOk,
		dtmetrics.PostDeploymentRecordNoAttestation,
		dtmetrics.PostDeploymentRecordRateLimited,
		dtmetrics.PostDeploymentRecordSoftFail,
		dtmetrics.PostDeploymentRecordHardFail,
		dtmetrics.PostDeploymentRecordClientError,
	}
}

func TestPostOne(t *testing.T) {
	tests := []struct {
		name              string
		record            *DeploymentRecord
		retries           int
		handler           http.HandlerFunc
		wantErr           bool
		errType           any // expected error type for errors.As
		errContain        string
		wantOk            float64
		wantNoAttestation float64
		wantRateLimited   float64
		wantSoftFail      float64
		wantHardFail      float64
		wantClientError   float64
	}{
		{
			name:   "success on 200",
			record: testRecord(),
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			},
			wantOk: 1,
		},
		{
			name:   "success on 201",
			record: testRecord(),
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
			},
			wantOk: 1,
		},
		{
			name:    "nil record returns error",
			record:  nil,
			wantErr: true,
			handler: func(_ http.ResponseWriter, _ *http.Request) {
				t.Fatal("server should not be called with nil record")
			},
			errContain: "record cannot be nil",
		},
		{
			name:   "404 with no artifacts found returns NoArtifactError",
			record: testRecord(),
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"no artifacts found"}`))
			},
			wantErr:           true,
			errType:           &NoArtifactError{},
			errContain:        "sha256:abc123",
			wantNoAttestation: 1,
		},
		{
			name:   "400 returns ClientError",
			record: testRecord(),
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte("bad request"))
			},
			wantErr:         true,
			errType:         &ClientError{},
			wantClientError: 1,
		},
		{
			name:   "403 forbidden returns ClientError",
			record: testRecord(),
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"message":"forbidden"}`))
			},
			wantErr:         true,
			errType:         &ClientError{},
			wantClientError: 1,
		},
		{
			name:   "422 invalid body returns ClientError",
			record: testRecord(),
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte("invalid body"))
			},
			wantErr:         true,
			errType:         &ClientError{},
			wantClientError: 1,
		},
		{
			name:    "429 rate limit retries then fails",
			record:  testRecord(),
			retries: 1,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
			},
			wantErr:         true,
			errContain:      "all retries exhausted",
			wantRateLimited: 2,
			wantHardFail:    1,
		},
		{
			name:    "403 with Retry-After header retries then fails",
			record:  testRecord(),
			retries: 1,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"message":"rate limit"}`))
			},
			wantErr:         true,
			errContain:      "all retries exhausted",
			wantRateLimited: 2,
			wantHardFail:    1,
		},
		{
			name:    "403 with x-ratelimit-remaining 0 retries then fails",
			record:  testRecord(),
			retries: 1,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-Ratelimit-Remaining", "0")
				w.Header().Set("X-Ratelimit-Reset", strconv.FormatInt(time.Now().Add(1*time.Second).Unix(), 10))
				w.WriteHeader(http.StatusForbidden)
			},
			wantErr:         true,
			errContain:      "all retries exhausted",
			wantRateLimited: 2,
			wantHardFail:    1,
		},
		{
			name:    "500 server error retries then fails",
			record:  testRecord(),
			retries: 1,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("internal error"))
			},
			wantErr:      true,
			errContain:   "all retries exhausted",
			wantSoftFail: 2,
			wantHardFail: 1,
		},
		{
			name:    "500 then 200 succeeds on retry",
			record:  testRecord(),
			retries: 2,
			handler: func() http.HandlerFunc {
				var count atomic.Int32
				return func(w http.ResponseWriter, _ *http.Request) {
					if count.Add(1) == 1 {
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					w.WriteHeader(http.StatusOK)
				}
			}(),
			wantSoftFail: 1,
			wantOk:       1,
		},
		{
			name:    "429 then 200 succeeds on retry",
			record:  testRecord(),
			retries: 2,
			handler: func() http.HandlerFunc {
				var count atomic.Int32
				return func(w http.ResponseWriter, _ *http.Request) {
					if count.Add(1) == 1 {
						w.Header().Set("Retry-After", "0")
						w.WriteHeader(http.StatusTooManyRequests)
						return
					}
					w.WriteHeader(http.StatusOK)
				}
			}(),
			wantRateLimited: 1,
			wantOk:          1,
		},
		{
			name:    "403 secondary rate limit then 200 succeeds on retry",
			record:  testRecord(),
			retries: 2,
			handler: func() http.HandlerFunc {
				var count atomic.Int32
				return func(w http.ResponseWriter, _ *http.Request) {
					if count.Add(1) == 1 {
						w.Header().Set("Retry-After", "0")
						w.WriteHeader(http.StatusForbidden)
						_, _ = w.Write([]byte(`{"message":"secondary rate limit"}`))
						return
					}
					w.WriteHeader(http.StatusOK)
				}
			}(),
			wantRateLimited: 1,
			wantOk:          1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			t.Cleanup(srv.Close)

			client, err := NewClient(srv.URL, "test-org", WithRetries(tt.retries))
			if err != nil {
				t.Fatalf("failed to create client: %v", err)
			}

			// Snapshot all counters before the call
			counters := allCounters()
			snapshots := make([]float64, len(counters))
			for i, c := range counters {
				snapshots[i] = testutil.ToFloat64(c)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			t.Cleanup(cancel)

			err = client.PostOne(ctx, tt.record)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tt.errType != nil {
					target := tt.errType
					switch target.(type) {
					case *NoArtifactError:
						var e *NoArtifactError
						if !errors.As(err, &e) {
							t.Errorf("expected NoArtifactError, got %T: %v", err, err)
						}
					case *ClientError:
						var e *ClientError
						if !errors.As(err, &e) {
							t.Errorf("expected ClientError, got %T: %v", err, err)
						}
					default:
						t.Fatalf("unexpected error type in test: %T", target)
					}
				}
				if tt.errContain != "" && !strings.Contains(err.Error(), tt.errContain) {
					t.Errorf("error %q should contain %q", err.Error(), tt.errContain)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Assert all metric deltas
			wantDeltas := []float64{
				tt.wantOk,
				tt.wantNoAttestation,
				tt.wantRateLimited,
				tt.wantSoftFail,
				tt.wantHardFail,
				tt.wantClientError,
			}
			names := []string{
				"PostDeploymentRecordOk",
				"PostDeploymentRecordNoAttestation",
				"PostDeploymentRecordRateLimited",
				"PostDeploymentRecordSoftFail",
				"PostDeploymentRecordHardFail",
				"PostDeploymentRecordClientError",
			}
			for i, c := range counters {
				got := testutil.ToFloat64(c) - snapshots[i]
				if got != wantDeltas[i] {
					t.Errorf("%s delta = %v, want %v", names[i], got, wantDeltas[i])
				}
			}
		})
	}
}

func TestPostOneSendsCorrectRequest(t *testing.T) {
	record := testRecord()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.URL.Path; got != "/orgs/test-org/artifacts/metadata/deployment-record" {
			t.Errorf("path = %s, want /orgs/test-org/artifacts/metadata/deployment-record", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %s, want application/json", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %s, want Bearer test-token", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	client, err := NewClient(srv.URL, "test-org",
		WithAPIToken("test-token"))
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	if err := client.PostOne(context.Background(), record); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseRateLimitDelay(t *testing.T) {
	tests := []struct {
		name    string
		headers http.Header
		wantMin time.Duration
		wantMax time.Duration
	}{
		{
			name:    "Retry-After in seconds",
			headers: http.Header{"Retry-After": []string{"5"}},
			wantMin: 5 * time.Second,
			wantMax: 5 * time.Second,
		},
		{
			name:    "Retry-After zero seconds",
			headers: http.Header{"Retry-After": []string{"0"}},
			wantMin: 0,
			wantMax: 0,
		},
		{
			name: "X-Ratelimit-Remaining 0 with reset",
			headers: http.Header{
				"X-Ratelimit-Remaining": []string{"0"},
				"X-Ratelimit-Reset":     []string{strconv.FormatInt(time.Now().Add(10*time.Second).Unix(), 10)},
			},
			wantMin: 9 * time.Second,
			wantMax: 11 * time.Second,
		},
		{
			name:    "no relevant headers defaults to 1 minute",
			headers: http.Header{},
			wantMin: time.Minute,
			wantMax: time.Minute,
		},
		{
			name: "Largest delay takes precedence",
			headers: http.Header{
				"Retry-After":           []string{"3"},
				"X-Ratelimit-Remaining": []string{"0"},
				"X-Ratelimit-Reset":     []string{strconv.FormatInt(time.Now().Add(60*time.Second).Unix(), 10)},
			},
			wantMin: 59 * time.Second,
			wantMax: 61 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{Header: tt.headers}
			result := parseRateLimitDelay(resp)
			if result < tt.wantMin || result > tt.wantMax {
				t.Errorf("parseRateLimitDelay() = %v, want between %v and %v", result, tt.wantMin, tt.wantMax)
			}
		})
	}
}

func TestPostOneRespectsRetryAfterAcrossGoroutines(t *testing.T) {
	var reqCount atomic.Int32
	firstReqDone := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count := reqCount.Add(1)
		if count == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			close(firstReqDone)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	client, err := NewClient(srv.URL, "test-org", WithRetries(2))
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	var wg sync.WaitGroup

	// Goroutine 1: triggers the rate limit
	wg.Go(func() {
		if err := client.PostOne(ctx, testRecord()); err != nil {
			t.Errorf("goroutine 1 error: %v", err)
		}
	})

	// Wait for the rate limit to be received and backoff set
	<-firstReqDone
	time.Sleep(50 * time.Millisecond)

	// Goroutine 2: must observe the shared backoff
	start := time.Now()
	wg.Go(func() {
		if err := client.PostOne(ctx, testRecord()); err != nil {
			t.Errorf("goroutine 2 error: %v", err)
		}
	})

	wg.Wait()

	elapsed := time.Since(start)
	if elapsed < 1500*time.Millisecond {
		t.Errorf("goroutine 2 should have waited for retry-after, but only waited %v", elapsed)
	}
}
