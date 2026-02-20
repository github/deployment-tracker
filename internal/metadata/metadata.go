package metadata

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/github/deployment-tracker/pkg/deploymentrecord"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8smetadata "k8s.io/client-go/metadata"
)

const (
	// MetadataAnnotationPrefix is the annotation key prefix for deployment record metadata like runtime risk and tags.
	MetadataAnnotationPrefix = "metadata.github.com/"
	// RuntimeRisksAnnotationKey is the tag key for runtime risks. Comes after MetadataAnnotationPrefix.
	RuntimeRisksAnnotationKey = "runtime-risks"
	// MaxCustomTags is the maximum number of custom tags per deployment record.
	MaxCustomTags = 5
	// MaxCustomTagLength is the maximum length for a custom tag key or value.
	MaxCustomTagLength = 100
)

// Aggregator uses the Kubernetes metadata client to aggregate metadata for a pod and its ownership hierarchy.
type Aggregator struct {
	metadataClient k8smetadata.Interface
}

// AggregatePodMetadata represents combined metadata for a pod and its ownership hierarchy.
type AggregatePodMetadata struct {
	RuntimeRisks map[deploymentrecord.RuntimeRisk]bool
	Tags         map[string]string
}

// NewAggregator creates a new Aggregator with a Kubernetes metadata client.
func NewAggregator(metadataClient k8smetadata.Interface) *Aggregator {
	return &Aggregator{
		metadataClient: metadataClient,
	}
}

// BuildAggregatePodMetadata takes a pod's partial object metadata and traverses its ownership hierarchy to return AggregatePodMetadata.
func (m *Aggregator) BuildAggregatePodMetadata(ctx context.Context, obj *metav1.PartialObjectMetadata) *AggregatePodMetadata {
	aggMetadata := &AggregatePodMetadata{
		RuntimeRisks: make(map[deploymentrecord.RuntimeRisk]bool),
		Tags:         make(map[string]string),
	}
	queue := []*metav1.PartialObjectMetadata{obj}
	visited := make(map[types.UID]bool)

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if visited[current.GetUID()] {
			slog.Warn("Already visited object, skipping to avoid cycles",
				"UID", current.GetUID(),
				"name", current.GetName(),
			)
			continue
		}
		visited[current.GetUID()] = true

		extractMetadataFromObject(current, aggMetadata)
		m.addOwnersToQueue(ctx, current, &queue)
	}

	return aggMetadata
}

// addOwnersToQueue takes a current object and looks up its owners, adding them to the queue for processing
// to collect their metadata.
func (m *Aggregator) addOwnersToQueue(ctx context.Context, current *metav1.PartialObjectMetadata, queue *[]*metav1.PartialObjectMetadata) {
	ownerRefs := current.GetOwnerReferences()

	for _, owner := range ownerRefs {
		ownerObj, err := m.getOwnerMetadata(ctx, current.GetNamespace(), owner)
		if err != nil {
			slog.Warn("Failed to get owner object for metadata collection",
				"namespace", current.GetNamespace(),
				"owner_kind", owner.Kind,
				"owner_name", owner.Name,
				"error", err,
			)
			continue
		}

		if ownerObj == nil {
			continue
		}

		*queue = append(*queue, ownerObj)
	}
}

// getOwnerMetadata retrieves partial object metadata for an owner ref.
func (m *Aggregator) getOwnerMetadata(ctx context.Context, namespace string, owner metav1.OwnerReference) (*metav1.PartialObjectMetadata, error) {
	gvr := schema.GroupVersionResource{
		Group:   "apps",
		Version: "v1",
	}

	switch owner.Kind {
	case "ReplicaSet":
		gvr.Resource = "replicasets"
	case "Deployment":
		gvr.Resource = "deployments"
	default:
		slog.Debug("Unsupported owner kind for metadata collection",
			"kind", owner.Kind,
			"name", owner.Name,
		)
		return nil, nil
	}

	obj, err := m.metadataClient.Resource(gvr).Namespace(namespace).Get(ctx, owner.Name, metav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			slog.Debug("Owner object not found for metadata collection",
				"namespace", namespace,
				"owner_kind", owner.Kind,
				"owner_name", owner.Name,
			)
			return nil, nil
		}
		return nil, err
	}
	return obj, nil
}

// extractMetadataFromObject extracts metadata from an object.
func extractMetadataFromObject(obj *metav1.PartialObjectMetadata, aggPodMetadata *AggregatePodMetadata) {
	annotations := obj.GetAnnotations()

	// Extract runtime risks
	if risks, exists := annotations[MetadataAnnotationPrefix+RuntimeRisksAnnotationKey]; exists {
		for _, risk := range strings.Split(risks, ",") {
			r := deploymentrecord.ValidateRuntimeRisk(risk)
			if r != "" {
				aggPodMetadata.RuntimeRisks[r] = true
			}
		}
	}

	// Extract tags by sorted keys to ensure tags are deterministic
	// if over the limit and some are dropped, the same ones will be dropped each time.
	keys := make([]string, 0, len(annotations))
	for key := range annotations {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	for _, key := range keys {
		if len(aggPodMetadata.Tags) >= MaxCustomTags {
			break
		}

		if strings.HasPrefix(key, MetadataAnnotationPrefix) {
			tagKey := strings.TrimPrefix(key, MetadataAnnotationPrefix)
			tagValue := annotations[key]

			if RuntimeRisksAnnotationKey == tagKey {
				// ignore runtime risks for custom tags
				continue
			}
			if utf8.RuneCountInString(tagKey) > MaxCustomTagLength || utf8.RuneCountInString(tagValue) > MaxCustomTagLength {
				slog.Warn("Tag key or value exceeds max length, skipping",
					"object_name", obj.GetName(),
					"kind", obj.Kind,
					"tag_key", tagKey,
					"tag_value", tagValue,
					"key_length", utf8.RuneCountInString(tagKey),
					"value_length", utf8.RuneCountInString(tagValue),
					"max_length", MaxCustomTagLength,
				)
				continue
			}
			if tagKey == "" {
				slog.Warn("Tag key is empty, skipping",
					"object_name", obj.GetName(),
					"kind", obj.Kind,
					"annotation", key,
					"tag_key", tagKey,
					"tag_value", tagValue,
				)
				continue
			}
			if _, exists := aggPodMetadata.Tags[tagKey]; exists {
				slog.Debug("Duplicate tag key found, skipping",
					"object_name", obj.GetName(),
					"kind", obj.Kind,
					"tag_key", tagKey,
					"existing_value", aggPodMetadata.Tags[tagKey],
					"new_value", tagValue,
				)
				continue
			}
			aggPodMetadata.Tags[tagKey] = tagValue
		}
	}
}
