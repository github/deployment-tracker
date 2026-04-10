package controller

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/github/deployment-tracker/pkg/dtmetrics"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
)

// registerEventHandlers adds pod event handlers to the informer. Events
// are filtered and enqueued to the controller's work queue for processing.
func (c *Controller) registerEventHandlers() error {
	_, err := c.podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				slog.Error("Invalid object returned",
					"object", obj,
				)
				return
			}

			// Only process pods that are running and belong
			// to a supported workload (Deployment, DaemonSet, StatefulSet, Job, or CronJob)
			if pod.Status.Phase == corev1.PodRunning && hasSupportedOwner(pod) {
				key, err := cache.MetaNamespaceKeyFunc(obj)

				// For our purposes, there are in practice
				// no error event we care about, so don't
				// bother with handling it.
				if err == nil {
					c.workqueue.Add(PodEvent{
						Key:       key,
						EventType: EventCreated,
					})
				}
			}

			// Also process Job-owned pods that completed before
			// we observed them in Running phase (e.g. sub-second Jobs).
			if isTerminalPhase(pod) && getJobOwnerName(pod) != "" {
				key, err := cache.MetaNamespaceKeyFunc(obj)
				if err == nil {
					c.workqueue.Add(PodEvent{
						Key:       key,
						EventType: EventCreated,
					})
				}
			}
		},
		UpdateFunc: func(oldObj, newObj any) {
			oldPod, ok := oldObj.(*corev1.Pod)
			if !ok {
				slog.Error("Invalid old object returned",
					"object", oldObj,
				)
				return
			}
			newPod, ok := newObj.(*corev1.Pod)
			if !ok {
				slog.Error("Invalid new object returned",
					"object", newObj,
				)
				return
			}

			// Skip if pod is being deleted or doesn't belong
			// to a supported workload.
			// Exception: Job-owned pods transitioning to a terminal phase
			// (Succeeded/Failed) from a non-Running state should still be
			// processed — this catches short-lived Jobs that skip Running.
			// We exclude Running→terminal transitions since those pods
			// were already enqueued when they entered Running.
			isJobTerminal := !isTerminalPhase(oldPod) && isTerminalPhase(newPod) &&
				oldPod.Status.Phase != corev1.PodRunning && getJobOwnerName(newPod) != ""
			if !isJobTerminal {
				if newPod.DeletionTimestamp != nil || !hasSupportedOwner(newPod) {
					return
				}
			}

			// Only process if pod just became running.
			// We need to process this as often when a container
			// is created, the spec does not contain the digest
			// so we need to wait for the status field to be
			// populated from where we can get the digest.
			if oldPod.Status.Phase != corev1.PodRunning &&
				newPod.Status.Phase == corev1.PodRunning {
				key, err := cache.MetaNamespaceKeyFunc(newObj)

				// For our purposes, there are in practice
				// no error event we care about, so don't
				// bother with handling it.
				if err == nil {
					c.workqueue.Add(PodEvent{
						Key:       key,
						EventType: EventCreated,
					})
				}
			}

			// Also catch Job-owned pods that transitioned directly
			// to a terminal phase without us seeing them as Running.
			if isJobTerminal {
				key, err := cache.MetaNamespaceKeyFunc(newObj)
				if err == nil {
					c.workqueue.Add(PodEvent{
						Key:       key,
						EventType: EventCreated,
					})
				}
			}
		},
		DeleteFunc: func(obj any) {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				// Handle deleted final state unknown
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					return
				}
				pod, ok = tombstone.Obj.(*corev1.Pod)
				if !ok {
					return
				}
			}

			// Only process pods that belong to a supported workload
			if !hasSupportedOwner(pod) {
				return
			}

			// Use the extracted pod for key derivation so that
			// tombstone objects are handled correctly.
			key, err := cache.MetaNamespaceKeyFunc(pod)
			// For our purposes, there are in practice
			// no error event we care about, so don't
			// bother with handling it.
			if err == nil {
				c.workqueue.Add(PodEvent{
					Key:        key,
					EventType:  EventDeleted,
					DeletedPod: pod,
				})
			}
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handlers: %w", err)
	}

	return nil
}

// runWorker runs a worker to process items from the work queue.
func (c *Controller) runWorker(ctx context.Context) {
	for c.processNextItem(ctx) {
	}
}

// startWorkers launches the specified number of workers and blocks until
// the context is cancelled.
func (c *Controller) startWorkers(ctx context.Context, workers int) {
	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, c.runWorker, time.Second)
	}
}

// processNextItem processes the next item from the work queue.
func (c *Controller) processNextItem(ctx context.Context) bool {
	event, shutdown := c.workqueue.Get()
	if shutdown {
		return false
	}
	defer c.workqueue.Done(event)

	start := time.Now()
	err := c.processEvent(ctx, event)
	dur := time.Since(start)

	if err == nil {
		dtmetrics.EventsProcessedOk.WithLabelValues(event.EventType).Inc()
		dtmetrics.EventsProcessedTimer.WithLabelValues("ok").Observe(dur.Seconds())

		c.workqueue.Forget(event)
		return true
	}
	dtmetrics.EventsProcessedTimer.WithLabelValues("failed").Observe(dur.Seconds())
	dtmetrics.EventsProcessedFailed.WithLabelValues(event.EventType).Inc()

	// Requeue on error with rate limiting
	slog.Error("Failed to process event, requeuing",
		"event_key", event.Key,
		"error", err,
	)
	c.workqueue.AddRateLimited(event)

	return true
}
