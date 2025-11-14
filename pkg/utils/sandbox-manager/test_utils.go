package utils

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/infra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

type FakeSandbox struct {
	DeletionTimestamp *metav1.Time
	State             string
}

func (f FakeSandbox) GetNamespace() string {
	return ""
}

func (f FakeSandbox) SetNamespace(string) {
	// noop
}

func (f FakeSandbox) GetName() string {
	return ""
}

func (f FakeSandbox) SetName(string) {
	// noop
}

func (f FakeSandbox) GetGenerateName() string {
	return ""
}

func (f FakeSandbox) SetGenerateName(string) {
	// noop
}

func (f FakeSandbox) GetUID() types.UID {
	return ""
}

func (f FakeSandbox) SetUID(types.UID) {
	// noop
}

func (f FakeSandbox) GetResourceVersion() string {
	return ""
}

func (f FakeSandbox) SetResourceVersion(string) {
	// noop
}

func (f FakeSandbox) GetGeneration() int64 {
	return 0
}

func (f FakeSandbox) SetGeneration(int64) {
	// noop
}

func (f FakeSandbox) GetSelfLink() string {
	return ""
}

func (f FakeSandbox) SetSelfLink(string) {
	// noop
}

func (f FakeSandbox) GetCreationTimestamp() metav1.Time {
	return metav1.Time{}
}

func (f FakeSandbox) SetCreationTimestamp(metav1.Time) {
	// noop
}

func (f FakeSandbox) GetDeletionTimestamp() *metav1.Time {
	return f.DeletionTimestamp
}

func (f FakeSandbox) SetDeletionTimestamp(*metav1.Time) {
	// noop
}

func (f FakeSandbox) GetDeletionGracePeriodSeconds() *int64 {
	return nil
}

func (f FakeSandbox) SetDeletionGracePeriodSeconds(*int64) {
	// noop
}

func (f FakeSandbox) GetLabels() map[string]string {
	return nil
}

func (f FakeSandbox) SetLabels(map[string]string) {
	// noop
}

func (f FakeSandbox) GetAnnotations() map[string]string {
	return nil
}

func (f FakeSandbox) SetAnnotations(map[string]string) {
	// noop
}

func (f FakeSandbox) GetFinalizers() []string {
	return nil
}

func (f FakeSandbox) SetFinalizers([]string) {
	// noop
}

func (f FakeSandbox) GetOwnerReferences() []metav1.OwnerReference {
	return nil
}

func (f FakeSandbox) SetOwnerReferences([]metav1.OwnerReference) {
	// noop
}

func (f FakeSandbox) GetManagedFields() []metav1.ManagedFieldsEntry {
	return nil
}

func (f FakeSandbox) SetManagedFields([]metav1.ManagedFieldsEntry) {
	// noop
}

func (f FakeSandbox) Pause(context.Context) error {
	return nil
}

func (f FakeSandbox) Resume(context.Context) error {
	return nil
}

func (f FakeSandbox) GetIP() string {
	return ""
}

func (f FakeSandbox) GetState() string {
	return f.State
}

func (f FakeSandbox) GetTemplate() string {
	return ""
}

func (f FakeSandbox) GetResource() infra.SandboxResource {
	return infra.SandboxResource{}
}

func (f FakeSandbox) GetOwnerUser() string {
	return ""
}

func (f FakeSandbox) PatchLabels(context.Context, map[string]string) error {
	return nil
}

func (f FakeSandbox) SetState(context.Context, string) error {
	return nil
}

func (f FakeSandbox) SaveTimer(context.Context, int, consts.EventType, bool, string) error {
	return nil
}

func (f FakeSandbox) LoadTimers(func(time.Duration, consts.EventType)) error {
	return nil
}

func (f FakeSandbox) Kill(context.Context) error {
	return nil
}

func (f FakeSandbox) InplaceRefresh(bool) error {
	return nil
}

func (f FakeSandbox) Request(*http.Request, string, int) (*http.Response, error) {
	return nil, nil
}

func (f FakeSandbox) GetRouteHeader() map[string]string {
	return nil
}

var initOnce sync.Once

func InitKLogOutput() {
	initOnce.Do(func() {
		klog.InitFlags(nil)
		_ = flag.Set("v", fmt.Sprintf("%d", consts.DebugLogLevel))
		flag.Parse()
	})
}
