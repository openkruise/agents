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

package sessionservice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	agents "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/autopause"
	"github.com/openkruise/agents/pkg/discovery"
	sessionbinding "github.com/openkruise/agents/pkg/sessionservice/binding"
	"github.com/openkruise/agents/pkg/utils/expectations"
	webhookutils "github.com/openkruise/agents/pkg/webhook/utils"
)

var controllerKind = agents.GroupVersion.WithKind("SessionService")

const observationDelay = time.Second

// Add registers the SessionService controller when its API is available.
func Add(mgr manager.Manager) error {
	if !discovery.DiscoverGVK(controllerKind) {
		return nil
	}
	return NewReconciler(mgr.GetClient()).SetupWithManager(mgr)
}

// Reconciler keeps cache-observation expectations local to this controller.
type Reconciler struct {
	client.Client
	Expectations expectations.ScaleExpectations
}

func NewReconciler(c client.Client) *Reconciler {
	return &Reconciler{
		Client:       c,
		Expectations: expectations.NewScaleExpectations(),
	}
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("sessionservice").
		For(&agents.SessionService{}, builder.WithPredicates(r.parentExpectationCleanup())).Owns(&agents.Sandbox{}).
		Owns(&appsv1.ControllerRevision{}).Complete(r)
}

// parentExpectationCleanup observes old identities directly from informer events.
// It does not filter events; normal reconciliation still handles every change.
func (r *Reconciler) parentExpectationCleanup() predicate.Funcs {
	return predicate.Funcs{
		DeleteFunc: func(e event.DeleteEvent) bool {
			r.Expectations.DeleteExpectations(client.ObjectKeyFromObject(e.Object).String())
			return true
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			if e.ObjectOld.GetUID() != e.ObjectNew.GetUID() {
				r.Expectations.DeleteExpectations(client.ObjectKeyFromObject(e.ObjectOld).String())
			}
			return true
		},
	}
}

// +kubebuilder:rbac:groups=agents.kruise.io,resources=sessionservices,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sessionservices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sessionservices/finalizers,verbs=update
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sessionservices/scale,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxes,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=apps,resources=controllerrevisions,verbs=get;list;watch;create

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	parent := &agents.SessionService{}
	if err := r.Get(ctx, req.NamespacedName, parent); err != nil {
		if apierrors.IsNotFound(err) {
			r.Expectations.DeleteExpectations(req.NamespacedName.String())
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !parent.DeletionTimestamp.IsZero() {
		// Owner references delegate member deletion to Kubernetes garbage collection.
		r.Expectations.DeleteExpectations(req.NamespacedName.String())
		return ctrl.Result{}, nil
	}
	log := logf.FromContext(ctx).WithValues("sessionService", req.NamespacedName, "sessionServiceUID", parent.UID)
	ctx = logf.IntoContext(ctx, log)
	var snapshot agents.SandboxList
	if err := r.List(ctx, &snapshot, client.InNamespace(parent.Namespace), client.MatchingLabels{sessionbinding.SessionServiceUIDLabel: string(parent.UID)}, client.UnsafeDisableDeepCopy); err != nil {
		return ctrl.Result{}, fmt.Errorf("list SessionService members: %w", err)
	}
	status, active := calculateStatus(parent, snapshot.Items)
	if message := unsupported(parent); message != "" {
		setCondition(status, parent, "Supported", false, "Unsupported", message)
		return ctrl.Result{}, r.updateStatus(ctx, parent, status)
	}
	revision, unsupportedMessage, err := r.ensureRevision(ctx, parent)
	if err != nil {
		if apierrors.IsAlreadyExists(err) || apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: observationDelay}, nil
		}
		return ctrl.Result{}, err
	}
	if unsupportedMessage != "" {
		setCondition(status, parent, "Supported", false, "Unsupported", unsupportedMessage)
		return ctrl.Result{}, r.updateStatus(ctx, parent, status)
	}
	if status.CurrentRevision == "" && len(active) == 0 {
		status.CurrentRevision = revision
	}
	status.UpdateRevision = revision
	setCondition(status, parent, "Supported", true, "Supported", "Fixed-replica template provisioning is supported")
	r.observeExpectations(ctx, parent, snapshot.Items)
	if satisfied, elapsed, _ := r.Expectations.SatisfiedExpectations(expectationKey(parent)); !satisfied {
		if elapsed >= expectations.ExpectationTimeout {
			// Release observation waits, not replay old writes. The next reconcile
			// decides from its cached snapshot and the latest desired Revision.
			r.Expectations.DeleteExpectations(expectationKey(parent))
		}
		return ctrl.Result{RequeueAfter: observationDelay}, r.updateStatus(ctx, parent, status)
	}
	if err := r.reconcileMembers(ctx, parent, revision, snapshot.Items, active, status); err != nil {
		if apierrors.IsAlreadyExists(err) || apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			log.Info("member write requires fresh cached judgment", "error", err)
			return ctrl.Result{RequeueAfter: observationDelay}, r.updateStatus(ctx, parent, status)
		}
		return ctrl.Result{}, err
	}

	if err := r.updateStatus(ctx, parent, status); err != nil {
		return ctrl.Result{}, err
	}
	if satisfied, _, _ := r.Expectations.SatisfiedExpectations(expectationKey(parent)); !satisfied {
		return ctrl.Result{RequeueAfter: observationDelay}, nil
	}
	return ctrl.Result{}, nil
}

// calculateStatus observes one cached member snapshot without mutating it.
func calculateStatus(parent *agents.SessionService, snapshot []agents.Sandbox) (*agents.SessionServiceStatus, []*agents.Sandbox) {
	status := parent.Status.DeepCopy()
	status.ObservedGeneration = parent.Generation
	status.Replicas = 0
	status.ReadyReplicas = 0
	status.Selector = labels.SelectorFromSet(map[string]string{sessionbinding.SessionServiceUIDLabel: string(parent.UID)}).String()
	var active []*agents.Sandbox
	for i := range snapshot {
		sbx := &snapshot[i]
		if !effectiveMember(parent, sbx) {
			continue
		}
		active = append(active, sbx)
		status.Replicas++
		if readyMember(sbx) {
			status.ReadyReplicas++
		}
	}
	meta.RemoveStatusCondition(&status.Conditions, "ScaleDownBlocked")
	return status, active
}

// scaleUp creates from the current template and Revision. GenerateName deliberately
// provides no create idempotency: an unknown result can leave an extra member.
func (r *Reconciler) scaleUp(ctx context.Context, parent *agents.SessionService, revision string, count int32) error {
	for i := int32(0); i < count; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		sbx := newMember(parent, revision)
		if err := r.Create(ctx, sbx); err != nil {
			return fmt.Errorf("create SessionService member with prefix %s: %w", sbx.GenerateName, err)
		}
		// Observation runs in Reconcile, serialized for this parent, so the
		// returned generated name is registered before the next observation.
		r.Expectations.ExpectScale(expectationKey(parent), expectations.Create, sbx.Name)
		logf.FromContext(ctx).Info("created SessionService member", "sandbox", sbx.Name, "sandboxUID", sbx.UID, "revision", revision)
	}
	return nil
}

// scaleDown retains binding-aware deletion for a future Strict affinity policy.
// It is not used by reconciliation: both shrink and rollout currently ignore bindings.
// It deletes only empty effective members from the caller's snapshot.
// Remaining capacity is reported to Reconcile; this helper does not update status.
func (r *Reconciler) scaleDown(ctx context.Context, parent *agents.SessionService, active []*agents.Sandbox, remaining int32) (int32, error) {
	// Membership and bindings come from the same cached snapshot used by DELETE.
	sort.Slice(active, func(i, j int) bool {
		return active[i].Name < active[j].Name
	})
	for _, sbx := range active {
		if remaining == 0 {
			break
		}
		if err := ctx.Err(); err != nil {
			return remaining, err
		}
		if !hasNoBindings(sbx) {
			continue
		}
		if err := r.deleteMember(ctx, parent, sbx); err != nil {
			return remaining, err
		}
		remaining--
	}
	return remaining, nil
}

func unsupported(parent *agents.SessionService) string {
	template := &parent.Spec.SandboxTemplateSpec
	if template.Template == nil {
		return "template is required"
	}
	if template.Backend != "" && template.Backend != "k8s" {
		return "Only the k8s template backend is supported"
	}
	if template.BuildOptions != nil {
		return "buildOptions is not supported by SessionService"
	}
	if errs := validateTemplate(template); len(errs) > 0 {
		return errs.ToAggregate().Error()
	}
	if parent.Spec.SessionPolicy.PauseAfterIdle != "" || parent.Spec.SessionPolicy.ExpireAfterIdle != "" {
		return "Idle pause and expiry are not implemented"
	}
	if _, err := maxSurge(parent); err != nil {
		return err.Error()
	}
	return ""
}

func validateTemplate(template *agents.SandboxTemplateSpec) field.ErrorList {
	path := field.NewPath("spec")
	errs := autopause.ValidateProbes(template.Probes, path.Child("probes"))
	errs = append(errs, autopause.ValidateAutoPausePolicy(template.AutoPausePolicy, template.Probes, path.Child("autoPausePolicy"))...)
	names := make(map[string]struct{}, len(template.VolumeClaimTemplates))
	for i := range template.VolumeClaimTemplates {
		name := template.VolumeClaimTemplates[i].Name
		namePath := path.Child("volumeClaimTemplates").Index(i).Child("metadata", "name")
		// Claim template names also become Pod volume and volumeMount names.
		for _, message := range utilvalidation.IsDNS1123Label(name) {
			errs = append(errs, field.Invalid(namePath, name, message))
		}
		if _, exists := names[name]; exists {
			errs = append(errs, field.Duplicate(namePath, name))
		}
		names[name] = struct{}{}
	}
	errs = append(errs, webhookutils.ValidateVolumeClaimTemplateMounts(template.Template, template.VolumeClaimTemplates, path)...)
	if lifecycle := template.Lifecycle; lifecycle != nil {
		lifecyclePath := path.Child("lifecycle")
		if lifecycle.PreUpgrade != nil || lifecycle.PostUpgrade != nil {
			errs = append(errs, field.Forbidden(lifecyclePath, "preUpgrade and postUpgrade are not allowed on SandboxTemplate"))
		}
	}
	return errs
}

func owned(parent *agents.SessionService, obj metav1.Object) bool {
	owner := metav1.GetControllerOf(obj)
	return obj.GetNamespace() == parent.Namespace &&
		owner != nil &&
		owner.UID == parent.UID &&
		owner.Name == parent.Name &&
		owner.Kind == controllerKind.Kind &&
		owner.APIVersion == controllerKind.GroupVersion().String() &&
		obj.GetLabels()[sessionbinding.SessionServiceUIDLabel] == string(parent.UID)
}

func effectiveMember(parent *agents.SessionService, sbx *agents.Sandbox) bool {
	if !owned(parent, sbx) || !sbx.DeletionTimestamp.IsZero() {
		return false
	}
	switch sbx.Status.Phase {
	case agents.SandboxSucceeded, agents.SandboxFailed, agents.SandboxTerminating:
		return false
	}
	return true
}

func hasNoBindings(sbx *agents.Sandbox) bool {
	bindings, err := sessionbinding.Decode(sbx.Annotations)
	return err == nil && len(bindings) == 0
}

func expectationKey(parent *agents.SessionService) string {
	return client.ObjectKeyFromObject(parent).String()
}

func (r *Reconciler) observeExpectations(ctx context.Context, parent *agents.SessionService, snapshot []agents.Sandbox) {
	key := expectationKey(parent)
	seen := map[string]*agents.Sandbox{}
	for i := range snapshot {
		seen[snapshot[i].Name] = &snapshot[i]
		r.Expectations.ObserveScale(key, expectations.Create, snapshot[i].Name)
	}
	for name := range r.Expectations.GetExpectations(key)[expectations.Delete] {
		if sbx := seen[name]; sbx == nil || !sbx.DeletionTimestamp.IsZero() || !owned(parent, sbx) {
			logf.FromContext(ctx).Info("observed completed SessionService deletion", "sandbox", name)
			r.Expectations.ObserveScale(key, expectations.Delete, name)
		}
	}
}

func memberPrefix(parent *agents.SessionService) string {
	name := parent.Name
	if len(name) > 40 {
		name = name[:40]
	}
	hash := sha256.Sum256([]byte(parent.UID))
	return strings.TrimRight(name, "-.") + fmt.Sprintf("-%x-", hash[:4])
}

func newMember(parent *agents.SessionService, revision string) *agents.Sandbox {
	creation := parent.Spec.SandboxTemplateSpec.DeepCopy()
	template := creation.Template
	if template.Labels == nil {
		template.Labels = map[string]string{}
	}
	template.Labels[sessionbinding.SessionServiceUIDLabel] = string(parent.UID)
	template.Labels[sessionbinding.SessionServiceRevisionLabel] = revision
	annotations := maps.Clone(template.Annotations)
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[sessionbinding.BindingsAnnotation] = "{}"
	return &agents.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName:    memberPrefix(parent),
			Namespace:       parent.Namespace,
			Labels:          maps.Clone(template.Labels),
			Annotations:     annotations,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(parent, controllerKind)},
		},
		Spec: agents.SandboxSpec{
			PauseStrategy:      creation.PauseStrategy,
			PersistentContents: creation.PersistentContents,
			Runtimes:           creation.Runtimes,
			Probes:             creation.Probes,
			AutoPausePolicy:    creation.AutoPausePolicy,
			Lifecycle:          creation.Lifecycle,
			StartupMode:        creation.StartupMode,
			EmbeddedSandboxTemplate: agents.EmbeddedSandboxTemplate{
				Template:             template,
				VolumeClaimTemplates: creation.VolumeClaimTemplates,
			},
		},
	}
}

func provisionRevisionName(parent *agents.SessionService, data []byte) string {
	hash := sha256.Sum256(append([]byte(string(parent.UID)+"\x00"), data...))
	return memberPrefix(parent) + fmt.Sprintf("%x", hash[:6])
}

func (r *Reconciler) ensureRevision(ctx context.Context, parent *agents.SessionService) (string, string, error) {
	data, err := json.Marshal(parent.Spec.SandboxTemplateSpec)
	if err != nil {
		return "", "", fmt.Errorf("encode SessionService creation input: %w", err)
	}
	name := provisionRevisionName(parent, data)
	var revisions appsv1.ControllerRevisionList
	if err := r.List(ctx, &revisions, client.InNamespace(parent.Namespace), client.MatchingLabels{sessionbinding.SessionServiceUIDLabel: string(parent.UID)}); err != nil {
		return "", "", fmt.Errorf("list SessionService revisions: %w", err)
	}
	found := false
	nextRevision := int64(1)
	for i := range revisions.Items {
		revision := &revisions.Items[i]
		if !owned(parent, revision) {
			if revision.Name == name {
				return "", "Revision name is occupied by different ownership", nil
			}
			continue
		}
		if revision.Revision >= nextRevision {
			nextRevision = revision.Revision + 1
		}
		if revision.Name == name {
			if !bytes.Equal(revision.Data.Raw, data) {
				return "", "Revision name is occupied by different content", nil
			}
			found = true
		}
	}
	if found {
		return name, "", nil
	}
	revision := &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: parent.Namespace,
			Labels: map[string]string{
				sessionbinding.SessionServiceUIDLabel: string(parent.UID),
			},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(parent, controllerKind)},
		},
		Revision: nextRevision,
		Data:     runtime.RawExtension{Raw: data},
	}
	if err := r.Create(ctx, revision); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return "", "", fmt.Errorf("create SessionService revision: %w", err)
		}
		var existing appsv1.ControllerRevision
		if err := r.Get(ctx, client.ObjectKeyFromObject(revision), &existing); err != nil {
			return "", "", err
		}
		if !owned(parent, &existing) || !bytes.Equal(existing.Data.Raw, data) {
			return "", "Revision name is occupied by different ownership or content", nil
		}
	}
	return name, "", nil
}

func setCondition(status *agents.SessionServiceStatus, parent *agents.SessionService, kind string, value bool, reason, message string) {
	condition := metav1.ConditionFalse
	if value {
		condition = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               kind,
		Status:             condition,
		ObservedGeneration: parent.Generation,
		Reason:             reason,
		Message:            message,
	})
}

func (r *Reconciler) updateStatus(ctx context.Context, parent *agents.SessionService, status *agents.SessionServiceStatus) error {
	if apiequality.Semantic.DeepEqual(parent.Status, *status) {
		return nil
	}
	updated := parent.DeepCopy()
	updated.Status = *status.DeepCopy()
	if err := r.Status().Update(ctx, updated); err != nil {
		return fmt.Errorf("update SessionService status: %w", err)
	}
	return nil
}

func desiredReplicas(parent *agents.SessionService) int32 {
	if parent.Spec.Replicas == nil {
		return 1
	}
	return *parent.Spec.Replicas
}

func maxSurge(parent *agents.SessionService) (int32, error) {
	value := intstr.FromString("25%")
	if parent.Spec.UpdateStrategy.MaxSurge != nil {
		value = *parent.Spec.UpdateStrategy.MaxSurge
	}
	if value.Type == intstr.Int {
		if value.IntVal <= 0 {
			return 0, fmt.Errorf("maxSurge must be positive")
		}
		return value.IntVal, nil
	}
	if value.Type != intstr.String {
		return 0, fmt.Errorf("invalid maxSurge type")
	}
	text := value.StrVal
	if !strings.HasSuffix(text, "%") {
		return 0, fmt.Errorf("maxSurge must be an integer or percentage")
	}
	digits := strings.TrimSuffix(text, "%")
	for _, ch := range digits {
		if ch < '0' || ch > '9' {
			return 0, fmt.Errorf("invalid maxSurge percentage")
		}
	}
	percent, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || percent <= 0 {
		return 0, fmt.Errorf("maxSurge percentage must be positive")
	}
	desired := int64(desiredReplicas(parent))
	// Divide before multiplication to avoid overflowing large valid percentages.
	if desired > 0 && percent > (int64(math.MaxInt32)*100)/desired {
		return 0, fmt.Errorf("maxSurge exceeds supported replica range")
	}
	result := (desired*percent + 99) / 100
	if result < 0 || result > math.MaxInt32 {
		return 0, fmt.Errorf("maxSurge exceeds supported replica range")
	}
	return int32(result), nil
}

func readyMember(sbx *agents.Sandbox) bool {
	return sbx.Status.Phase == agents.SandboxRunning && meta.IsStatusConditionTrue(sbx.Status.Conditions, string(agents.SandboxConditionReady))
}

// deleteMember conditionally deletes a member without inspecting session bindings.
// Callers enforce replica and rollout Ready budgets. Pod grace/preStop remain normal.
func (r *Reconciler) deleteMember(ctx context.Context, parent *agents.SessionService, sbx *agents.Sandbox) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	uid, rv := sbx.UID, sbx.ResourceVersion
	key := expectationKey(parent)
	r.Expectations.ExpectScale(key, expectations.Delete, sbx.Name)
	if err := r.Delete(ctx, sbx, client.Preconditions{UID: &uid, ResourceVersion: &rv}); err != nil {
		r.Expectations.ObserveScale(key, expectations.Delete, sbx.Name)
		return fmt.Errorf("delete SessionService member %s: %w", sbx.Name, err)
	}
	return nil
}

func (r *Reconciler) reconcileMembers(ctx context.Context, parent *agents.SessionService, revision string, snapshot []agents.Sandbox, active []*agents.Sandbox, status *agents.SessionServiceStatus) error {
	desired := desiredReplicas(parent)
	surge, err := maxSurge(parent)
	if err != nil {
		setCondition(status, parent, "Supported", false, "Unsupported", err.Error())
		return nil
	}
	members := observeMemberRevisions(parent, revision, snapshot, active, status.Replicas)
	updatedCount := len(members.updated)
	if updatedCount > math.MaxInt32 {
		return fmt.Errorf("updated member count exceeds supported replica range")
	}
	updated := int32(updatedCount)
	rolling := len(members.old) > 0 || (status.CurrentRevision != "" && status.CurrentRevision != revision)
	capacityLimit := int64(desired) + int64(surge)

	if desired == 0 && members.total == 0 {
		status.CurrentRevision = revision
		return nil
	}

	// Ordinary refill counts effective members. During a rollout, terminating CRs
	// also occupy capacity until they disappear from the observed snapshot.
	occupied := status.Replicas
	if rolling {
		occupied = members.total
	}
	if desired > 0 && updated == desired && members.updatedReady == desired && occupied == desired {
		status.CurrentRevision = revision
		return nil
	}
	space := capacityLimit - int64(occupied)
	// Create first when capacity allows; never create and delete in one reconcile.
	// Expectations and subsequent member observations gate the next decision.
	if updated < desired && space > 0 {
		return r.scaleUp(ctx, parent, revision, min(desired-updated, int32(min(space, 16))))
	}

	return r.reconcileMemberDeletions(ctx, parent, revision, members, desired, space, status)
}

// memberRevisions summarizes the same observation used for capacity decisions.
// total includes controlled terminating CRs, which are absent from active.
type memberRevisions struct {
	old, updated []*agents.Sandbox
	updatedReady int32
	total        int32
}

func observeMemberRevisions(parent *agents.SessionService, revision string, snapshot []agents.Sandbox, active []*agents.Sandbox, replicas int32) memberRevisions {
	members := memberRevisions{total: replicas}
	for _, sbx := range active {
		if sbx.Labels[sessionbinding.SessionServiceRevisionLabel] != revision {
			members.old = append(members.old, sbx)
			continue
		}
		members.updated = append(members.updated, sbx)
		if readyMember(sbx) {
			members.updatedReady++
		}
	}
	for i := range snapshot {
		sbx := &snapshot[i]
		if owned(parent, sbx) && (!sbx.DeletionTimestamp.IsZero() || sbx.Status.Phase == agents.SandboxTerminating) {
			members.total++
		}
	}
	return members
}

// scaleDownAndReportBindings is reserved for future integration when affinity is Strict.
// Reconciliation does not call it yet: both Strict and BestEffort currently ignore
// bindings when deleting members.
func (r *Reconciler) scaleDownAndReportBindings(ctx context.Context, parent *agents.SessionService, candidates []*agents.Sandbox, count int32, status *agents.SessionServiceStatus) error {
	remaining, err := r.scaleDown(ctx, parent, candidates, count)
	if remaining > 0 {
		setCondition(status, parent, "ScaleDownBlocked", true, "BindingsPresentOrInvalid", "Surplus members have occupied or invalid bindings")
	}
	return err
}

// reconcileMemberDeletions applies one deletion policy to shrink and rollout.
// Bindings do not protect members. Unready members are preferred, then obsolete
// revisions, with one shared budget preventing deletion below desired Ready capacity.
// Reconcile reaches this function only when it cannot first create target members.
func (r *Reconciler) reconcileMemberDeletions(ctx context.Context, parent *agents.SessionService, revision string, members memberRevisions, desired int32, space int64, status *agents.SessionServiceStatus) error {
	remaining := int64(status.Replicas) - int64(desired)
	if remaining <= 0 {
		return nil
	}
	updated := int64(len(members.updated))
	if updated < int64(desired) {
		// Reclaim excess capacity, or one full slot, before creating next round.
		remaining = min(remaining, max(int64(1), -space))
	}
	targetSurplus := max(updated-int64(desired), int64(0))
	overCapacity := max(-space, int64(0))
	readyBudget := status.ReadyReplicas - desired
	candidates := make([]*agents.Sandbox, 0, len(members.old)+len(members.updated))
	candidates = append(candidates, members.old...)
	candidates = append(candidates, members.updated...)
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if readyMember(a) != readyMember(b) {
			return !readyMember(a)
		}
		aUpdated := a.Labels[sessionbinding.SessionServiceRevisionLabel] == revision
		bUpdated := b.Labels[sessionbinding.SessionServiceRevisionLabel] == revision
		if aUpdated != bUpdated {
			return !aUpdated
		}
		return a.Name < b.Name
	})
	for _, sbx := range candidates {
		if remaining <= 0 {
			break
		}
		ready := readyMember(sbx)
		isUpdated := sbx.Labels[sessionbinding.SessionServiceRevisionLabel] == revision
		// Preserve desired target members unless an unready one must be removed
		// to reclaim over-capacity without sacrificing a Ready old member.
		if isUpdated && targetSurplus <= 0 && (overCapacity <= 0 || ready) {
			continue
		}
		if ready && readyBudget <= 0 {
			continue
		}
		if err := r.deleteMember(ctx, parent, sbx); err != nil {
			return err
		}
		remaining--
		if ready {
			readyBudget--
		}
		if isUpdated && targetSurplus > 0 {
			targetSurplus--
		}
		if overCapacity > 0 {
			overCapacity--
		}
	}
	return nil
}
