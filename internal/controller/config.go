package controller

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
