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

var _ = Describe("submission handoff before driver creation", Serial, func() {
	DescribeTable("waits for the recorded submission and recovers its arriving driver", func(state v1beta2.ApplicationStateType) {
		app := newRecoveryApplication(state)
		app.Status.SubmissionID = "in-flight-run"
		app.Status.DriverInfo.PodName = util.GetDriverPodName(app)
		submit := newControlledSubmit(false)
		request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(app)}
		for _, age := range []time.Duration{0, 9 * time.Minute} {
			app.Status.LastSubmissionAttemptTime = metav1.NewTime(time.Now().Add(-age))
			Expect(k8sClient.Status().Update(context.Background(), app)).To(Succeed())
			intent := getRecoveryApplication(app)

			result, err := newRecoveryReconciler(k8sClient).Reconcile(context.Background(), request)

			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))
			Expect(getRecoveryApplication(app).Status).To(Equal(intent.Status))
			Expect(submit.invocations()).To(BeZero())
		}
		pod := createDriverPod(app.Name, app.Namespace)
		pod.Labels[common.LabelSubmissionID] = app.Status.SubmissionID
		pod.Annotations = map[string]string{
			common.AnnotationAppUID:        string(app.UID),
			common.AnnotationAppGeneration: strconv.FormatInt(app.Generation, 10),
		}
		persistRecoveryDriver(app, pod)

		_, err := newRecoveryReconciler(k8sClient).Reconcile(context.Background(), request)

		Expect(err).NotTo(HaveOccurred())
		latest := getRecoveryApplication(app)
		Expect(latest.Status.AppState.State).To(Equal(v1beta2.ApplicationStateRunning))
		Expect(latest.Status.SubmissionID).To(Equal("in-flight-run"))
		Expect(submit.invocations()).To(BeZero())
		current := &corev1.Pod{}
		Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(pod), current)).To(Succeed())
		Expect(current.UID).To(Equal(pod.UID))
		Expect(current.DeletionTimestamp.IsZero()).To(BeTrue())
	},
		Entry("NEW", v1beta2.ApplicationStateNew),
		Entry("SUBMISSION_FAILED", v1beta2.ApplicationStateFailedSubmission),
		Entry("PENDING_RERUN", v1beta2.ApplicationStatePendingRerun),
	)

	DescribeTable("permits a fresh submission after the wait is resolved", func(state v1beta2.ApplicationStateType, age time.Duration, savedError string) {
		app := newRecoveryApplication(state)
		app.Status.SubmissionID = "previous-run"
		app.Status.DriverInfo.PodName = util.GetDriverPodName(app)
		app.Status.LastSubmissionAttemptTime = metav1.NewTime(time.Now().Add(-age))
		app.Status.AppState.ErrorMessage = savedError
		Expect(k8sClient.Status().Update(context.Background(), app)).To(Succeed())
		submit := newControlledSubmit(false)

		_, err := newRecoveryReconciler(k8sClient).Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(app)})

		Expect(err).NotTo(HaveOccurred())
		Expect(submit.invocations()).To(Equal(1))
		latest := getRecoveryApplication(app)
		Expect(latest.Status.AppState.State).To(Equal(v1beta2.ApplicationStateSubmitted))
		Expect(latest.Status.SubmissionID).NotTo(Equal("previous-run"))
		Expect(latest.Status.SubmissionID).To(Equal(submit.submissionID()))
		Expect(latest.Status.AppState.ErrorMessage).To(BeEmpty())
		Expect(latest.Status.LastSubmissionAttemptTime.After(app.Status.LastSubmissionAttemptTime.Time)).To(BeTrue())
	},
		Entry("NEW intent exceeded the default ten-minute timeout", v1beta2.ApplicationStateNew, 11*time.Minute, ""),
		Entry("SUBMISSION_FAILED intent exceeded the default ten-minute timeout", v1beta2.ApplicationStateFailedSubmission, 11*time.Minute, ""),
		Entry("PENDING_RERUN intent exceeded the default ten-minute timeout", v1beta2.ApplicationStatePendingRerun, 11*time.Minute, ""),
		Entry("a saved failure is already due for retry", v1beta2.ApplicationStateFailedSubmission, time.Minute, "spark-submit failed before driver creation"),
	)
})
