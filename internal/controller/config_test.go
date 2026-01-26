package controller

import (
	"testing"
)

func TestValidTemplate(t *testing.T) {
	tests := []struct {
		name     string
		template string
		expected bool
	}{
		{
			name:     "empty string",
			template: "",
			expected: false,
		},
		{
			name:     "static string without placeholders",
			template: "static-deployment-name",
			expected: false,
		},
		{
			name:     "namespace placeholder only",
			template: "{{namespace}}",
			expected: true,
		},
		{
			name:     "deployment name placeholder only",
			template: "{{deploymentName}}",
			expected: true,
		},
		{
			name:     "container name placeholder only",
			template: "{{containerName}}",
			expected: true,
		},
		{
			name:     "all three placeholders",
			template: "{{namespace}}/{{deploymentName}}/{{containerName}}",
			expected: true,
		},
		{
			name:     "namespace and deployment name",
			template: "{{namespace}}-{{deploymentName}}",
			expected: true,
		},
		{
			name:     "mixed static and placeholders",
			template: "prefix-{{namespace}}-suffix",
			expected: true,
		},
		{
			name:     "placeholder with surrounding text",
			template: "app/{{containerName}}/prod",
			expected: true,
		},
		{
			name:     "similar but invalid placeholder",
			template: "{{namespaces}}",
			expected: false,
		},
		{
			name:     "partial placeholder - missing closing braces",
			template: "{{namespace",
			expected: false,
		},
		{
			name:     "partial placeholder - missing opening braces",
			template: "namespace}}",
			expected: false,
		},
		{
			name:     "wrong case placeholder",
			template: "{{Namespace}}",
			expected: false,
		},
		{
			name:     "placeholder with extra space",
			template: "{{ namespace }}",
			expected: false,
		},
		{
			name:     "default template format",
			template: TmplNS + "/" + TmplDN + "/" + TmplCN,
			expected: true,
		},
		{
			name:     "complex valid template",
			template: "org/{{namespace}}/env/{{deploymentName}}/container/{{containerName}}",
			expected: true,
		},
		{
			name:     "whitespace only",
			template: "   ",
			expected: false,
		},
		{
			name:     "special characters without placeholders",
			template: "app-name_v1.2.3",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ValidTemplate(tt.template)
			if result != tt.expected {
				t.Errorf("ValidTemplate(%q) = %v, expected %v", tt.template, result, tt.expected)
			}
		})
	}
}
