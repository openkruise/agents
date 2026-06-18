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

package core

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils/expectations"
)

const CommonControlName = "common"

var (
	// ScaleExpectations tracks expected Job create actions to avoid duplicated
	// reconciles due to stale informer cache. Aligned with the sandbox controller pattern.
	ScaleExpectations = expectations.NewScaleExpectations()
)

type EnsureFuncArgs struct {
	Pod       *corev1.Pod
	Commit    *agentsv1alpha1.Commit
	NewStatus *agentsv1alpha1.CommitStatus
}

type CommitControl interface {
	EnsureCommitRunning(ctx context.Context, args *EnsureFuncArgs) (time.Duration, error)
	EnsureCommitUpdated(ctx context.Context, args *EnsureFuncArgs) (time.Duration, error)
	EnsureCommitDeleted(ctx context.Context, args *EnsureFuncArgs) (time.Duration, error)
}

// CommitControlFactory describes how to create a CommitControl.
type CommitControlFactory struct {
	Name     string
	Required bool // if true, initialization failure is fatal
	New      func(client.Client, record.EventRecorder) (CommitControl, error)
}

var commitControlFactories []CommitControlFactory

// RegisterCommitControl registers a factory for creating a CommitControl.
// Call this in init() of the control implementation file.
func RegisterCommitControl(f CommitControlFactory) {
	commitControlFactories = append(commitControlFactories, f)
}

func NewCommitControl(c client.Client, recorder record.EventRecorder) (map[string]CommitControl, error) {
	controls := map[string]CommitControl{}
	for _, f := range commitControlFactories {
		ctrl, err := f.New(c, recorder)
		if err != nil {
			if f.Required {
				klog.Errorf("required control %q initialization failed: %v", f.Name, err)
				return nil, err
			}
			klog.Warningf("optional control %q initialization skipped: %v", f.Name, err)
			continue
		}
		controls[f.Name] = ctrl
	}
	return controls, nil
}
