package sandboxcr

import (
	"context"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	utils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func ModifyPickedSandbox(ctx context.Context, sbx *Sandbox, opts infra.ClaimSandboxOptions) error {
	if err := sbx.InplaceRefresh(ctx, true); err != nil {
		return err
	}
	if opts.Modifier != nil {
		opts.Modifier(sbx)
	}
	if opts.InplaceUpdate != nil {
		// should perform an inplace update
		sbx.SetImage(opts.InplaceUpdate.Image)
	}
	// claim sandbox
	sbx.SetOwnerReferences([]metav1.OwnerReference{}) // make SandboxSet scale up
	labels := sbx.GetLabels()
	if labels == nil {
		labels = make(map[string]string, 1)
	}
	labels[agentsv1alpha1.LabelSandboxIsClaimed] = "true"
	sbx.SetLabels(labels)

	sbx.Annotations[agentsv1alpha1.AnnotationClaimTime] = time.Now().Format(time.RFC3339)
	return nil
}

func PerformLockSandbox(ctx context.Context, sbx *Sandbox, lock string, owner string, client clients.SandboxClient) (time.Duration, error) {
	start := time.Now()
	utils.LockSandbox(sbx.Sandbox, lock, owner)
	updated, err := client.ApiV1alpha1().Sandboxes(sbx.Namespace).Update(ctx, sbx.Sandbox, metav1.UpdateOptions{})
	if err == nil {
		sbx.Sandbox = updated
		return time.Since(start), nil
	}
	return time.Since(start), err
}
