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

package sparkapplication_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/kubeflow/spark-operator/api/v1beta2"
	"github.com/kubeflow/spark-operator/internal/controller/sparkapplication"
	"github.com/kubeflow/spark-operator/pkg/common"
	"github.com/kubeflow/spark-operator/pkg/util"
)

var _ = Describe("SparkPodEventHandler", func() {
	Context("When a pod's submission ID does not match its SparkApplication", func() {
		ctx := context.Background()
		appName := "test-event-handler-stale-pod"
		appNamespace := "default"
		key := types.NamespacedName{Name: appName, Namespace: appNamespace}

		var queue workqueue.RateLimitingInterface
		var handler *sparkapplication.SparkPodEventHandler

		BeforeEach(func() {
			app := &v1beta2.SparkApplication{
				ObjectMeta: metav1.ObjectMeta{Name: appName, Namespace: appNamespace},
				Spec: v1beta2.SparkApplicationSpec{
					MainApplicationFile: util.StringPtr("local:///dummy.jar"),
				},
			}
			v1beta2.SetSparkApplicationDefaults(app)
			Expect(k8sClient.Create(ctx, app)).To(Succeed())
			app.Status.AppState.State = v1beta2.ApplicationStatePendingRerun
			app.Status.SubmissionID = "current-submission-id"
			Expect(k8sClient.Status().Update(ctx, app)).To(Succeed())

			queue = workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
			handler = sparkapplication.NewSparkPodEventHandler(k8sClient, nil)
		})

		AfterEach(func() {
			queue.ShutDown()
			app := &v1beta2.SparkApplication{}
			Expect(k8sClient.Get(ctx, key, app)).To(Succeed())
			Expect(k8sClient.Delete(ctx, app)).To(Succeed())
		})

		stalePod := func(role string) *corev1.Pod {
			pod := createDriverPod(appName, appNamespace)
			pod.Labels[common.LabelSparkRole] = role
			pod.Labels[common.LabelSubmissionID] = "old-submission-id"
			return pod
		}

		It("Should enqueue the app when the driver pod is deleted", func() {
			handler.Delete(ctx, event.DeleteEvent{Object: stalePod(common.SparkRoleDriver)}, queue)
			Eventually(queue.Len).Should(Equal(1))
		})

		It("Should not enqueue the app when an executor pod is deleted", func() {
			handler.Delete(ctx, event.DeleteEvent{Object: stalePod(common.SparkRoleExecutor)}, queue)
			Consistently(queue.Len, 200*time.Millisecond).Should(Equal(0))
		})

		It("Should not enqueue the app when the driver pod is updated", func() {
			oldPod := stalePod(common.SparkRoleDriver)
			newPod := oldPod.DeepCopy()
			newPod.Status.Phase = corev1.PodFailed
			handler.Update(ctx, event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPod}, queue)
			Consistently(queue.Len, 200*time.Millisecond).Should(Equal(0))
		})
	})
})
