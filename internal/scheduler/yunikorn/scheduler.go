/*
Copyright 2024 The Kubeflow authors.

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

package yunikorn

import (
	"encoding/json"
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"

	"github.com/kubeflow/spark-operator/api/v1beta2"
	"github.com/kubeflow/spark-operator/internal/scheduler"
	"github.com/kubeflow/spark-operator/internal/scheduler/yunikorn/resourceusage"
	"github.com/kubeflow/spark-operator/pkg/util"
)

const (
	SchedulerName = "yunikorn"

	// The names are set to match the Yunikorn gang scheduling example for Spark, but these can be any
	// value as long as what's on the pod matches the task group definition
	// https://yunikorn.apache.org/docs/next/user_guide/gang_scheduling/#enable-gang-scheduling-for-spark-jobs
	driverTaskGroupName   = "spark-driver"
	executorTaskGroupName = "spark-executor"

	// https://yunikorn.apache.org/docs/next/user_guide/labels_and_annotations_in_yunikorn/
	taskGroupNameAnnotation = "yunikorn.apache.org/task-group-name"
	taskGroupsAnnotation    = "yunikorn.apache.org/task-groups"
	queueLabel              = "queue"
	appIDLabel              = "yunikorn.apache.org/app-id"

	// The driver and executors are submitted to separate queues, and one Yunikorn application
	// cannot span two queues, so they must keep distinct application IDs.
	driverAppIDRole   = "driver"
	executorAppIDRole = "executors"

	// Kubernetes caps label values at 63 characters.
	maxLabelValueLength = 63

	// Leading characters of the submission UUID kept in the application ID. Enough to
	// distinguish consecutive submissions of the same application, which is all that matters.
	appIDSubmissionIDLength = 8
)

// This struct has been defined separately rather than imported so that tags can be included for JSON marshalling
// https://github.com/apache/yunikorn-k8shim/blob/207e4031c6484c965fca4018b6b8176afc5956b4/pkg/cache/amprotocol.go#L47-L56
type taskGroup struct {
	Name         string              `json:"name"`
	MinMember    int32               `json:"minMember"`
	MinResource  map[string]string   `json:"minResource,omitempty"`
	NodeSelector map[string]string   `json:"nodeSelector,omitempty"`
	Tolerations  []corev1.Toleration `json:"tolerations,omitempty"`
	Affinity     *corev1.Affinity    `json:"affinity,omitempty"`
	Labels       map[string]string   `json:"labels,omitempty"`
}

type Scheduler struct{}

func Factory(_ scheduler.Config) (scheduler.Interface, error) {
	return &Scheduler{}, nil
}

func (s *Scheduler) Name() string {
	return SchedulerName
}

func (s *Scheduler) ShouldSchedule(_ *v1beta2.SparkApplication) bool {
	// Yunikorn gets all the information it needs from pod annotations on the originating pod,
	// so no additional resources need to be created
	return true
}

func (s *Scheduler) Schedule(app *v1beta2.SparkApplication) error {
	driverMinResources, err := resourceusage.DriverPodRequests(app)
	if err != nil {
		return fmt.Errorf("failed to calculate driver minResources: %w", err)
	}

	taskGroups := []taskGroup{
		{
			Name:         driverTaskGroupName,
			MinMember:    1,
			MinResource:  driverMinResources,
			NodeSelector: mergeNodeSelector(app.Spec.NodeSelector, app.Spec.Driver.NodeSelector),
			Tolerations:  app.Spec.Driver.Tolerations,
			Affinity:     app.Spec.Driver.Affinity,
			Labels:       app.Spec.Driver.Labels,
		},
	}

	// A minMember of zero is not a valid config for a Yunikorn task group, so we should leave out
	// the executor task group completely if the initial number of executors is zero
	if numInitialExecutors := util.GetInitialExecutorNumber(app); numInitialExecutors > 0 {
		executorMinResources, err := resourceusage.ExecutorPodRequests(app)
		if err != nil {
			return fmt.Errorf("failed to calculate executor minResources: %w", err)
		}

		taskGroups = append(taskGroups, taskGroup{
			Name:         executorTaskGroupName,
			MinMember:    numInitialExecutors,
			MinResource:  executorMinResources,
			NodeSelector: mergeNodeSelector(app.Spec.NodeSelector, app.Spec.Executor.NodeSelector),
			Tolerations:  app.Spec.Executor.Tolerations,
			Affinity:     app.Spec.Executor.Affinity,
			Labels:       app.Spec.Executor.Labels,
		})
	}

	// Ensure that the driver and executors pods are scheduled by Yunikorn
	// if it is installed with the admissions controller disabled
	app.Spec.Driver.SchedulerName = util.StringPtr(SchedulerName)
	app.Spec.Executor.SchedulerName = util.StringPtr(SchedulerName)

	// Yunikorn would otherwise re-use the application ID the driver sets under the label
	// "spark-app-selector", but an explicit app-id label outranks it, and callers render a
	// static one into .spec.sparkConf — so set it per submission here instead. [ENG-45235]
	// https://github.com/apache/yunikorn-k8shim/blob/2278b3217c702ccb796e4d623bc7837625e5a4ec/pkg/common/utils/utils.go#L168-L171
	addAppIDLabels(app)
	addQueueLabels(app)
	if err := addTaskGroupAnnotations(app, taskGroups); err != nil {
		return fmt.Errorf("failed to add task group annotations: %w", err)
	}

	return nil
}

func (s *Scheduler) Cleanup(_ *v1beta2.SparkApplication) error {
	// No additional resources are created so there's nothing to be cleaned up
	return nil
}

func addTaskGroupAnnotations(app *v1beta2.SparkApplication, taskGroups []taskGroup) error {
	marshalledTaskGroups, err := json.Marshal(taskGroups)
	if err != nil {
		return fmt.Errorf("failed to marshal taskGroups: %w", err)
	}

	if app.Spec.Driver.Annotations == nil {
		app.Spec.Driver.Annotations = make(map[string]string)
	}
	if app.Spec.Executor.Annotations == nil {
		app.Spec.Executor.Annotations = make(map[string]string)
	}

	app.Spec.Driver.Annotations[taskGroupNameAnnotation] = driverTaskGroupName
	app.Spec.Executor.Annotations[taskGroupNameAnnotation] = executorTaskGroupName

	// The task group definition only needs to be present on the originating pod
	// https://yunikorn.apache.org/docs/next/user_guide/gang_scheduling/#app-configuration
	app.Spec.Driver.Annotations[taskGroupsAnnotation] = string(marshalledTaskGroups)

	return nil
}

// addAppIDLabels gives each submission its own Yunikorn application ID. Reusing the previous
// submission's ID makes a resubmitted driver attach to that application while it is still
// Completing, and Yunikorn then orphans the task when the application completes. [ENG-45235]
func addAppIDLabels(app *v1beta2.SparkApplication) {
	if app.Status.SubmissionID == "" {
		return
	}
	submissionID := app.Status.SubmissionID
	if len(submissionID) > appIDSubmissionIDLength {
		submissionID = submissionID[:appIDSubmissionIDLength]
	}

	if app.Spec.Driver.Labels == nil {
		app.Spec.Driver.Labels = make(map[string]string)
	}
	if app.Spec.Executor.Labels == nil {
		app.Spec.Executor.Labels = make(map[string]string)
	}

	app.Spec.Driver.Labels[appIDLabel] = appID(app.Name, driverAppIDRole, submissionID)
	app.Spec.Executor.Labels[appIDLabel] = appID(app.Name, executorAppIDRole, submissionID)
}

func appID(name, role, submissionID string) string {
	suffix := fmt.Sprintf("-%s-%s", role, submissionID)
	if len(name)+len(suffix) > maxLabelValueLength {
		name = name[:maxLabelValueLength-len(suffix)]
	}
	return name + suffix
}

func addQueueLabels(app *v1beta2.SparkApplication) {
	if app.Spec.BatchSchedulerOptions != nil && app.Spec.BatchSchedulerOptions.Queue != nil {
		if app.Spec.Driver.Labels == nil {
			app.Spec.Driver.Labels = make(map[string]string)
		}
		if app.Spec.Executor.Labels == nil {
			app.Spec.Executor.Labels = make(map[string]string)
		}

		app.Spec.Driver.Labels[queueLabel] = *app.Spec.BatchSchedulerOptions.Queue
		app.Spec.Executor.Labels[queueLabel] = *app.Spec.BatchSchedulerOptions.Queue
	}
}

func mergeNodeSelector(appNodeSelector map[string]string, podNodeSelector map[string]string) map[string]string {
	// app.Spec.NodeSelector is passed "spark.kubernetes.node.selector.%s", which means it will be present
	// in the pod definition before the mutating webhook. The mutating webhook merges the driver/executor-specific
	// NodeSelector with what's already present
	nodeSelector := make(map[string]string)
	maps.Copy(nodeSelector, appNodeSelector)
	maps.Copy(nodeSelector, podNodeSelector)

	// Return nil if there are no entries in the map so that the field is skipped during JSON marshalling
	if len(nodeSelector) == 0 {
		return nil
	}
	return nodeSelector
}
