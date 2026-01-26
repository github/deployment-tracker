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
	APIToken            string
	BaseURL             string
	Organization        string
}

// ValidTemplate verifies that at least one placeholder is present
// in the provided template t.
func ValidTemplate(t string) bool {
	hasPlaceholder := strings.Contains(t, TmplNS) ||
		strings.Contains(t, TmplDN) ||
		strings.Contains(t, TmplCN)

	return hasPlaceholder
}
