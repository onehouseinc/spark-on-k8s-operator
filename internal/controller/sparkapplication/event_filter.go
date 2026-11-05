/*
Copyright 2024 The kubeflow authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sparkapplication

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/kubeflow/spark-operator/api/v1beta2"
	"github.com/kubeflow/spark-operator/pkg/util"
)

// sparkPodEventFilter filters Spark pod events.
type sparkPodEventFilter struct {
	namespaces map[string]bool
}

// sparkPodEventFilter implements the predicate.Predicate interface.
var _ predicate.Predicate = &sparkPodEventFilter{}

// newSparkPodEventFilter creates a new SparkPodEventFilter instance.
func newSparkPodEventFilter(namespaces []string) *sparkPodEventFilter {
	nsMap := make(map[string]bool)
	if len(namespaces) == 0 {
		nsMap[metav1.NamespaceAll] = true
	} else {
		for _, ns := range namespaces {
			nsMap[ns] = true
		}
	}

	return &sparkPodEventFilter{
		namespaces: nsMap,
	}
}

// Create implements predicate.Predicate.
func (f *sparkPodEventFilter) Create(e event.CreateEvent) bool {
	pod, ok := e.Object.(*corev1.Pod)
	if !ok {
		return false
	}

	return f.filter(pod)
}

// Update implements predicate.Predicate.
func (f *sparkPodEventFilter) Update(e event.UpdateEvent) bool {
	oldPod, ok := e.ObjectOld.(*corev1.Pod)
	if !ok {
		return false
	}

	newPod, ok := e.ObjectNew.(*corev1.Pod)
	if !ok {
		return false
	}

	if newPod.Status.Phase == oldPod.Status.Phase {
		return false
	}

	return f.filter(newPod)
}

// Delete implements predicate.Predicate.
func (f *sparkPodEventFilter) Delete(e event.DeleteEvent) bool {
	pod, ok := e.Object.(*corev1.Pod)
	if !ok {
		return false
	}

	return f.filter(pod)
}

// Generic implements predicate.Predicate.
func (f *sparkPodEventFilter) Generic(e event.GenericEvent) bool {
	pod, ok := e.Object.(*corev1.Pod)
	if !ok {
		return false
	}

	return f.filter(pod)
}

func (f *sparkPodEventFilter) filter(pod *corev1.Pod) bool {
	if !util.IsLaunchedBySparkOperator(pod) {
		return false
	}

	return f.namespaces[metav1.NamespaceAll] || f.namespaces[pod.Namespace]
}

// SparkApplicationEventFilter limits events to managed namespaces and retires
// the current run when its desired spec changes.
type SparkApplicationEventFilter struct {
	statusWriter client.Client
	// apiReader refreshes the application only after a status-write conflict.
	apiReader  client.Reader
	recorder   record.EventRecorder
	namespaces map[string]bool
}

var _ predicate.Predicate = &SparkApplicationEventFilter{}

// NewSparkApplicationEventFilter uses the watch object for invalidation and live
// reads only to retry conflicts. Watch objects are never mutated in place.
func NewSparkApplicationEventFilter(writer client.Client, apiReader client.Reader, recorder record.EventRecorder, namespaces []string) *SparkApplicationEventFilter {
	nsMap := make(map[string]bool)
	if len(namespaces) == 0 {
		nsMap[metav1.NamespaceAll] = true
	} else {
		for _, ns := range namespaces {
			nsMap[ns] = true
		}
	}

	return &SparkApplicationEventFilter{
		statusWriter: writer,
		apiReader:    apiReader,
		recorder:     recorder,
		namespaces:   nsMap,
	}
}

// Create implements predicate.Predicate.
func (f *SparkApplicationEventFilter) Create(e event.CreateEvent) bool {
	app, ok := e.Object.(*v1beta2.SparkApplication)
	if !ok {
		return false
	}

	return f.filter(app)
}

// Update enqueues changes and marks a changed spec INVALIDATING only if the event
// still describes the live application and submission. Failed invalidation writes
// still enqueue reconciliation so stamped drivers can expose a spec mismatch.
func (f *SparkApplicationEventFilter) Update(e event.UpdateEvent) bool {
	oldApp, ok := e.ObjectOld.(*v1beta2.SparkApplication)
	if !ok {
		return false
	}

	newApp, ok := e.ObjectNew.(*v1beta2.SparkApplication)
	if !ok {
		return false
	}

	if !f.filter(newApp) {
		return false
	}

	if oldApp.ResourceVersion == newApp.ResourceVersion && !util.IsExpired(newApp) && !util.ShouldRetry(newApp) {
		return false
	}

	if !equality.Semantic.DeepEqual(oldApp.Spec, newApp.Spec) {
		firstAttempt := true
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			// The event's resourceVersion protects the first write; only a conflict
			// requires a live read to avoid retrying the same stale cache entry.
			latest := newApp.DeepCopy()
			if !firstAttempt {
				latest = &v1beta2.SparkApplication{}
				if err := f.apiReader.Get(context.TODO(), client.ObjectKeyFromObject(newApp), latest); err != nil {
					return client.IgnoreNotFound(err)
				}
			}
			firstAttempt = false
			// A delayed event must not invalidate a replacement app or a newer submission.
			if latest.UID != newApp.UID || latest.Generation != newApp.Generation ||
				latest.Status.SubmissionID != newApp.Status.SubmissionID ||
				!latest.DeletionTimestamp.IsZero() || latest.Status.AppState.State == v1beta2.ApplicationStateInvalidating {
				return nil
			}
			latest.Status.AppState.State = v1beta2.ApplicationStateInvalidating
			return client.IgnoreNotFound(f.statusWriter.Status().Update(context.TODO(), latest))
		})
		if err != nil {
			logger.Error(err, "Failed to update application status", "application", newApp.Name)
			f.recorder.Eventf(
				newApp,
				corev1.EventTypeWarning,
				"SparkApplicationSpecUpdateFailed",
				"Failed to update spec for SparkApplication %s: %v",
				newApp.Name,
				err,
			)
		}
	}

	return true
}

// Delete implements predicate.Predicate.
func (f *SparkApplicationEventFilter) Delete(e event.DeleteEvent) bool {
	app, ok := e.Object.(*v1beta2.SparkApplication)
	if !ok {
		return false
	}

	return f.filter(app)
}

// Generic implements predicate.Predicate.
func (f *SparkApplicationEventFilter) Generic(e event.GenericEvent) bool {
	app, ok := e.Object.(*v1beta2.SparkApplication)
	if !ok {
		return false
	}

	return f.filter(app)
}

func (f *SparkApplicationEventFilter) filter(app *v1beta2.SparkApplication) bool {
	return f.namespaces[metav1.NamespaceAll] || f.namespaces[app.Namespace]
}
