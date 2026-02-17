package ociutil

// ExtractDigest extracts the digest from an ImageID.
// ImageID format is typically: docker-pullable://image@sha256:abc123...
// or docker://sha256:abc123...
func ExtractDigest(imageID string) string {
	if imageID == "" {
		return ""
	}

	// Look for sha256: in the imageID
	for i := 0; i < len(imageID)-7; i++ {
		if imageID[i:i+7] == "sha256:" {
			// Return everything from sha256: onwards
			remaining := imageID[i:]
			// Find end (could be end of string or next separator)
			end := len(remaining)
			for j, c := range remaining {
				if c == '@' || c == ' ' {
					end = j
					break
				}
			}
			return remaining[:end]
		}
	}

	return imageID
}
