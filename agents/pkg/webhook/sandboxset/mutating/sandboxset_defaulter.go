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

package mutating

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/discovery"
	"github.com/openkruise/agents/pkg/features"
	"github.com/openkruise/agents/pkg/utils/defaults"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
)

var (
	controllerKind = agentsv1alpha1.SchemeGroupVersion.WithKind("SandboxSet")

	defaultPersistentContents []string
)

func SetDefaultPersistentContents(contents string) error {
	if contents == "" {
		return nil
	}
	persistentContents := strings.Split(contents, ",")
	for _, content := range persistentContents {
		if content != agentsv1alpha1.PersistentContentIp &&
			content != agentsv1alpha1.PersistentContentFilesystem &&
			content != agentsv1alpha1.PersistentContentMemory {
			return fmt.Errorf("default-sandboxset-persistent-contents is invalid and only supports three contents: ip, memory, and filesystem")
		}
	}
	defaultPersistentContents = persistentContents
	return nil
}

type SandboxSetDefaulter struct {
	Client  client.Client
	Decoder admission.Decoder
}

// +kubebuilder:webhook:path=/default-sandboxset,mutating=true,failurePolicy=fail,sideEffects=None,admissionReviewVersions=v1;v1beta1,groups=agents.kruise.io,resources=sandboxsets,verbs=create;update,versions=v1alpha1,name=md-sbs.kb.io

func (h *SandboxSetDefaulter) Path() string {
	return "/default-sandboxset"
}

func (h *SandboxSetDefaulter) Enabled() bool {
	if !utilfeature.DefaultFeatureGate.Enabled(features.SandboxSetGate) || !discovery.DiscoverGVK(controllerKind) {
		return false
	}
	return true
}

func (h *SandboxSetDefaulter) Handle(_ context.Context, req admission.Request) admission.Response {
	obj := &agentsv1alpha1.SandboxSet{}
	err := h.Decoder.Decode(req, obj)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	clone := obj.DeepCopy()
	setDefaultPodTemplate(obj.Spec.Template)
	setDefaultUpdateStrategy(&obj.Spec.UpdateStrategy)

	if req.Operation == admissionv1.Create && len(obj.Spec.PersistentContents) == 0 && len(defaultPersistentContents) > 0 {
		obj.Spec.PersistentContents = defaultPersistentContents
	}

	// Apply defaulting logic to volume claim templates
	setDefaultVolumeClaimTemplates(obj.Spec.VolumeClaimTemplates)

	if !reflect.DeepEqual(obj, clone) {
		marshal, err := json.Marshal(obj)
		if err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}
		return admission.PatchResponseFromRaw(req.Object.Raw, marshal)
	}
	return admission.Allowed("")
}

// setDefaultUpdateStrategy applies default values to the update strategy.
func setDefaultUpdateStrategy(strategy *agentsv1alpha1.SandboxSetUpdateStrategy) {
	if strategy.MaxUnavailable == nil {
		defaultMaxUnavailable := intstr.FromString("20%")
		strategy.MaxUnavailable = &defaultMaxUnavailable
	}
}

func setDefaultPodTemplate(template *v1.PodTemplateSpec) {
	if template == nil {
		return
	}
	if ptr.Deref(template.Spec.AutomountServiceAccountToken, true) {
		template.Spec.AutomountServiceAccountToken = ptr.To(false)
	}
	defaults.SetDefaultPodSpec(&template.Spec)
}

// setDefaultVolumeClaimTemplates applies default values to the volume claim templates
func setDefaultVolumeClaimTemplates(templates []v1.PersistentVolumeClaim) {
	for i := range templates {
		vct := &templates[i]
		// Set default access modes if not specified
		if len(vct.Spec.AccessModes) == 0 {
			vct.Spec.AccessModes = []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce}
		}

		// Set default volume mode if not specified
		if vct.Spec.VolumeMode == nil {
			volumeMode := v1.PersistentVolumeFilesystem
			vct.Spec.VolumeMode = &volumeMode
		}
	}
}
