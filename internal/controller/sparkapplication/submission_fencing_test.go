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
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/kubeflow/spark-operator/api/v1beta2"
	"github.com/kubeflow/spark-operator/pkg/common"
	"github.com/kubeflow/spark-operator/pkg/util"
)

var _ = Describe("submission outcome fencing", Serial, func() {
	DescribeTable("preserves a replacement while the old submission finishes", func(recreate bool) {
		app := newRecoveryApplication(v1beta2.ApplicationStateNew)
		submit := newControlledSubmit(true)
		done := startRecoveryReconcile(newRecoveryReconciler(k8sClient), app, submit)
		submit.waitForInvocation()
		replacement := getRecoveryApplication(app)
		if recreate {
			intent := replacement.Status
			Expect(k8sClient.Delete(context.Background(), replacement)).To(Succeed())
			replacement = &v1beta2.SparkApplication{
				ObjectMeta: metav1.ObjectMeta{Name: app.Name, Namespace: app.Namespace},
				Spec:       app.Spec,
			}
			Expect(k8sClient.Create(context.Background(), replacement)).To(Succeed())
			Expect(replacement.UID).NotTo(Equal(app.UID))
			// Retaining the recorded ID makes the application UID the distinguishing fence.
			replacement.Status = intent
		} else {
			replacement.Status.SubmissionID = "replacement-run"
		}
		replacement.Status.ExecutionAttempts = 42
		Expect(k8sClient.Status().Update(context.Background(), replacement)).To(Succeed())
		expected := getRecoveryApplication(replacement)
		pod := createDriverPod(replacement.Name, replacement.Namespace)
		pod.Labels[common.LabelSubmissionID] = replacement.Status.SubmissionID
		persistRecoveryDriver(replacement, pod)

		submit.release()
		var outcome recoveryResult
		Eventually(done, 10*time.Second).Should(Receive(&outcome))
		Expect(outcome.err).NotTo(HaveOccurred())

		latest := getRecoveryApplication(replacement)
		Expect(latest.UID).To(Equal(expected.UID))
		Expect(latest.Status).To(Equal(expected.Status))
		Expect(latest.DeletionTimestamp.IsZero()).To(BeTrue())
		current := &corev1.Pod{}
		Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(pod), current)).To(Succeed())
		Expect(current.UID).To(Equal(pod.UID))
		Expect(current.DeletionTimestamp.IsZero()).To(BeTrue())
		Expect(submit.invocations()).To(Equal(1))
	},
		Entry("after the application is deleted and recreated", true),
		Entry("after the recorded submission ID changes", false),
	)

	DescribeTable("recovers a terminal driver without launching another run", func(phase corev1.PodPhase, state v1beta2.ApplicationStateType, exitCode int32) {
		app := newRecoveryApplication(v1beta2.ApplicationStateNew)
		app.Status.SubmissionID = "recorded-run"
		app.Status.DriverInfo.PodName = util.GetDriverPodName(app)
		app.Status.LastSubmissionAttemptTime = metav1.Now()
		Expect(k8sClient.Status().Update(context.Background(), app)).To(Succeed())
		pod := createDriverPod(app.Name, app.Namespace)
		pod.Labels[common.LabelSubmissionID] = app.Status.SubmissionID
		pod.Annotations = map[string]string{
			common.AnnotationAppUID:        string(app.UID),
			common.AnnotationAppGeneration: strconv.FormatInt(app.Generation, 10),
		}
		persistRecoveryDriver(app, pod)
		pod.Status.Phase = phase
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: common.SparkDriverContainerName,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: exitCode, Reason: "finished-before-recovery",
			}},
		}}
		Expect(k8sClient.Status().Update(context.Background(), pod)).To(Succeed())
		submit := newControlledSubmit(false)

		_, err := newRecoveryReconciler(k8sClient).Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(app)})

		Expect(err).NotTo(HaveOccurred())
		latest := getRecoveryApplication(app)
		Expect(latest.Status.SubmissionID).To(Equal(app.Status.SubmissionID))
		Expect(latest.Status.AppState.State).To(Equal(state))
		Expect(latest.Status.TerminationTime.IsZero()).To(BeFalse())
		Expect(latest.Status.SubmissionAttempts).To(Equal(int32(1)))
		Expect(latest.Status.ExecutionAttempts).To(Equal(int32(1)))
		if exitCode != 0 {
			Expect(latest.Status.AppState.ErrorMessage).To(ContainSubstring("ExitCode: 42"))
		}
		current := &corev1.Pod{}
		Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(pod), current)).To(Succeed())
		Expect(current.UID).To(Equal(pod.UID))
		Expect(current.Status.Phase).To(Equal(phase))
		Expect(current.DeletionTimestamp.IsZero()).To(BeTrue())
		Expect(submit.invocations()).To(BeZero())
	},
		Entry("Succeeded", corev1.PodSucceeded, v1beta2.ApplicationStateSucceeding, int32(0)),
		Entry("Failed", corev1.PodFailed, v1beta2.ApplicationStateFailing, int32(42)),
	)
})
