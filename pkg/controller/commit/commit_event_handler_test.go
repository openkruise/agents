/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package commit

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func newCommitJob(name, namespace, commitName string, complete, failed bool) *batchv1.Job {
	trueVal := true
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: agentsv1alpha1.SchemeGroupVersion.String(),
					Kind:       "Commit",
					Name:       commitName,
					Controller: &trueVal,
				},
			},
		},
	}
	if complete {
		job.Status.Conditions = append(job.Status.Conditions, batchv1.JobCondition{
			Type:   batchv1.JobComplete,
			Status: corev1.ConditionTrue,
		})
	}
	if failed {
		job.Status.Conditions = append(job.Status.Conditions, batchv1.JobCondition{
			Type:   batchv1.JobFailed,
			Status: corev1.ConditionTrue,
		})
	}
	return job
}

func TestAddEvent(t *testing.T) {
	tests := []struct {
		name          string
		job           *batchv1.Job
		expectEnqueue bool
		expectKey     types.NamespacedName
	}{
		{
			name:          "completed job enqueues commit",
			job:           newCommitJob("job-1", "default", "my-commit", true, false),
			expectEnqueue: true,
			expectKey:     types.NamespacedName{Namespace: "default", Name: "my-commit"},
		},
		{
			name:          "failed job enqueues commit",
			job:           newCommitJob("job-2", "default", "my-commit", false, true),
			expectEnqueue: true,
			expectKey:     types.NamespacedName{Namespace: "default", Name: "my-commit"},
		},
		{
			name:          "running job does not enqueue",
			job:           newCommitJob("job-3", "default", "my-commit", false, false),
			expectEnqueue: false,
		},
		{
			name: "job without commit owner does not enqueue",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "job-4", Namespace: "default"},
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
					},
				},
			},
			expectEnqueue: false,
		},
		{
			name:          "non-Job object does not enqueue",
			job:           nil,
			expectEnqueue: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
			defer q.ShutDown()

			h := &enqueueRequestForJob{}
			if tt.job == nil {
				// Pass a non-Job object
				h.addEvent(q, &corev1.Pod{})
			} else {
				h.addEvent(q, tt.job)
			}

			if tt.expectEnqueue {
				if q.Len() != 1 {
					t.Fatalf("expected 1 enqueued item, got %d", q.Len())
				}
				item, _ := q.Get()
				if item.NamespacedName != tt.expectKey {
					t.Errorf("expected key %v, got %v", tt.expectKey, item.NamespacedName)
				}
				q.Done(item)
			} else {
				if q.Len() != 0 {
					t.Errorf("expected 0 enqueued items, got %d", q.Len())
				}
			}
		})
	}
}

func TestCommitOwnerName(t *testing.T) {
	tests := []struct {
		name       string
		job        *batchv1.Job
		expectName string
		expectOK   bool
	}{
		{
			name:       "valid commit owner",
			job:        newCommitJob("job-1", "default", "my-commit", false, false),
			expectName: "my-commit",
			expectOK:   true,
		},
		{
			name: "no owner references",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "job-2", Namespace: "default"},
			},
			expectName: "",
			expectOK:   false,
		},
		{
			name: "wrong kind in owner",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "job-3",
					Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{
						{Kind: "Sandbox", Name: "sbx-1", Controller: func() *bool { b := true; return &b }()},
					},
				},
			},
			expectName: "",
			expectOK:   false,
		},
		{
			name: "non-controller owner",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "job-4",
					Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: agentsv1alpha1.SchemeGroupVersion.String(),
							Kind:       "Commit",
							Name:       "my-commit",
							// Controller not set
						},
					},
				},
			},
			expectName: "",
			expectOK:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, ok := commitOwnerName(tt.job)
			if ok != tt.expectOK {
				t.Errorf("expected ok=%v, got %v", tt.expectOK, ok)
			}
			if name != tt.expectName {
				t.Errorf("expected name=%q, got %q", tt.expectName, name)
			}
		})
	}
}
