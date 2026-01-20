package deploymentrecord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/github/deployment-tracker/pkg/metrics"
)

// ClientOption is a function that configures the Client.
type ClientOption func(*Client)

// Client is an API client for posting deployment records.
type Client struct {
	baseURL    string
	org        string
	httpClient *http.Client
	retries    int
	apiToken   string
}

// NewClient creates a new API client with the given base URL and
// organization.
func NewClient(baseURL, org string, opts ...ClientOption) *Client {
	c := &Client{
		baseURL: baseURL,
		org:     org,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
		retries: 3,
	}

	for _, opt := range opts {
		opt(c)
	}

	return c
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
			// Wait before retry with exponential backoff
			time.Sleep(time.Duration(attempt*100) *
				time.Millisecond)
		}

		// Reset reader position for retries
		bodyReader.Reset(body)

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bodyReader)
		if err != nil {
			return fmt.Errorf("failed to create request: %w", err)
		}

		req.Header.Set("Content-Type", "application/json")
		if c.apiToken != "" {
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
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			metrics.PostDeploymentRecordOk.Inc()
			return nil
		}

		lastErr = fmt.Errorf("unexpected status code: %d", resp.StatusCode)

		// Don't retry on client errors (4xx) except for 429
		// (rate limit)
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 429 {
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
