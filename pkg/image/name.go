package image

import (
	"strings"
)

// ExtractName extracts the image name and tag from a container
// image reference.
// Returns the image name (without tag or digest) and the tag (or empty
// string if no tag).
// If the image only has a digest (no tag), the tag will be empty.
// Examples:
//   - "nginx:1.21" -> "nginx", "1.21"
//   - "nginx@sha256:abc123" -> "nginx", ""
//   - "nginx:1.21@sha256:abc123" -> "nginx", "1.21"
//   - "registry.example.com/myapp:v1.0" ->
//     "registry.example.com/myapp", "v1.0"
//   - "gcr.io/project/image:latest" -> "gcr.io/project/image", "latest"
//   - "localhost:5000/myapp:v1.0" -> "localhost:5000/myapp", "v1.0"
func ExtractName(image string) (string, string) {
	if image == "" {
		return "", ""
	}

	var tag string

	// First, remove digest if present (after @)
	if idx := strings.Index(image, "@"); idx != -1 {
		image = image[:idx]
	}

	// Then, extract and remove tag if present (after :)
	// But be careful with port numbers in registry URLs like
	// "localhost:5000/image:tag"
	// We need to find the last : that comes after the last /
	lastSlash := strings.LastIndex(image, "/")
	tagStart := strings.LastIndex(image, ":")

	// Only extract the tag if : comes after the last /
	// This handles cases like "localhost:5000/image" where we don't
	// want to extract ":5000" as tag
	if tagStart > lastSlash {
		tag = image[tagStart+1:]
		image = image[:tagStart]
	}

	return image, tag
}
