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
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/github/deployment-tracker/pkg/dtmetrics"
	"golang.org/x/time/rate"
)

// ClientOption is a function that configures the Client.
type ClientOption func(*Client)

// validOrgPattern validates organization names (alphanumeric, hyphens,
// underscores).
var validOrgPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// Client is an API client for posting deployment records.
type Client struct {
	baseURL          string
	org              string
	httpClient       *http.Client
	retries          int
	apiToken         string
	transport        *ghinstallation.Transport
	requestThrottler *rate.Limiter

	// rateLimitDeadline is a UnixNano timestamp shared across workers.
	rateLimitDeadline atomic.Int64
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
		// 3 req/sec (180 req/min) with burst of 20
		requestThrottler: rate.NewLimiter(rate.Limit(3), 20),
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

// WithGHApp configures a GitHub app to use for authentication.
// If provided values are invalid, this will panic.
// If an API token is also set, the GitHub App will take precedence.
func WithGHApp(id, installID string, pkBytes []byte, pkPath string) ClientOption {
	return func(c *Client) {
		if len(pkBytes) > 0 && pkPath != "" {
			panic("both GitHub App private key and private key path are set")
		}

		pid, err := strconv.Atoi(id)
		if err != nil {
			panic(err)
		}
		piid, err := strconv.Atoi(installID)
		if err != nil {
			panic(err)
		}

		if len(pkBytes) > 0 {
			c.transport, err = ghinstallation.New(
				http.DefaultTransport,
				int64(pid),
				int64(piid),
				pkBytes)
		} else {
			c.transport, err = ghinstallation.NewKeyFromFile(
				http.DefaultTransport,
				int64(pid),
				int64(piid),
				pkPath)
		}

		if err != nil {
			panic(err)
		}
	}
}

// WithRequestThrottler sets a custom rate limiter for API calls.
func WithRequestThrottler(rps float64, burst int) ClientOption {
	return func(c *Client) {
		c.requestThrottler = rate.NewLimiter(rate.Limit(rps), burst)
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

// ClusterNoRepositoriesError represents a 404 response from the cluster endpoint
// indicating no repositories were found for the given cluster.
type ClusterNoRepositoriesError struct {
	err error
}

func (c *ClusterNoRepositoriesError) Error() string {
	return fmt.Sprintf("cluster_no_repositories_error: %s", c.err.Error())
}

func (c *ClusterNoRepositoriesError) Unwrap() error {
	return c.err
}

// NoArtifactError represents a 404 client response whose body indicates "no artifacts found".
type NoArtifactError struct {
	err error
}

func (n *NoArtifactError) Error() string {
	if n == nil || n.err == nil {
		return "no artifact found"
	}
	return fmt.Sprintf("no artifact found: %s", n.err.Error())
}

func (n *NoArtifactError) Unwrap() error {
	return n.err
}

// PostOne posts a single deployment record to the GitHub deployment
// records API.
func (c *Client) PostOne(ctx context.Context, record *Record) error {
	if record == nil {
		return errors.New("record cannot be nil")
	}

	singleURL := fmt.Sprintf("%s/orgs/%s/artifacts/metadata/deployment-record", c.baseURL, c.org)

	body, err := buildRequestBody(record)
	if err != nil {
		return fmt.Errorf("failed to marshal record: %w", err)
	}

	respBody, statusCode, lastErr := c.postWithRetry(ctx, singleURL, body)

	var clientErr *ClientError
	switch {
	case errors.As(lastErr, &clientErr):
		dtmetrics.PostDeploymentRecordClientError.Inc()
		slog.Warn("client error, aborting",
			"status_code", statusCode,
			"url", singleURL,
			"resp_msg", string(respBody),
		)
		return fmt.Errorf("client error: %w", lastErr)
	case statusCode >= 200 && statusCode < 300:
		dtmetrics.PostDeploymentRecordOk.Inc()
		return nil
	case statusCode == 404:
		dtmetrics.PostDeploymentRecordUnknownArtifact.Inc()
		slog.Debug("no artifact attestation found, no record created",
			"status_code", statusCode,
			"container_name", record.Name,
			"resp_msg", string(respBody),
			"digest", record.Digest,
		)
		return &NoArtifactError{err: fmt.Errorf("no attestation found for %s", record.Digest)}
	default:
		dtmetrics.PostDeploymentRecordHardFail.Inc()
		slog.Error("all retries exhausted",
			"count", c.retries,
			"error", lastErr,
			"container_name", record.Name,
		)
		return fmt.Errorf("all retries exhausted: %w", lastErr)
	}
}

// PostCluster sends a full cluster state of records to GitHub deployment
// records cluster API.
func (c *Client) PostCluster(ctx context.Context, records []*Record, cluster string) ([]byte, error) {
	if len(records) == 0 {
		slog.Debug("Records is empty, skipping")
		return nil, nil
	}

	clusterURL := fmt.Sprintf("%s/orgs/%s/artifacts/metadata/deployment-record/cluster/%s", c.baseURL, c.org, url.PathEscape(cluster))

	body, err := buildClusterRequestBody(records)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal records: %w", err)
	}

	respBody, statusCode, lastErr := c.postWithRetry(ctx, clusterURL, body)

	var clientErr *ClientError
	switch {
	case errors.As(lastErr, &clientErr):
		dtmetrics.PostDeploymentRecordClientError.Inc()
		slog.Warn("client error, aborting",
			"status_code", statusCode,
			"url", clusterURL,
			"resp_msg", string(respBody),
		)
		return nil, fmt.Errorf("client error: %w", lastErr)
	case statusCode >= 200 && statusCode < 300:
		dtmetrics.PostDeploymentRecordOk.Inc()
		return respBody, nil
	case statusCode == 404:
		dtmetrics.PostDeploymentRecordUnknownArtifact.Inc()
		return nil, &ClusterNoRepositoriesError{err: errors.New("no repositories found")}
	default:
		dtmetrics.PostDeploymentRecordHardFail.Inc()
		slog.Error("all retries exhausted",
			"count", c.retries,
			"error", lastErr,
			"cluster", cluster,
		)
		return nil, fmt.Errorf("all retries exhausted: %w", lastErr)
	}
}

func (c *Client) postWithRetry(ctx context.Context, targetURL string, body []byte) ([]byte, int, error) {
	bodyReader := bytes.NewReader(body)
	var lastErr error
	// The first attempt is not a retry!
	for attempt := range c.retries + 1 {
		if err := waitForBackoff(ctx, attempt); err != nil {
			return nil, 0, err
		}

		if err := c.waitForServerRateLimit(ctx); err != nil {
			return nil, 0, err
		}

		if err := c.requestThrottler.Wait(ctx); err != nil {
			return nil, 0, fmt.Errorf("request throttler wait failed: %w", err)
		}

		// Reset reader position for retries
		bodyReader.Reset(body)

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bodyReader)
		if err != nil {
			return nil, 0, fmt.Errorf("failed to create request: %w", err)
		}

		req.Header.Set("Content-Type", "application/json")
		if c.transport != nil {
			// Token is thread safe, so no need for external
			// locking
			tok, err := c.transport.Token(ctx)
			if err != nil {
				return nil, 0, fmt.Errorf("failed to get access token: %w", err)
			}
			req.Header.Set("Authorization", "Bearer "+tok)
		} else if c.apiToken != "" {
			req.Header.Set("Authorization", "Bearer "+c.apiToken)
		}
		req.Header.Set("User-Agent", "GitHub-Deployment-Tracker")

		start := time.Now()
		// nolint: gosec
		resp, err := c.httpClient.Do(req)
		dur := time.Since(start)
		dtmetrics.PostDeploymentRecordTimer.Observe(dur.Seconds())
		if err != nil {
			lastErr = fmt.Errorf("post request failed: %w", err)

			slog.Warn("recoverable error, re-trying",
				"attempt", attempt,
				"retries", c.retries,
				"error", lastErr)
			dtmetrics.PostDeploymentRecordSoftFail.Inc()
			continue
		}

		// Drain and close response body to enable connection reuse
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			return respBody, resp.StatusCode, nil
		case resp.StatusCode == 404:
			// Not found - do not retry
			return respBody, resp.StatusCode, nil
		case resp.StatusCode >= 400 && resp.StatusCode < 500:
			// Check headers that indicate rate limiting
			if resp.Header.Get("Retry-After") != "" || resp.Header.Get("X-Ratelimit-Remaining") == "0" {
				retryDelay := parseRateLimitDelay(resp)
				c.setRetryAfter(retryDelay)
				dtmetrics.PostDeploymentRecordRateLimited.Inc()
				slog.Warn("rate limited, retrying",
					"attempt", attempt,
					"status_code", resp.StatusCode,
					"retry-after", resp.Header.Get("Retry-After"),
					"x-ratelimit-remaining", resp.Header.Get("X-Ratelimit-Remaining"),
					"retry_delay", retryDelay.Seconds(),
					"url", targetURL,
					"resp_msg", string(respBody),
				)
				lastErr = fmt.Errorf("rate limited, attempt %d", attempt)
				continue
			}
			// Don't retry non rate limiting client errors
			slog.Warn("client error, aborting",
				"attempt", attempt,
				"status_code", resp.StatusCode,
				"resp_msg", string(respBody),
			)
			return nil, resp.StatusCode, &ClientError{err: fmt.Errorf("unexpected client err with status code %d", resp.StatusCode)}
		default:
			// Retry with backoff
			dtmetrics.PostDeploymentRecordSoftFail.Inc()
			slog.Debug("retriable error",
				"attempt", attempt,
				"status_code", resp.StatusCode,
				"url", targetURL,
				"resp_msg", string(respBody),
			)
			lastErr = fmt.Errorf("server error, attempt %d", attempt)
		}
	}
	return nil, 0, lastErr
}

// waitForServerRateLimit blocks until the global server rate limit backoff has elapsed.
// All workers sharing this client observe the same deadline.
func (c *Client) waitForServerRateLimit(ctx context.Context) error {
	deadline := c.rateLimitDeadline.Load()
	delay := time.Until(time.Unix(0, deadline))
	if delay <= 0 {
		return nil
	}

	slog.Info("waiting for server rate limit backoff",
		"delay", delay.Round(time.Millisecond),
	)

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("context cancelled during server rate limit wait: %w", ctx.Err())
	}
}

// setRetryAfter records a global backoff deadline.
// Ensures deadline can only be extended, not shortened.
func (c *Client) setRetryAfter(d time.Duration) {
	newDeadline := time.Now().Add(d).UnixNano()
	for {
		current := c.rateLimitDeadline.Load()
		if newDeadline <= current {
			return
		}
		if c.rateLimitDeadline.CompareAndSwap(current, newDeadline) {
			return
		}
	}
}

// parseRateLimitDelay extracts the backoff duration from a rate-limit response:
// Return largest delay from header options.
// If no headers are set, default to 1 minute.
func parseRateLimitDelay(resp *http.Response) time.Duration {
	// GitHub docs show Retry-After header will always be an int
	var retryAfterDelay *time.Duration
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if seconds, err := strconv.Atoi(ra); err == nil {
			rad := time.Duration(seconds) * time.Second
			retryAfterDelay = &rad
		}
	}

	var rateLimitResetDelay *time.Duration
	if resp.Header.Get("X-Ratelimit-Remaining") == "0" {
		if resetStr := resp.Header.Get("X-Ratelimit-Reset"); resetStr != "" {
			if epoch, err := strconv.ParseInt(resetStr, 10, 64); err == nil {
				if d := time.Until(time.Unix(epoch, 0)); d > 0 {
					rateLimitResetDelay = &d
				}
			}
		}
	}

	switch {
	case retryAfterDelay != nil && rateLimitResetDelay != nil:
		return max(*retryAfterDelay, *rateLimitResetDelay)
	case retryAfterDelay != nil:
		return *retryAfterDelay
	case rateLimitResetDelay != nil:
		return *rateLimitResetDelay
	default:
		return time.Minute
	}
}

// buildRequestBody adds return_records=false to a deployment record request body
// which results in a minimal response payload.
func buildRequestBody(record *Record) ([]byte, error) {
	return json.Marshal(struct {
		Record
		ReturnRecords bool `json:"return_records"`
	}{
		Record:        *record,
		ReturnRecords: false,
	})
}

// buildClusterRequestBody count the total records, builds ClusterRecordsBody,
// and returns []byte.
func buildClusterRequestBody(records []*Record) ([]byte, error) {
	if len(records) == 0 {
		return nil, nil
	}
	deploymentRecords := []BaseRecord{}

	for _, r := range records {
		deploymentRecords = append(deploymentRecords, r.BaseRecord)
	}

	return json.Marshal(ClusterRecordsBody{
		LogicalEnvironment:  records[0].LogicalEnvironment,
		PhysicalEnvironment: records[0].PhysicalEnvironment,
		PartialSuccess:      true,
		Deployments:         deploymentRecords,
	})
}

func waitForBackoff(ctx context.Context, attempt int) error {
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
		timer := time.NewTimer(delay)
		defer timer.Stop()

		select {
		case <-timer.C:
		case <-ctx.Done():
			return fmt.Errorf("context cancelled during retry backoff: %w", ctx.Err())
		}
	}
	return nil
}
