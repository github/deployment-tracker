package image

import (
	"testing"
)

func TestExtractName(t *testing.T) {
	tests := []struct {
		name        string
		image       string
		expectedImg string
		expectedTag string
	}{
		{
			name:        "simple image with tag",
			image:       "nginx:1.21",
			expectedImg: "nginx",
			expectedTag: "1.21",
		},
		{
			name:        "simple image with latest tag",
			image:       "nginx:latest",
			expectedImg: "nginx",
			expectedTag: "latest",
		},
		{
			name:        "image with digest",
			image:       "nginx@sha256:abc123def456",
			expectedImg: "nginx",
			expectedTag: "",
		},
		{
			name:        "image with tag and digest",
			image:       "nginx:1.21@sha256:abc123def456",
			expectedImg: "nginx",
			expectedTag: "1.21",
		},
		{
			name:        "registry with port and tag",
			image:       "localhost:5000/myapp:v1.0",
			expectedImg: "localhost:5000/myapp",
			expectedTag: "v1.0",
		},
		{
			name:        "registry with port and digest",
			image:       "localhost:5000/myapp@sha256:abc123",
			expectedImg: "localhost:5000/myapp",
			expectedTag: "",
		},
		{
			name:        "registry with port no tag",
			image:       "localhost:5000/myapp",
			expectedImg: "localhost:5000/myapp",
			expectedTag: "",
		},
		{
			name:        "gcr image with tag",
			image:       "gcr.io/my-project/my-image:v1.0.0",
			expectedImg: "gcr.io/my-project/my-image",
			expectedTag: "v1.0.0",
		},
		{
			name:        "docker hub with namespace and tag",
			image:       "myuser/myapp:latest",
			expectedImg: "myuser/myapp",
			expectedTag: "latest",
		},
		{
			name:        "full registry path with tag",
			image:       "registry.example.com/namespace/image:tag",
			expectedImg: "registry.example.com/namespace/image",
			expectedTag: "tag",
		},
		{
			name:        "image without tag",
			image:       "nginx",
			expectedImg: "nginx",
			expectedTag: "",
		},
		{
			name:        "empty string",
			image:       "",
			expectedImg: "",
			expectedTag: "",
		},
		{
			name:        "ghcr image with tag",
			image:       "ghcr.io/owner/repo/image:v2.0",
			expectedImg: "ghcr.io/owner/repo/image",
			expectedTag: "v2.0",
		},
		{
			name:        "ecr image with tag",
			image:       "123456789.dkr.ecr.us-east-1.amazonaws.com/my-app:latest",
			expectedImg: "123456789.dkr.ecr.us-east-1.amazonaws.com/my-app",
			expectedTag: "latest",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resultImg, resultTag := ExtractName(tt.image)
			if resultImg != tt.expectedImg {
				t.Errorf("ExtractName(%q) image = %q, expected %q", tt.image, resultImg, tt.expectedImg)
			}
			if resultTag != tt.expectedTag {
				t.Errorf("ExtractName(%q) tag = %q, expected %q", tt.image, resultTag, tt.expectedTag)
			}
		})
	}
}
