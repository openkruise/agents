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

package validating

import (
	"context"
	"net/http"

	"github.com/distribution/reference"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/discovery"
	"github.com/openkruise/agents/pkg/features"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
)

var controllerKind = agentsv1alpha1.SchemeGroupVersion.WithKind("Commit")

// CommitValidatingHandler validates Commit resources on create.
type CommitValidatingHandler struct {
	Client  client.Client
	Decoder admission.Decoder
}

// +kubebuilder:webhook:path=/validate-commit,mutating=false,failurePolicy=fail,sideEffects=None,admissionReviewVersions=v1;v1beta1,groups=agents.kruise.io,resources=commits,verbs=create,versions=v1alpha1,name=v-cmt.kb.io

func (h *CommitValidatingHandler) Path() string {
	return "/validate-commit"
}

func (h *CommitValidatingHandler) Enabled() bool {
	return utilfeature.DefaultFeatureGate.Enabled(features.CommitGate) && discovery.DiscoverGVK(controllerKind)
}

func (h *CommitValidatingHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	obj := &agentsv1alpha1.Commit{}
	if err := h.Decoder.Decode(req, obj); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	var errList field.ErrorList
	errList = append(errList, h.validateCommit(ctx, obj, field.NewPath("spec"))...)
	if len(errList) > 0 {
		return admission.Errored(http.StatusUnprocessableEntity, errList.ToAggregate())
	}
	return admission.Allowed("")
}

func (h *CommitValidatingHandler) validateCommit(ctx context.Context, commit *agentsv1alpha1.Commit, fldPath *field.Path) field.ErrorList {
	var errList field.ErrorList

	// Validate image format
	named, err := reference.ParseNormalizedNamed(commit.Spec.Image)
	if err != nil {
		errList = append(errList, field.Invalid(fldPath.Child("image"), commit.Spec.Image, "invalid image reference"))
	} else if _, ok := named.(reference.Tagged); !ok {
		errList = append(errList, field.Invalid(fldPath.Child("image"), commit.Spec.Image, "image must be tagged"))
	}

	// Validate target pod exists and is running
	pod := &corev1.Pod{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: commit.Namespace, Name: commit.Spec.PodName}, pod); err != nil {
		errList = append(errList, field.Invalid(fldPath.Child("podName"), commit.Spec.PodName, "pod not found"))
		return errList
	}

	if pod.DeletionTimestamp != nil {
		errList = append(errList, field.Invalid(fldPath.Child("podName"), commit.Spec.PodName, "pod is being deleted"))
	}

	if pod.Status.Phase != corev1.PodRunning {
		errList = append(errList, field.Invalid(fldPath.Child("podName"), commit.Spec.PodName, "pod is not running"))
	}

	// Validate container exists
	found := false
	for _, container := range pod.Spec.Containers {
		if container.Name == commit.Spec.ContainerName {
			found = true
			break
		}
	}
	if !found {
		errList = append(errList, field.Invalid(fldPath.Child("containerName"), commit.Spec.ContainerName, "container not found in pod"))
	}

	// Validate TTL
	if commit.Spec.Ttl != nil && commit.Spec.Ttl.Duration < 0 {
		errList = append(errList, field.Invalid(fldPath.Child("ttl"), commit.Spec.Ttl, "ttl cannot be negative"))
	}

	return errList
}
