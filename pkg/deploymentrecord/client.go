package deploymentrecord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/github/deployment-tracker/pkg/metrics"
	"github.com/bradleyfalzon/ghinstallation/v2"
	"golang.org/x/time/rate"
)

// ClientOption is a function that configures the Client.
type ClientOption func(*Client)

// validOrgPattern validates organization names (alphanumeric, hyphens,
// underscores).
var validOrgPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// Client is an API client for posting deployment records.
type Client struct {
	baseURL     string
	org         string
	httpClient  *http.Client
	retries     int
	apiToken    string
	transport   *ghinstallation.Transport
	rateLimiter *rate.Limiter
}

// NewClient creates a new API client with the given base URL and
// organization. Returns an error if the base URL is not HTTPS for
// non-local hosts.
func NewClient(baseURL, org string, opts ...ClientOption) (*Client, error) {
	// Check if URL is local (allowed to use HTTP)
	isLocal := strings.HasPrefix(baseURL, "http://localhost") ||
		strings.HasPrefix(baseURL, "http://127.0.0.1") ||
		strings.Contains(baseURL, ".svc.cluster.local")

	// Reject non-HTTPS URLs for non-local hosts
	if strings.HasPrefix(baseURL, "http://") && !isLocal {
		return nil, fmt.Errorf("insecure URL not allowed: %s (use HTTPS for non-local hosts)", baseURL)
	}

	// Add https:// prefix if no scheme is provided
	if !strings.HasPrefix(baseURL, "https://") && !strings.HasPrefix(baseURL, "http://") {
		baseURL = "https://" + baseURL
	}

	// Validate organization name to prevent URL injection
	if !validOrgPattern.MatchString(org) {
		return nil, fmt.Errorf("invalid organization name: %s (must be alphanumeric, hyphens, or underscores)", org)
	}

	c := &Client{
		baseURL: baseURL,
		org:     org,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
		retries: 3,
		// 20 req/sec with burst of 50
		rateLimiter: rate.NewLimiter(rate.Limit(20), 50),
	}

	for _, opt := range opts {
		opt(c)
	}

	return c, nil
}

// WithTimeout sets the HTTP client timeout in seconds.
func WithTimeout(seconds int) ClientOption {
	return func(c *Client) {
		c.httpClient.Timeout = time.Duration(seconds) * time.Second
	}
}

// WithRetries sets the number of retries for failed requests.
func WithRetries(retries int) ClientOption {
	return func(c *Client) {
		c.retries = retries
	}
}

// WithAPIToken sets the API token for Bearer authentication.
func WithAPIToken(token string) ClientOption {
	return func(c *Client) {
		c.apiToken = token
	}
}

// WithGHApp configres an GitHub app to use for authentication.
// If provided values are invalid, this will panic.
// If an API token is also set, the GitHub App will take precedence.
func WithGHApp(id, installId, pk string) ClientOption {
	return func(c *Client) {
		pid, err := strconv.Atoi(id)
		if err != nil {
			panic(err)
		}
		piid, err := strconv.Atoi(installId)
		if err != nil {
			panic(err)
		}
		c.transport, err = ghinstallation.NewKeyFromFile(
			http.DefaultTransport,
			int64(pid),
			int64(piid),
			pk)
		if err != nil {
			panic(err)
		}
	}
}

// WithRateLimiter sets a custom rate limiter for API calls.
func WithRateLimiter(rps float64, burst int) ClientOption {
	return func(c *Client) {
		c.rateLimiter = rate.NewLimiter(rate.Limit(rps), burst)
	}
}

// ClientError represents a client error that can not be retried.
type ClientError struct {
	err error
}

func (c *ClientError) Error() string {
	return fmt.Sprintf("client_error: %s", c.err.Error())
}

func (c *ClientError) Unwrap() error {
	return c.err
}

// PostOne posts a single deployment record to the GitHub deployment
// records API.
func (c *Client) PostOne(ctx context.Context, record *DeploymentRecord) error {
	if record == nil {
		return errors.New("record cannot be nil")
	}

	// Wait for rate limiter
	if err := c.rateLimiter.Wait(ctx); err != nil {
		return fmt.Errorf("rate limiter wait failed: %w", err)
	}

	url := fmt.Sprintf("%s/orgs/%s/artifacts/metadata/deployment-record", c.baseURL, c.org)

	body, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("failed to marshal record: %w", err)
	}

	bodyReader := bytes.NewReader(body)

	var lastErr error
	// The first attempt is not a retry!
	for attempt := range c.retries + 1 {
		if attempt > 0 {
			backoff := time.Duration(math.Pow(2,
				float64(attempt))) * 100 * time.Millisecond
			//nolint:gosec
			jitter := time.Duration(rand.Int64N(50)) * time.Millisecond
			delay := backoff + jitter

			if delay > 5*time.Second {
				delay = 5 * time.Second
			}

			// Wait with context cancellation support
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return fmt.Errorf("context cancelled during retry backoff: %w", ctx.Err())
			}
		}

		// Reset reader position for retries
		bodyReader.Reset(body)

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bodyReader)
		if err != nil {
			return fmt.Errorf("failed to create request: %w", err)
		}

		req.Header.Set("Content-Type", "application/json")
		if c.transport != nil {
			// Token is thread safe, so no need for external
			// locking
			tok, err := c.transport.Token(ctx)
			if err != nil {
				return fmt.Errorf("failed to get access token: %w", err)
			}
			req.Header.Set("Authorization", "Bearer "+tok)
		} else if c.apiToken != "" {
			req.Header.Set("Authorization", "Bearer "+c.apiToken)
		}

		start := time.Now()
		resp, err := c.httpClient.Do(req)
		dur := time.Since(start)
		metrics.PostDeploymentRecordTimer.Observe(dur.Seconds())
		if err != nil {
			lastErr = fmt.Errorf("post request failed: %w", err)

			slog.Warn("recoverable error, re-trying",
				"attempt", attempt,
				"retries", c.retries,
				"error", lastErr)
			metrics.PostDeploymentRecordSoftFail.Inc()
			continue
		}

		// Drain and close response body to enable connection reuse
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			metrics.PostDeploymentRecordOk.Inc()
			return nil
		}

		lastErr = fmt.Errorf("unexpected status code: %d", resp.StatusCode)

		// Don't retry on client errors (4xx) except for 429
		// (rate limit)
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 429 {
			metrics.PostDeploymentRecordClientError.Inc()
			slog.Warn("client error, aborting",
				"attempt", attempt,
				"error", lastErr)
			return &ClientError{err: lastErr}
		}
		metrics.PostDeploymentRecordSoftFail.Inc()
	}

	metrics.PostDeploymentRecordHardFail.Inc()
	slog.Error("all retries exhausted",
		"count", c.retries,
		"error", lastErr)
	return fmt.Errorf("all retries exhausted: %w", lastErr)
}
