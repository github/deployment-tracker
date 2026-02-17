package ociutil

import (
	"testing"
)

func TestExtractDigest(t *testing.T) {
	tests := []struct {
		name     string
		imageID  string
		expected string
	}{
		{
			name:     "empty string",
			imageID:  "",
			expected: "",
		},
		{
			name:     "docker-pullable format",
			imageID:  "docker-pullable://nginx@sha256:abc123def456",
			expected: "sha256:abc123def456",
		},
		{
			name:     "docker format",
			imageID:  "docker://sha256:abc123def456789",
			expected: "sha256:abc123def456789",
		},
		{
			name:     "just sha256 digest",
			imageID:  "sha256:0123456789abcdef",
			expected: "sha256:0123456789abcdef",
		},
		{
			name:     "full gcr image with digest",
			imageID:  "docker-pullable://gcr.io/my-project/my-image@sha256:fedcba9876543210",
			expected: "sha256:fedcba9876543210",
		},
		{
			name:     "registry with port and digest",
			imageID:  "docker-pullable://localhost:5000/myapp@sha256:1234567890abcdef",
			expected: "sha256:1234567890abcdef",
		},
		{
			name:     "no sha256 prefix returns original",
			imageID:  "some-random-id-without-sha",
			expected: "some-random-id-without-sha",
		},
		{
			name:     "digest with trailing space",
			imageID:  "docker://sha256:abc123 extra",
			expected: "sha256:abc123",
		},
		{
			name:     "digest with trailing @",
			imageID:  "sha256:abc123@extra",
			expected: "sha256:abc123",
		},
		{
			name:     "real world kubernetes imageID",
			imageID:  "docker-pullable://ghcr.io/github/deployment-tracker@sha256:a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
			expected: "sha256:a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
		},
		{
			name:     "containerd format",
			imageID:  "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			expected: "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ExtractDigest(tt.imageID)
			if result != tt.expected {
				t.Errorf("ExtractDigest(%q) = %q, want %q", tt.imageID, result, tt.expected)
			}
		})
	}
}
