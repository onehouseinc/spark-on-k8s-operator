/*
Copyright 2026 The Kubeflow authors.

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
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubeflow/spark-operator/api/v1beta2"
	"github.com/kubeflow/spark-operator/pkg/common"
	"github.com/kubeflow/spark-operator/pkg/util"
)

const (
	// Submission and cleanup need another attempt even when pod events are filtered.
	submissionRecheckInterval = 5 * time.Second
	// Active applications need fewer API reads, but must still recover from missed events.
	activeApplicationRecheckInterval = 5 * time.Minute
)

// reconcileSubmissionAttempt handles NEW, SUBMISSION_FAILED and PENDING_RERUN.
// It first recovers any recorded attempt, then applies retry policy and cleanup
// before starting another one. The expected state prevents a retry from acting on
// an application that another writer has already advanced or invalidated.
func (r *Reconciler) reconcileSubmissionAttempt(ctx context.Context, req ctrl.Request, expectedState v1beta2.ApplicationStateType) (ctrl.Result, error) {
	result := ctrl.Result{RequeueAfter: submissionRecheckInterval}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		app, err := r.getSparkApplicationFromAPI(ctx, req.NamespacedName)
		if err != nil {
			return client.IgnoreNotFound(err)
		}
		if app.Status.AppState.State != expectedState || !app.DeletionTimestamp.IsZero() {
			return nil
		}
		if handled, err := r.recoverSubmissionFromDriver(ctx, app); handled || err != nil {
			return err
		}
		// A previous operator may still be submitting after leadership changes.
		// Keep its identity until the bounded submit process has had time to exit.
		if app.Status.SubmissionID != "" && !app.Status.LastSubmissionAttemptTime.IsZero() &&
			app.Status.TerminationTime.IsZero() && app.Status.AppState.ErrorMessage == "" {
			if time.Since(app.Status.LastSubmissionAttemptTime.Time) < r.options.SubmissionTimeout {
				return nil
			}
			// Save the expired attempt before applying retry policy on the next pass;
			// otherwise a lost outcome could bypass Never or exhaust the budget unnoticed.
			app.Status.SubmissionAttempts++
			app.Status.AppState = v1beta2.ApplicationState{
				State:        v1beta2.ApplicationStateFailedSubmission,
				ErrorMessage: "submission timed out before driver creation",
			}
			return r.updateSparkApplicationStatus(ctx, app)
		}
		if expectedState == v1beta2.ApplicationStateFailedSubmission {
			if !util.ShouldRetry(app) {
				app.Status.AppState.State = v1beta2.ApplicationStateFailed
				app.Status.TerminationTime = metav1.Now()
				return r.updateSparkApplicationStatus(ctx, app)
			}
			delay, err := util.TimeUntilNextRetryDue(app)
			if err != nil {
				return err
			}
			if delay > 0 {
				result.RequeueAfter = delay
				return nil
			}
		}
		resources, err := r.getSubmissionResourcesFromAPI(ctx, app)
		if err != nil {
			return err
		}
		if len(resources) != 0 {
			if expectedState == v1beta2.ApplicationStateNew && r.options.SparkApplicationMetrics != nil {
				r.options.SparkApplicationMetrics.IncOrphanedResourceCleanupCount(app)
			}
			return r.deleteObservedSubmissionResources(ctx, app, resources)
		}
		if expectedState == v1beta2.ApplicationStatePendingRerun {
			r.resetSparkApplicationStatus(app)
		}
		return r.startSubmissionAttempt(ctx, app)
	})
	return result, err
}

// recoverSubmissionFromDriver uses the live driver to recover a lost submit outcome.
// A true result means this pass recovered, is waiting for, or rejected that driver;
// the caller must not start another submission. False permits normal retry/cleanup
// only when the error is nil. Retired and completed attempts are never revived.
func (r *Reconciler) recoverSubmissionFromDriver(ctx context.Context, app *v1beta2.SparkApplication) (bool, error) {
	pod, err := r.getDriverPodFromAPI(ctx, app)
	if err != nil || pod == nil {
		return false, err
	}
	if pod.Labels[common.LabelSparkAppName] != app.Name || !isResourceOwnedByApplication(app, pod) {
		return true, fmt.Errorf("driver pod %s does not belong to SparkApplication %s", pod.Name, app.Name)
	}
	if !pod.DeletionTimestamp.IsZero() {
		return true, nil
	}
	if driverHasDifferentSpecGeneration(app, pod) {
		app.Status.AppState = v1beta2.ApplicationState{State: v1beta2.ApplicationStateInvalidating}
		return true, r.updateSparkApplicationStatus(ctx, app)
	}
	if pod.Labels[common.LabelSubmissionID] != "" && pod.Labels[common.LabelSubmissionID] != app.Status.SubmissionID {
		return true, fmt.Errorf("driver pod %s has an unrecorded submission ID; refusing to adopt or delete it", pod.Name)
	}
	// Invalidation leaves the old ID but clears the attempt time. A finished run
	// retains its termination time. Neither is an interrupted new submission.
	if app.Status.SubmissionID == "" || app.Status.LastSubmissionAttemptTime.IsZero() || !app.Status.TerminationTime.IsZero() {
		return false, nil
	}
	missingConfigMapName, err := r.findMissingDriverConfigMap(ctx, pod)
	if err != nil {
		return true, err
	}
	if missingConfigMapName != "" && time.Since(app.Status.LastSubmissionAttemptTime.Time) < r.options.SubmissionTimeout {
		return true, nil
	}
	// A saved failure already counted its attempt. A new intent clears the error
	// even when it starts from SUBMISSION_FAILED, so its lost outcome still counts.
	if app.Status.AppState.State != v1beta2.ApplicationStateFailedSubmission || app.Status.AppState.ErrorMessage == "" {
		app.Status.SubmissionAttempts++
	}
	if missingConfigMapName != "" {
		app.Status.AppState = v1beta2.ApplicationState{
			State:        v1beta2.ApplicationStateFailedSubmission,
			ErrorMessage: fmt.Sprintf("submission timed out waiting for driver ConfigMap %s", missingConfigMapName),
		}
		// Persist failure before deleting its evidence. A failed status write must
		// leave the partial driver available so the next pass can count the attempt.
		if err := r.updateSparkApplicationStatus(ctx, app); err != nil {
			return true, err
		}
		return true, r.deleteObservedSubmissionResources(ctx, app, []client.Object{pod})
	}
	app.Status.ExecutionAttempts++
	app.Status.AppState.ErrorMessage = ""
	app.Status.ExecutorState = nil
	app.Status.DriverInfo.PodName = pod.Name
	if err := r.updateSparkApplicationState(ctx, app); err != nil {
		return true, err
	}
	return true, r.updateSparkApplicationStatus(ctx, app)
}

// isResourceOwnedByApplication requires evidence tied to this application's UID,
// not just its reusable name. Owner references support older resources; a UID
// annotation supports drivers created without the webhook. Conflicting stamps veto both.
func isResourceOwnedByApplication(app *v1beta2.SparkApplication, object client.Object) bool {
	if uid, stamped := object.GetAnnotations()[common.AnnotationAppUID]; stamped && uid != string(app.UID) {
		return false
	}
	if owner := metav1.GetControllerOf(object); owner != nil {
		return owner.Kind == "SparkApplication" && owner.Name == app.Name && owner.UID == app.UID
	}
	return object.GetAnnotations()[common.AnnotationAppUID] == string(app.UID) && app.UID != ""
}

// findMissingDriverConfigMap checks the generated ConfigMap needed by a Pending
// driver. It returns its name only after a live NotFound response; an empty name
// does not prove general pod readiness, and API failures remain errors.
func (r *Reconciler) findMissingDriverConfigMap(ctx context.Context, pod *corev1.Pod) (string, error) {
	if pod.Status.Phase != corev1.PodPending {
		return "", nil
	}
	// Spark 3.5 creates this ConfigMap after the driver pod. An interrupted
	// submit can leave a Pending pod that cannot start without it.
	for _, volume := range pod.Spec.Volumes {
		if volume.Name != "spark-conf-volume-driver" || volume.ConfigMap == nil {
			continue
		}
		name := volume.ConfigMap.Name
		err := r.options.APIReader.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: name}, &corev1.ConfigMap{})
		if errors.IsNotFound(err) {
			return name, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", nil
}

// driverHasDifferentSpecGeneration detects a stamped driver from a different spec.
// An unstamped legacy driver provides no generation evidence; absence alone must
// not invalidate it.
func driverHasDifferentSpecGeneration(app *v1beta2.SparkApplication, pod *corev1.Pod) bool {
	generation, stamped := pod.Annotations[common.AnnotationAppGeneration]
	return stamped && generation != strconv.FormatInt(app.Generation, 10)
}

// startSubmissionAttempt saves identity and start time before creating resources
// or running spark-submit. Once submission begins, an outcome-write failure is
// left for recovery instead of escaping to the caller's retry loop and launching twice.
func (r *Reconciler) startSubmissionAttempt(ctx context.Context, app *v1beta2.SparkApplication) error {
	app.Status.SubmissionID = uuid.New().String()
	app.Status.DriverInfo = v1beta2.DriverInfo{PodName: util.GetDriverPodName(app)}
	if r.options.EnableUIService {
		app.Status.DriverInfo.WebUIServiceName = util.GetDefaultUIServiceName(app)
		if r.options.IngressURLFormat != "" {
			app.Status.DriverInfo.WebUIIngressName = util.GetDefaultUIIngressName(app)
		}
	}
	app.Status.LastSubmissionAttemptTime = metav1.Now()
	app.Status.TerminationTime = metav1.Time{}
	app.Status.AppState.ErrorMessage = ""
	app.Status.ExecutorState = nil
	app.Status.SparkApplicationID = ""
	if err := r.updateSparkApplicationStatus(ctx, app); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	intent := app.DeepCopy()
	app.Status.SubmissionAttempts++
	if err := r.prepareAndRunSparkSubmit(app); err != nil {
		app.Status.AppState = v1beta2.ApplicationState{
			State: v1beta2.ApplicationStateFailedSubmission, ErrorMessage: err.Error(),
		}
	} else {
		app.Status.AppState = v1beta2.ApplicationState{State: v1beta2.ApplicationStateSubmitted}
		app.Status.ExecutionAttempts++
	}
	if err := r.persistSubmissionOutcome(ctx, intent, app); err != nil {
		logger.Error(err, "Failed to save the submission outcome", "name", app.Name,
			"namespace", app.Namespace, "submissionID", app.Status.SubmissionID)
	}
	// A lost outcome is recovered from the recorded intent on the next reconcile.
	// It must not retry the caller's submission side effects on a status conflict.
	return nil
}

// persistSubmissionOutcome retries the result of one launch without rerunning it.
// The saved intent identifies the application and attempt whose result may be
// written; a replacement or newer run keeps its status, while a changed spec
// retires this attempt through INVALIDATING.
func (r *Reconciler) persistSubmissionOutcome(ctx context.Context, intent, outcome *v1beta2.SparkApplication) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest, err := r.getSparkApplicationFromAPI(ctx, client.ObjectKeyFromObject(intent))
		if err != nil {
			return client.IgnoreNotFound(err)
		}
		if latest.UID != intent.UID || latest.Status.SubmissionID != intent.Status.SubmissionID ||
			!latest.DeletionTimestamp.IsZero() || latest.Status.AppState.State != intent.Status.AppState.State {
			return nil
		}
		if latest.Generation != intent.Generation {
			latest.Status.AppState = v1beta2.ApplicationState{State: v1beta2.ApplicationStateInvalidating}
		} else {
			if outcome.Status.AppState.State == v1beta2.ApplicationStateFailedSubmission {
				// spark-submit can fail after creating a usable driver. Recover it
				// before exposing a false submission failure to status consumers.
				if handled, err := r.recoverSubmissionFromDriver(ctx, latest); handled || err != nil {
					return err
				}
			}
			latest.Status = outcome.Status
		}
		if err := r.updateSparkApplicationStatus(ctx, latest); err != nil {
			return err
		}
		r.recordSparkApplicationEvent(latest)
		return nil
	})
}
