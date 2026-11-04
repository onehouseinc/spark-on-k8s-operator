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
		r := newRecoveryReconciler(k8sClient)
		request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(app)}
		attempts := app.Status.SubmissionAttempts
		if savedError == "" {
			_, err := r.Reconcile(context.Background(), request)
			Expect(err).NotTo(HaveOccurred())
			latest := getRecoveryApplication(app)
			Expect(latest.Status.AppState.State).To(Equal(v1beta2.ApplicationStateFailedSubmission))
			Expect(latest.Status.AppState.ErrorMessage).To(ContainSubstring("timed out"))
			Expect(latest.Status.SubmissionID).To(Equal("previous-run"))
			attempts++
			Expect(latest.Status.SubmissionAttempts).To(Equal(attempts))
			Expect(submit.invocations()).To(BeZero())
		}

		_, err := r.Reconcile(context.Background(), request)

		Expect(err).NotTo(HaveOccurred())
		Expect(submit.invocations()).To(Equal(1))
		latest := getRecoveryApplication(app)
		Expect(latest.Status.AppState.State).To(Equal(v1beta2.ApplicationStateSubmitted))
		Expect(latest.Status.SubmissionID).NotTo(Equal("previous-run"))
		Expect(latest.Status.SubmissionID).To(Equal(submit.submissionID()))
		Expect(latest.Status.AppState.ErrorMessage).To(BeEmpty())
		Expect(latest.Status.LastSubmissionAttemptTime.After(app.Status.LastSubmissionAttemptTime.Time)).To(BeTrue())
		Expect(latest.Status.SubmissionAttempts).To(Equal(attempts + 1))
	},
		Entry("NEW intent exceeded the default ten-minute timeout", v1beta2.ApplicationStateNew, 11*time.Minute, ""),
		Entry("SUBMISSION_FAILED intent exceeded the default ten-minute timeout", v1beta2.ApplicationStateFailedSubmission, 11*time.Minute, ""),
		Entry("PENDING_RERUN intent exceeded the default ten-minute timeout", v1beta2.ApplicationStatePendingRerun, 11*time.Minute, ""),
		Entry("a saved failure is already due for retry", v1beta2.ApplicationStateFailedSubmission, time.Minute, "spark-submit failed before driver creation"),
	)

	DescribeTable("honors the restart policy after a driverless intent expires", func(state v1beta2.ApplicationStateType, policy v1beta2.RestartPolicyType, attempts int32) {
		app := newRecoveryApplication(state)
		app.Spec.RestartPolicy.Type = policy
		app.Spec.RestartPolicy.OnSubmissionFailureRetries = util.Int32Ptr(1)
		Expect(k8sClient.Update(context.Background(), app)).To(Succeed())
		app.Status.SubmissionID = "lost-failed-outcome"
		app.Status.LastSubmissionAttemptTime = metav1.NewTime(time.Now().Add(-11 * time.Minute))
		app.Status.SubmissionAttempts = attempts
		Expect(k8sClient.Status().Update(context.Background(), app)).To(Succeed())
		submit := newControlledSubmit(false)
		r := newRecoveryReconciler(k8sClient)
		request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(app)}

		_, err := r.Reconcile(context.Background(), request)
		Expect(err).NotTo(HaveOccurred())
		latest := getRecoveryApplication(app)
		Expect(latest.Status.AppState.State).To(Equal(v1beta2.ApplicationStateFailedSubmission))
		Expect(latest.Status.SubmissionAttempts).To(Equal(attempts + 1))
		Expect(latest.Status.SubmissionID).To(Equal("lost-failed-outcome"))
		Expect(submit.invocations()).To(BeZero())

		for range 2 {
			_, err = r.Reconcile(context.Background(), request)
			Expect(err).NotTo(HaveOccurred())
			latest = getRecoveryApplication(app)
			Expect(latest.Status.AppState.State).To(Equal(v1beta2.ApplicationStateFailed))
			Expect(latest.Status.SubmissionAttempts).To(Equal(attempts + 1))
			Expect(latest.Status.SubmissionID).To(Equal("lost-failed-outcome"))
			Expect(submit.invocations()).To(BeZero())
		}
	},
		Entry("Never does not launch a second attempt", v1beta2.ApplicationStateNew, v1beta2.RestartPolicyNever, int32(0)),
		Entry("OnFailure stops after its final permitted attempt", v1beta2.ApplicationStateFailedSubmission, v1beta2.RestartPolicyOnFailure, int32(1)),
	)

	It("counts an expired driverless intent only once while waiting for its retry", func() {
		app := newRecoveryApplication(v1beta2.ApplicationStateNew)
		app.Spec.RestartPolicy.OnSubmissionFailureRetryInterval = util.Int64Ptr(3600)
		Expect(k8sClient.Update(context.Background(), app)).To(Succeed())
		app.Status.SubmissionID = "expired-intent"
		app.Status.LastSubmissionAttemptTime = metav1.NewTime(time.Now().Add(-11 * time.Minute))
		Expect(k8sClient.Status().Update(context.Background(), app)).To(Succeed())
		submit := newControlledSubmit(false)
		r := newRecoveryReconciler(k8sClient)
		request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(app)}

		for range 3 {
			result, err := r.Reconcile(context.Background(), request)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))
			latest := getRecoveryApplication(app)
			Expect(latest.Status.AppState.State).To(Equal(v1beta2.ApplicationStateFailedSubmission))
			Expect(latest.Status.SubmissionAttempts).To(Equal(int32(1)))
			Expect(latest.Status.SubmissionID).To(Equal("expired-intent"))
			Expect(latest.Status.LastSubmissionAttemptTime).To(Equal(app.Status.LastSubmissionAttemptTime))
			Expect(submit.invocations()).To(BeZero())
		}
	})
})
