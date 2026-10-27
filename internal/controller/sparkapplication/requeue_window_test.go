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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestWithinPendingRerunRequeueWindow(t *testing.T) {
	now := time.Date(2026, 10, 27, 13, 6, 40, 0, time.UTC)
	grace := int64(30)
	deletingPod := func(deadline time.Time, grace *int64) *corev1.Pod {
		ts := metav1.NewTime(deadline)
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &ts, DeletionGracePeriodSeconds: grace}}
	}

	testCases := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{name: "no driver pod", pod: nil, want: false},
		{name: "driver pod not being deleted", pod: &corev1.Pod{}, want: false},
		{name: "deletion requested 10s ago", pod: deletingPod(now.Add(-10*time.Second), nil), want: true},
		{name: "deletion requested 61s ago", pod: deletingPod(now.Add(-61*time.Second), nil), want: false},
		// Deadline 20s in the future with a 30s grace period means the request was 10s ago.
		{name: "within grace period", pod: deletingPod(now.Add(20*time.Second), &grace), want: true},
		// Deadline 40s ago with a 30s grace period means the request was 70s ago.
		{name: "past window after grace period", pod: deletingPod(now.Add(-40*time.Second), &grace), want: false},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := withinPendingRerunRequeueWindow(tc.pod, now); got != tc.want {
				t.Errorf("withinPendingRerunRequeueWindow() = %v, want %v", got, tc.want)
			}
		})
	}
}
