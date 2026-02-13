package deploymentrecord

import "testing"

func TestValidateRuntimeRisk(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected RuntimeRisk
	}{
		{
			name:     "valid runtime risk",
			input:    "critical-resource",
			expected: CriticalResource,
		},
		{
			name:     "valid runtime risk with space",
			input:    "critical-resource ",
			expected: CriticalResource,
		},
		{
			name:     "invalid empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "invalid unknown risk",
			input:    "unknown-risk",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ValidateRuntimeRisk(tt.input)
			if result != tt.expected {
				t.Errorf("input: %s, actual: %q, wanted: %q", tt.input, result, tt.expected)
			}
		})
	}
}
