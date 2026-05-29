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

// BaseRecord represents a deployment record for the deployment record cluster endpoint.
type BaseRecord struct {
	Name           string            `json:"name"`
	Digest         string            `json:"digest"`
	Version        string            `json:"version,omitempty"`
	Status         string            `json:"status"`
	DeploymentName string            `json:"deployment_name"`
	RuntimeRisks   []RuntimeRisk     `json:"runtime_risks,omitempty"`
	Tags           map[string]string `json:"tags,omitempty"`
}

// Record represents a deployment event record.
type Record struct {
	BaseRecord
	LogicalEnvironment  string `json:"logical_environment"`
	PhysicalEnvironment string `json:"physical_environment"`
	Cluster             string `json:"cluster"`
}

// RecordResp represents the response of a created deployment record from the
// deployment record cluster endpoint.
type RecordResp struct {
	Record
	Created       string `json:"created"`
	UpdatedAt     string `json:"updated_at"`
	AttestationID int    `json:"attestation_id"`
}

// RecordErrorResp represents a failed deployment record from the
// deployment record cluster endpoint, including the cause of the failure.
type RecordErrorResp struct {
	Record
	Cause string `json:"cause"`
}

// ClusterRecordsBody represents the post body for the deployment record cluster endpoint.
type ClusterRecordsBody struct {
	LogicalEnvironment  string       `json:"logical_environment"`
	PhysicalEnvironment string       `json:"physical_environment"`
	PartialSuccess      bool         `json:"partial_success"`
	Deployments         []BaseRecord `json:"deployments"`
}

// RecordsClusterResp represents the response from the deployment record
// cluster endpoint, containing successfully created records and any errors.
type RecordsClusterResp struct {
	TotalCount        int                `json:"total_count"`
	DeploymentRecords []*RecordResp      `json:"deployment_records"`
	Errors            []*RecordErrorResp `json:"errors,omitempty"`
}

// NewDeploymentRecord creates a new Record with the given status.
// Status must be either StatusDeployed or StatusDecommissioned.
//
//nolint:revive
func NewDeploymentRecord(name, digest, version, logicalEnv, physicalEnv,
	cluster, status, deploymentName string, runtimeRisks []RuntimeRisk, tags map[string]string) *Record {
	// Validate status
	if status != StatusDeployed && status != StatusDecommissioned {
		status = StatusDeployed // default to deployed if invalid
	}

	return &Record{
		LogicalEnvironment:  logicalEnv,
		PhysicalEnvironment: physicalEnv,
		Cluster:             cluster,
		BaseRecord: BaseRecord{
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
