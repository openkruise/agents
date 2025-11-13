package infra

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash"
	"hash/fnv"
	"os"
	"regexp"
	"strings"

	"github.com/davecgh/go-spew/spew"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

type SandboxTemplate struct {
	metav1.ObjectMeta `json:"metadata"`
	Spec              SandboxTemplateSpec `json:"spec"`
}

type SandboxTemplateSpec struct {
	MinPoolSize int32                  `json:"minPoolSize"`
	MaxPoolSize int32                  `json:"maxPoolSize"`
	ExpectUsage *intstr.IntOrString    `json:"expectUsage"`
	Template    corev1.PodTemplateSpec `json:"template"`
}

func (t *SandboxTemplate) Init(namespace string) {
	templateHash := ComputeHash(&t.Spec.Template, nil)
	if t.Name == "" {
		t.Name = "base"
	}
	t.Namespace = namespace
	if t.Spec.MinPoolSize == 0 {
		t.Spec.MinPoolSize = consts.DefaultMinPoolSize
	}
	if t.Spec.MaxPoolSize < t.Spec.MinPoolSize {
		t.Spec.MaxPoolSize = t.Spec.MinPoolSize * consts.DefaultMaxPoolSizeFactor
	}
	if t.Spec.ExpectUsage == nil {
		t.Spec.ExpectUsage = ptr.To(intstr.FromString("50%"))
	}
	t.Labels = clearAndInitInnerKeys(t.Labels)
	t.Labels[consts.LabelTemplateHash] = templateHash
	t.Labels[consts.LabelSandboxPool] = t.Name
	t.Spec.Template.Labels = clearAndInitInnerKeys(t.Spec.Template.Labels)
	t.Spec.Template.Labels[consts.LabelTemplateHash] = templateHash
	t.Spec.Template.Labels[consts.LabelSandboxPool] = t.Name
	t.Annotations = clearAndInitInnerKeys(t.Annotations)
	t.Spec.Template.Annotations = clearAndInitInnerKeys(t.Spec.Template.Annotations)
}

func clearAndInitInnerKeys(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	for k := range m {
		if strings.HasPrefix(k, consts.InternalPrefix) {
			delete(m, k)
		}
	}
	return m
}

//goland:noinspection SpellCheckingInspection
const alphanums = "bcdfghjklmnpqrstvwxz2456789"

// DeepHashObject writes specified object to hash using the spew library
// which follows pointers and prints actual values of the nested objects
// ensuring the hash does not change when a pointer changes.
func DeepHashObject(hasher hash.Hash, objectToWrite interface{}) {
	hasher.Reset()
	printer := spew.ConfigState{
		Indent:         " ",
		SortKeys:       true,
		DisableMethods: true,
		SpewKeys:       true,
	}
	printer.Fprintf(hasher, "%#v", objectToWrite)
}

// ComputeHash returns a hash value calculated from pod template and
// a collisionCount to avoid hash collision. The hash will be safe encoded to
// avoid bad words.
func ComputeHash(template *corev1.PodTemplateSpec, collisionCount *int32) string {
	podTemplateSpecHasher := fnv.New32a()
	DeepHashObject(podTemplateSpecHasher, *template)

	// Add collisionCount in the hash if it exists.
	if collisionCount != nil {
		collisionCountBytes := make([]byte, 8)
		binary.LittleEndian.PutUint32(collisionCountBytes, uint32(*collisionCount))
		podTemplateSpecHasher.Write(collisionCountBytes)
	}

	return SafeEncodeString(fmt.Sprint(podTemplateSpecHasher.Sum32()))
}

// SafeEncodeString encodes s using the same characters as rand.String. This reduces the chances of bad words and
// ensures that strings generated from hash functions appear consistent throughout the API.
func SafeEncodeString(s string) string {
	r := make([]byte, len(s))
	for i, b := range []rune(s) {
		r[i] = alphanums[int(b)%len(alphanums)]
	}
	return string(r)
}

var isYamlMatcher = regexp.MustCompile(`\.ya?ml$`)
var isTemplateMatcher = regexp.MustCompile(`kind:\s+SandboxTemplate`)

// LoadBuiltinTemplates loads built-in templates from local directory.
// Currently, custom templates are not supported yet.
// Maybe we can use CRs to persistent user templates in the future.
func LoadBuiltinTemplates(ctx context.Context, infra Infrastructure, templateDir string, namespace string) error {
	log := klog.FromContext(ctx)
	// Check if the template directory exists
	if _, err := os.Stat(templateDir); os.IsNotExist(err) {
		return fmt.Errorf("template directory %s does not exist", templateDir)
	}

	// Read all files in the template directory
	files, err := os.ReadDir(templateDir)
	if err != nil {
		return err
	}

	// Iterate through each file
	for _, file := range files {
		// Process only YAML files
		if !file.IsDir() && isYamlMatcher.MatchString(file.Name()) {
			filePath := templateDir + "/" + file.Name()

			// Read the file content
			data, err := os.ReadFile(filePath)
			if err != nil {
				return fmt.Errorf("failed to read template file %s: %w", filePath, err)
			}

			// Check if the file contains "kind: SandboxTemplate"
			if isTemplateMatcher.Match(data) {
				// Unmarshal into models.SandboxTemplate structure
				// Note: This requires importing the models package and defining the SandboxTemplate struct
				// For now, we'll just log that we found a matching file
				log.Info("found SandboxTemplate", "file", filePath)
				var t SandboxTemplate
				if err := yaml.Unmarshal(data, &t); err != nil {
					return err
				}
				if _, ok := infra.GetPoolByTemplate(t.Name); ok {
					log.Info("template name conflict", "name", t.Name)
					return fmt.Errorf("template name conflict: %s", t.Name)
				}
				t.Init(namespace)
				metadata := infra.InjectTemplateMetadata()
				for k, v := range metadata.Labels {
					t.Spec.Template.Labels[k] = v
				}
				for k, v := range metadata.Annotations {
					t.Spec.Template.Annotations[k] = v
				}
				infra.AddPool(t.Name, infra.NewPoolFromTemplate(&t))
			}
		}
	}
	return nil
}
