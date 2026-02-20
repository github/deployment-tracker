package metadata

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/github/deployment-tracker/pkg/deploymentrecord"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	metadatafake "k8s.io/client-go/metadata/fake"
	k8stesting "k8s.io/client-go/testing"
)

func newPartialObject(uid, name, kind string, annotations map[string]string, owners []metav1.OwnerReference) *metav1.PartialObjectMetadata {
	return &metav1.PartialObjectMetadata{
		TypeMeta: metav1.TypeMeta{Kind: kind, APIVersion: "apps/v1"},
		ObjectMeta: metav1.ObjectMeta{
			UID:             types.UID(uid),
			Name:            name,
			Namespace:       "default",
			Annotations:     annotations,
			OwnerReferences: owners,
		},
	}
}

func ownerRef(kind, name, uid string) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "apps/v1",
		Kind:       kind,
		Name:       name,
		UID:        types.UID(uid),
	}
}

func TestExtractMetadataFromObject(t *testing.T) {
	tests := []struct {
		name                 string
		annotations          map[string]string
		expectedRuntimeRisks map[deploymentrecord.RuntimeRisk]bool
		expectedTags         map[string]string
	}{
		{
			name:                 "no annotations",
			annotations:          nil,
			expectedRuntimeRisks: map[deploymentrecord.RuntimeRisk]bool{},
			expectedTags:         map[string]string{},
		},
		{
			name: "single runtime risk",
			annotations: map[string]string{
				MetadataAnnotationPrefix + RuntimeRisksAnnotationKey: "critical-resource",
			},
			expectedRuntimeRisks: map[deploymentrecord.RuntimeRisk]bool{
				deploymentrecord.CriticalResource: true,
			},
			expectedTags: map[string]string{},
		},
		{
			name: "multiple runtime risks",
			annotations: map[string]string{
				MetadataAnnotationPrefix + RuntimeRisksAnnotationKey: "critical-resource,internet-exposed,sensitive-data",
			},
			expectedRuntimeRisks: map[deploymentrecord.RuntimeRisk]bool{
				deploymentrecord.CriticalResource: true,
				deploymentrecord.InternetExposed:  true,
				deploymentrecord.SensitiveData:    true,
			},
			expectedTags: map[string]string{},
		},
		{
			name: "multiple runtime risks with spaces and capitals",
			annotations: map[string]string{
				MetadataAnnotationPrefix + RuntimeRisksAnnotationKey: "Critical-Resource, internet-exposed, sensitive-data",
			},
			expectedRuntimeRisks: map[deploymentrecord.RuntimeRisk]bool{
				deploymentrecord.CriticalResource: true,
				deploymentrecord.InternetExposed:  true,
				deploymentrecord.SensitiveData:    true,
			},
			expectedTags: map[string]string{},
		},
		{
			name: "invalid runtime risks are ignored",
			annotations: map[string]string{
				MetadataAnnotationPrefix + RuntimeRisksAnnotationKey: "critical-resource,not-a-risk",
			},
			expectedRuntimeRisks: map[deploymentrecord.RuntimeRisk]bool{
				deploymentrecord.CriticalResource: true,
			},
			expectedTags: map[string]string{},
		},
		{
			name: "custom tags extracted",
			annotations: map[string]string{
				MetadataAnnotationPrefix + "team": "platform",
				MetadataAnnotationPrefix + "env":  "prod",
			},
			expectedRuntimeRisks: map[deploymentrecord.RuntimeRisk]bool{},
			expectedTags:         map[string]string{"team": "platform", "env": "prod"},
		},
		{
			name: "runtime risks annotation excluded from tags",
			annotations: map[string]string{
				MetadataAnnotationPrefix + RuntimeRisksAnnotationKey: "critical-resource",
				MetadataAnnotationPrefix + "team":                    "platform",
			},
			expectedRuntimeRisks: map[deploymentrecord.RuntimeRisk]bool{
				deploymentrecord.CriticalResource: true,
			},
			expectedTags: map[string]string{"team": "platform"},
		},
		{
			name: "other annotations are ignored",
			annotations: map[string]string{
				"app.kubernetes.io/name":          "my-app",
				MetadataAnnotationPrefix + "team": "platform",
			},
			expectedRuntimeRisks: map[deploymentrecord.RuntimeRisk]bool{},
			expectedTags:         map[string]string{"team": "platform"},
		},
		{
			name: "max custom tags of 5 enforced",
			annotations: map[string]string{
				MetadataAnnotationPrefix + "tag1": "1",
				MetadataAnnotationPrefix + "tag2": "2",
				MetadataAnnotationPrefix + "tag3": "3",
				MetadataAnnotationPrefix + "tag4": "4",
				MetadataAnnotationPrefix + "tag5": "5",
				MetadataAnnotationPrefix + "tag6": "6",
			},
			expectedRuntimeRisks: map[deploymentrecord.RuntimeRisk]bool{},
			expectedTags:         map[string]string{"tag1": "1", "tag2": "2", "tag3": "3", "tag4": "4", "tag5": "5"},
		},
		{
			name: "tag key exceeding max length is skipped",
			annotations: map[string]string{
				MetadataAnnotationPrefix + strings.Repeat("k", MaxCustomTagLength+1): "val",
				MetadataAnnotationPrefix + "good":                                    "val",
			},
			expectedRuntimeRisks: map[deploymentrecord.RuntimeRisk]bool{},
			expectedTags:         map[string]string{"good": "val"},
		},
		{
			name: "tag value exceeding max length is skipped",
			annotations: map[string]string{
				MetadataAnnotationPrefix + "key":  strings.Repeat("v", MaxCustomTagLength+1),
				MetadataAnnotationPrefix + "good": "val",
			},
			expectedRuntimeRisks: map[deploymentrecord.RuntimeRisk]bool{},
			expectedTags:         map[string]string{"good": "val"},
		},
		{
			name: "empty tag key after prefix is skipped",
			annotations: map[string]string{
				MetadataAnnotationPrefix: "value",
			},
			expectedRuntimeRisks: map[deploymentrecord.RuntimeRisk]bool{},
			expectedTags:         map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := newPartialObject("uid-1", "test-pod", "Pod", tt.annotations, nil)
			agg := &AggregatePodMetadata{
				RuntimeRisks: make(map[deploymentrecord.RuntimeRisk]bool),
				Tags:         make(map[string]string),
			}

			extractMetadataFromObject(obj, agg)

			if len(agg.RuntimeRisks) != len(tt.expectedRuntimeRisks) {
				t.Errorf("RuntimeRisks count = %d, expected %d", len(agg.RuntimeRisks), len(tt.expectedRuntimeRisks))
			}
			for risk := range tt.expectedRuntimeRisks {
				if !agg.RuntimeRisks[risk] {
					t.Errorf("missing expected runtime risk %q", risk)
				}
			}

			if len(agg.Tags) != len(tt.expectedTags) {
				t.Errorf("Tags count = %d, expected %d\ngot:  %v\nexpected: %v", len(agg.Tags), len(tt.expectedTags), agg.Tags, tt.expectedTags)
			}
			for k, v := range tt.expectedTags {
				if agg.Tags[k] != v {
					t.Errorf("Tags[%q] = %q, expected %q", k, agg.Tags[k], v)
				}
			}
		})
	}
}

func TestAggregatePodMetadata(t *testing.T) {
	tests := []struct {
		name                 string
		pod                  *metav1.PartialObjectMetadata
		clusterObjects       []runtime.Object
		expectedRuntimeRisks map[deploymentrecord.RuntimeRisk]bool
		expectedTags         map[string]string
	}{
		{
			name: "pod with no owners",
			pod: newPartialObject("pod-1", "my-pod", "Pod", map[string]string{
				MetadataAnnotationPrefix + "team": "platform",
			}, nil),
			clusterObjects:       nil,
			expectedRuntimeRisks: map[deploymentrecord.RuntimeRisk]bool{},
			expectedTags:         map[string]string{"team": "platform"},
		},
		{
			name: "aggregates through full deployment ownership chain: pod -> replicaset -> deployment",
			pod: newPartialObject("pod-1", "my-pod", "Pod", map[string]string{
				MetadataAnnotationPrefix + "team": "platform",
			}, []metav1.OwnerReference{ownerRef("ReplicaSet", "my-rs", "rs-1")}),
			clusterObjects: []runtime.Object{
				newPartialObject("rs-1", "my-rs", "ReplicaSet", map[string]string{
					MetadataAnnotationPrefix + RuntimeRisksAnnotationKey: "internet-exposed",
				}, []metav1.OwnerReference{ownerRef("Deployment", "my-deploy", "deploy-1")}),
				newPartialObject("deploy-1", "my-deploy", "Deployment", map[string]string{
					MetadataAnnotationPrefix + RuntimeRisksAnnotationKey: "sensitive-data",
					MetadataAnnotationPrefix + "env":                     "prod",
				}, nil),
			},
			expectedRuntimeRisks: map[deploymentrecord.RuntimeRisk]bool{
				deploymentrecord.InternetExposed: true,
				deploymentrecord.SensitiveData:   true,
			},
			expectedTags: map[string]string{"team": "platform", "env": "prod"},
		},
		{
			name: "unsupported owner kind is skipped",
			pod: newPartialObject("pod-1", "my-pod", "Pod", nil,
				[]metav1.OwnerReference{ownerRef("ServiceAccount", "my-sa", "sa-1")}),
			clusterObjects:       nil,
			expectedRuntimeRisks: map[deploymentrecord.RuntimeRisk]bool{},
			expectedTags:         map[string]string{},
		},
		{
			name: "missing owner is skipped gracefully",
			pod: newPartialObject("pod-1", "my-pod", "Pod", map[string]string{
				MetadataAnnotationPrefix + "team": "platform",
			}, []metav1.OwnerReference{ownerRef("ReplicaSet", "gone-rs", "rs-gone")}),
			clusterObjects:       nil,
			expectedRuntimeRisks: map[deploymentrecord.RuntimeRisk]bool{},
			expectedTags:         map[string]string{"team": "platform"},
		},
		{
			name: "duplicate tags from owner do not overwrite pod tags",
			pod: newPartialObject("pod-1", "my-pod", "Pod", map[string]string{
				MetadataAnnotationPrefix + "team": "from-pod",
			}, []metav1.OwnerReference{ownerRef("ReplicaSet", "my-rs", "rs-1")}),
			clusterObjects: []runtime.Object{
				newPartialObject("rs-1", "my-rs", "ReplicaSet", map[string]string{
					MetadataAnnotationPrefix + "team": "from-rs",
				}, nil),
			},
			expectedRuntimeRisks: map[deploymentrecord.RuntimeRisk]bool{},
			expectedTags:         map[string]string{"team": "from-pod"},
		},
		{
			name: "runtime risks are merged across hierarchy",
			pod: newPartialObject("pod-1", "my-pod", "Pod", map[string]string{
				MetadataAnnotationPrefix + RuntimeRisksAnnotationKey: "critical-resource",
			}, []metav1.OwnerReference{ownerRef("ReplicaSet", "my-rs", "rs-1")}),
			clusterObjects: []runtime.Object{
				newPartialObject("rs-1", "my-rs", "ReplicaSet", map[string]string{
					MetadataAnnotationPrefix + RuntimeRisksAnnotationKey: "critical-resource,internet-exposed",
				}, nil),
			},
			expectedRuntimeRisks: map[deploymentrecord.RuntimeRisk]bool{
				deploymentrecord.CriticalResource: true,
				deploymentrecord.InternetExposed:  true,
			},
			expectedTags: map[string]string{},
		},
		{
			name: "cycle detection in ownership chain is handled correctly",
			pod: newPartialObject("pod-1", "my-pod", "Pod", map[string]string{
				MetadataAnnotationPrefix + "team": "platform",
			}, []metav1.OwnerReference{ownerRef("ReplicaSet", "my-rs", "rs-1")}),
			clusterObjects: []runtime.Object{
				newPartialObject("rs-1", "my-rs", "ReplicaSet", map[string]string{
					MetadataAnnotationPrefix + RuntimeRisksAnnotationKey: "critical-resource",
				}, []metav1.OwnerReference{ownerRef("Deployment", "my-deploy", "deploy-1")}),
				newPartialObject("deploy-1", "my-deploy", "Deployment", nil,
					[]metav1.OwnerReference{ownerRef("ReplicaSet", "my-rs", "rs-1")}),
			},
			expectedRuntimeRisks: map[deploymentrecord.RuntimeRisk]bool{
				deploymentrecord.CriticalResource: true,
			},
			expectedTags: map[string]string{"team": "platform"},
		},
		{
			name: "multiple owner references are all traversed",
			pod: newPartialObject("pod-1", "my-pod", "Pod", map[string]string{
				MetadataAnnotationPrefix + "team": "platform",
			}, []metav1.OwnerReference{
				ownerRef("ReplicaSet", "rs-1", "rs-1-uid"),
				ownerRef("ReplicaSet", "rs-2", "rs-2-uid"),
			}),
			clusterObjects: []runtime.Object{
				newPartialObject("rs-1-uid", "rs-1", "ReplicaSet", map[string]string{
					MetadataAnnotationPrefix + RuntimeRisksAnnotationKey: "critical-resource",
					MetadataAnnotationPrefix + "org":                     "engineering",
				}, nil),
				newPartialObject("rs-2-uid", "rs-2", "ReplicaSet", map[string]string{
					MetadataAnnotationPrefix + RuntimeRisksAnnotationKey: "internet-exposed",
					MetadataAnnotationPrefix + "env":                     "prod",
				}, nil),
			},
			expectedRuntimeRisks: map[deploymentrecord.RuntimeRisk]bool{
				deploymentrecord.CriticalResource: true,
				deploymentrecord.InternetExposed:  true,
			},
			expectedTags: map[string]string{"team": "platform", "org": "engineering", "env": "prod"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := metadatafake.NewTestScheme()
			_ = metav1.AddMetaToScheme(scheme)
			scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"}, &metav1.PartialObjectMetadata{})
			scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, &metav1.PartialObjectMetadata{})

			fakeClient := metadatafake.NewSimpleMetadataClient(scheme, tt.clusterObjects...)
			m := NewAggregator(fakeClient)

			result := m.AggregatePodMetadata(context.Background(), tt.pod)

			if len(result.RuntimeRisks) != len(tt.expectedRuntimeRisks) {
				t.Errorf("RuntimeRisks count = %d, expected %d", len(result.RuntimeRisks), len(tt.expectedRuntimeRisks))
			}
			for risk := range tt.expectedRuntimeRisks {
				if !result.RuntimeRisks[risk] {
					t.Errorf("missing expected runtime risk %q", risk)
				}
			}

			if len(result.Tags) != len(tt.expectedTags) {
				t.Errorf("Tags count = %d, expected %d\ngot:  %v\nexpected: %v", len(result.Tags), len(tt.expectedTags), result.Tags, tt.expectedTags)
			}
			for k, v := range tt.expectedTags {
				if result.Tags[k] != v {
					t.Errorf("Tags[%q] = %q, expected %q", k, result.Tags[k], v)
				}
			}
		})
	}
}

func TestAggregatePodMetadata_OwnerFetchError(t *testing.T) {
	scheme := metadatafake.NewTestScheme()
	_ = metav1.AddMetaToScheme(scheme)
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"}, &metav1.PartialObjectMetadata{})

	fakeClient := metadatafake.NewSimpleMetadataClient(scheme)
	fakeClient.PrependReactor("get", "replicasets", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("simulated API server error")
	})

	m := NewAggregator(fakeClient)

	pod := newPartialObject("pod-1", "my-pod", "Pod", map[string]string{
		MetadataAnnotationPrefix + "team": "platform",
	}, []metav1.OwnerReference{ownerRef("ReplicaSet", "my-rs", "rs-1")})

	result := m.AggregatePodMetadata(context.Background(), pod)

	if len(result.RuntimeRisks) != 0 {
		t.Errorf("expected no runtime risks, got %v", result.RuntimeRisks)
	}
	if len(result.Tags) != 1 || result.Tags["team"] != "platform" {
		t.Errorf("expected only pod-level tags, got %v", result.Tags)
	}
}
