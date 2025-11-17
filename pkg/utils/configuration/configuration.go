/*
Copyright 2025 The Kruise Authors.

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

package configuration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	SandboxConfigurationDir = "/configuration"

	SandboxResumeAcsPodPersistentContentKey = "sandbox.resume.acs.pod.persistent.content.json"
)

type ConfigurationObject struct {
	Key    string
	Object interface{}
}

var (
	sandboxConfigurations = map[string]interface{}{}

	objs = []ConfigurationObject{
		{
			Key:    SandboxResumeAcsPodPersistentContentKey,
			Object: &SandboxResumeAcsPodPersistentContent{},
		},
	}
)

func init() {
	logger := logf.FromContext(context.TODO())
	for i := range objs {
		obj := objs[i]
		filePath := filepath.Join(SandboxConfigurationDir, obj.Key)
		data, err := os.ReadFile(filePath)
		if err != nil {
			fmt.Println(fmt.Sprintf("read file(%s) failed: %s", filePath, err.Error()))
			logger.Error(err, "read file failed", "file", filePath)
			continue
		}
		err = json.Unmarshal(data, obj.Object)
		if err != nil {
			fmt.Println(fmt.Sprintf("read file(%s) failed: %s", filePath, err.Error()))
			logger.Error(err, "Unmarshal failed", "file", filePath, "data", string(data))
			continue
		}
		sandboxConfigurations[SandboxResumeAcsPodPersistentContentKey] = obj.Object
		logger.Info("read configuration file success", "file", filePath)
		fmt.Println(fmt.Sprintf("read file(%s) success", filePath))
	}
}

// SandboxResumeAcsPodPersistentContent record Pod configurations to be restored during resuming acs Pod.
type SandboxResumeAcsPodPersistentContent struct {
	AnnotationKeys []string `json:"annotationKeys"`
	LabelKeys      []string `json:"labelKeys"`
}

func GetSandboxResumeAcsPodPersistentContent() *SandboxResumeAcsPodPersistentContent {
	for key, obj := range sandboxConfigurations {
		if key == SandboxResumeAcsPodPersistentContentKey {
			content := obj.(*SandboxResumeAcsPodPersistentContent)
			return content
		}
	}
	return nil
}
