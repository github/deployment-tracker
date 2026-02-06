package deploymentrecord

// Status constants for deployment records.
const (
	StatusDeployed       = "deployed"
	StatusDecommissioned = "decommissioned"
)

// Runtime risks for deployment records.
type RuntimeRisk string

const (
	CriticalResource RuntimeRisk = "critical-resource"
	InternetExposed  RuntimeRisk = "internet-exposed"
	LateralMovement  RuntimeRisk = "lateral-movement"
	SensitiveData    RuntimeRisk = "sensitive-data"
)

// DeploymentRecord represents a deployment event record.
type DeploymentRecord struct {
	Name                string        `json:"name"`
	Digest              string        `json:"digest"`
	Version             string        `json:"version,omitempty"`
	LogicalEnvironment  string        `json:"logical_environment"`
	PhysicalEnvironment string        `json:"physical_environment"`
	Cluster             string        `json:"cluster"`
	Status              string        `json:"status"`
	DeploymentName      string        `json:"deployment_name"`
	RuntimeRisks        []RuntimeRisk `json:"runtime_risks,omitempty"`
}

// NewDeploymentRecord creates a new DeploymentRecord with the given status.
// Status must be either StatusDeployed or StatusDecommissioned.
//
//nolint:revive
func NewDeploymentRecord(name, digest, version, logicalEnv, physicalEnv,
	cluster, status, deploymentName string, runtimeRisks []RuntimeRisk) *DeploymentRecord {
	// Validate status
	if status != StatusDeployed && status != StatusDecommissioned {
		status = StatusDeployed // default to deployed if invalid
	}

	return &DeploymentRecord{
		Name:                name,
		Digest:              digest,
		Version:             version,
		LogicalEnvironment:  logicalEnv,
		PhysicalEnvironment: physicalEnv,
		Cluster:             cluster,
		Status:              status,
		DeploymentName:      deploymentName,
		RuntimeRisks:        runtimeRisks,
	}
}

// ValidateRuntimeRisk confirms is string is a valid runtime risk,
// then returns the canonical runtime risk constant if valid, empty string otherwise.
func ValidateRuntimeRisk(risk string) RuntimeRisk {
	r := RuntimeRisk(risk)
	switch r {
	case CriticalResource, InternetExposed, LateralMovement, SensitiveData:
		return r
	default:
		return ""
	}
}
