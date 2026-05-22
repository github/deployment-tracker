package deploymentrecord

import (
	"log/slog"
	"strings"
)

// Status constants for deployment records.
const (
	StatusDeployed       = "deployed"
	StatusDecommissioned = "decommissioned"
)

// RuntimeRisk for deployment records.
type RuntimeRisk string

// Valid runtime risks.
const (
	CriticalResource RuntimeRisk = "critical-resource"
	InternetExposed  RuntimeRisk = "internet-exposed"
	LateralMovement  RuntimeRisk = "lateral-movement"
	SensitiveData    RuntimeRisk = "sensitive-data"
)

// Map of valid runtime risks.
var validRuntimeRisks = map[RuntimeRisk]bool{
	CriticalResource: true,
	InternetExposed:  true,
	LateralMovement:  true,
	SensitiveData:    true,
}

// DeploymentRecord represents a deployment event record.
type DeploymentRecord struct {
	DeploymentRecordBase
	LogicalEnvironment  string `json:"logical_environment"`
	PhysicalEnvironment string `json:"physical_environment"`
	Cluster             string `json:"cluster"`
}

// DeploymentRecordBase represents a deployment record for the deployment record cluster endpoint.
type DeploymentRecordBase struct {
	Name           string            `json:"name"`
	Digest         string            `json:"digest"`
	Version        string            `json:"version,omitempty"`
	Status         string            `json:"status"`
	DeploymentName string            `json:"deployment_name"`
	RuntimeRisks   []RuntimeRisk     `json:"runtime_risks,omitempty"`
	Tags           map[string]string `json:"tags,omitempty"`
}

// DeploymentRecords represents the post body for the deployment record cluster endpoint.
type DeploymentRecords struct {
	LogicalEnvironment  string                 `json:"logical_environment"`
	PhysicalEnvironment string                 `json:"physical_environment"`
	Deployments         []DeploymentRecordBase `json:"deployments"`
}

// NewDeploymentRecord creates a new DeploymentRecord with the given status.
// Status must be either StatusDeployed or StatusDecommissioned.
//
//nolint:revive
func NewDeploymentRecord(name, digest, version, logicalEnv, physicalEnv,
	cluster, status, deploymentName string, runtimeRisks []RuntimeRisk, tags map[string]string) *DeploymentRecord {
	// Validate status
	if status != StatusDeployed && status != StatusDecommissioned {
		status = StatusDeployed // default to deployed if invalid
	}

	return &DeploymentRecord{
		LogicalEnvironment:  logicalEnv,
		PhysicalEnvironment: physicalEnv,
		Cluster:             cluster,
		DeploymentRecordBase: DeploymentRecordBase{
			Name:           name,
			Digest:         digest,
			Version:        version,
			Status:         status,
			DeploymentName: deploymentName,
			RuntimeRisks:   runtimeRisks,
			Tags:           tags,
		},
	}
}

// ValidateRuntimeRisk confirms if string is a valid runtime risk,
// then returns the canonical runtime risk constant if valid, empty string otherwise.
func ValidateRuntimeRisk(risk string) RuntimeRisk {
	r := RuntimeRisk(strings.ToLower(strings.TrimSpace(risk)))
	if !validRuntimeRisks[r] {
		slog.Debug("Invalid runtime risk", "risk", risk)
		return ""
	}
	return r
}
