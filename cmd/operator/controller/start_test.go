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

package controller

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestSubmissionTimeoutConfiguration(t *testing.T) {
	for _, tt := range []struct {
		name    string
		args    []string
		want    time.Duration
		wantErr bool
	}{
		{name: "default", want: 10 * time.Minute},
		{name: "custom", args: []string{"--submission-timeout=25m"}, want: 25 * time.Minute},
		{name: "zero", args: []string{"--submission-timeout=0"}, wantErr: true},
		{name: "negative", args: []string{"--submission-timeout=-1m"}, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			command := NewStartCommand()
			command.SetOut(io.Discard)
			command.SetErr(io.Discard)
			command.SetArgs(tt.args)
			var configuredTimeout time.Duration
			started := false
			// Exercise Cobra parsing and validation without starting a Kubernetes manager.
			command.Run = func(_ *cobra.Command, _ []string) {
				started = true
				configuredTimeout = newSparkApplicationReconcilerOptions(nil).SubmissionTimeout
			}

			err := command.Execute()

			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "submission-timeout must be positive") {
					t.Fatalf("expected a positive-timeout validation error, got %v", err)
				}
				if started {
					t.Fatal("controller started with an invalid submission timeout")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !started || configuredTimeout != tt.want {
				t.Fatalf("controller started = %t with timeout %s, want %s", started, configuredTimeout, tt.want)
			}
		})
	}
}
