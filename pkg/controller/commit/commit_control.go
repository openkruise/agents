package commit

import (
	"fmt"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/controller/commit/core"
	commitutil "github.com/openkruise/agents/pkg/utils/commit"
)

func (r *CommitReconciler) getControl(commit *agentsv1alpha1.Commit) (core.CommitControl, error) {
	if mode, ok := commit.Annotations[commitutil.AnnotationModeKey]; ok && mode != "" {
		control, ok := r.controls[mode]
		if !ok {
			return nil, fmt.Errorf("commit mode(%s) control not found", mode)
		}
		return control, nil
	}
	control, ok := r.controls[core.CommonControlName]
	if !ok {
		return nil, fmt.Errorf("commit mode(%s) control not found", core.CommonControlName)
	}
	return control, nil
}
