package controller

import (
	"strings"
)

const (
	// TmplNS is the meta variable for the k8s namespace.
	TmplNS = "{{namespace}}"
	// TmplDN is the meta variable for the k8s deployment name.
	TmplDN = "{{deploymentName}}"
	// TmplCN is the meta variable for the container name.
	TmplCN = "{{containerName}}"
)

// Config holds the global configuration for the controller.
type Config struct {
	Template            string
	LogicalEnvironment  string
	PhysicalEnvironment string
	Cluster             string
	//nolint:gosec
	APIToken    string
	BaseURL     string
	GHAppID     string
	GHInstallID string
	// GHAppPrivateKey must be the PEM Encoding of the
	// private key
	GHAppPrivateKey     []byte
	GHAppPrivateKeyPath string
	Organization        string
	// BulkClusterSync enables the async cluster job endpoint for startup
	// state sync. When false, startup sync is skipped and only individual
	// PostOne calls are used. Note: this is experimental and requires
	// enablement of a feature flag by GitHub. If enabled without the API-side
	// feature flag, the controller will not post deployment records at startup,
	// only ongoing pod events that arrive after the initial informer sync.
	BulkClusterSync bool
}

// ValidTemplate verifies that at least one placeholder is present
// in the provided template t.
func ValidTemplate(t string) bool {
	hasPlaceholder := strings.Contains(t, TmplNS) ||
		strings.Contains(t, TmplDN) ||
		strings.Contains(t, TmplCN)

	return hasPlaceholder
}
