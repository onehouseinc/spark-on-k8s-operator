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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubeflow/spark-operator/pkg/common"
)

func TestSubmissionProcessTimeout(t *testing.T) {
	installSparkSubmit(t, "exec sleep 30\n")
	start := time.Now()
	err := runSparkSubmit(&sparkSubmitCommand{attemptStartTime: start}, 50*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected submission deadline, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("submission took %s despite its 50ms deadline", elapsed)
	}
}

func TestSubmissionProcessResults(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		want string
	}{
		{name: "success", body: "exit 0\n"},
		{name: "failure", body: "echo 'invalid Spark configuration' >&2\nexit 1\n", want: "invalid Spark configuration"},
		{name: "existing driver", body: "echo 'code=409' >&2\nexit 1\n", want: "driver pod already exist"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			installSparkSubmit(t, tt.body)
			err := runSparkSubmit(&sparkSubmitCommand{attemptStartTime: time.Now()}, 5*time.Second)
			if tt.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want error containing %q", err, tt.want)
			}
		})
	}
}

func TestSubmissionProcessArguments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "arguments")
	t.Setenv("SUBMISSION_TEST_ARGUMENTS", path)
	installSparkSubmit(t, "printf '%s\\n' \"$@\" > \"$SUBMISSION_TEST_ARGUMENTS\"\n")
	args := []string{"--conf", "spark.test=value with spaces", "local:///app.jar"}
	if err := runSparkSubmit(&sparkSubmitCommand{args: args, attemptStartTime: time.Now()}, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Join(args, "\n") + "\n"; string(got) != want {
		t.Fatalf("arguments = %q, want %q", got, want)
	}
}

// installSparkSubmit points SPARK_HOME at a temporary executable so tests exercise
// real subprocess deadlines, argument passing and stderr without launching Spark.
func installSparkSubmit(t *testing.T, body string) {
	t.Helper()
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	if err := os.Mkdir(bin, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "spark-submit"), []byte("#!/bin/sh\n"+body), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(common.EnvSparkHome, home)
}
