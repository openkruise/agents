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
	"errors"
	"fmt"
	"maps"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	agents "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/discovery"
	sessionbinding "github.com/openkruise/agents/pkg/sessionservice/binding"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/expectations"
)

var controllerKind = agents.GroupVersion.WithKind("SessionService")

const observationDelay = time.Second

const revisionUIDIndex = "sessionServiceUID"

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

func revisionUID(obj client.Object) []string {
	uid := obj.GetLabels()[sessionbinding.SessionServiceUIDLabel]
	if uid == "" {
		return nil
	}
	return []string{uid}
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID); err != nil {
		return fmt.Errorf("index SessionService revisions: %w", err)
	}
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
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxes,verbs=get;list;watch;create;delete;patch
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
	// Expiry is independent of migration, including for Moving records.
	expired, nextExpiry, err := r.expireBindings(ctx, parent, snapshot.Items, time.Now())
	if err != nil {
		return ctrl.Result{}, err
	}
	if expired {
		// Binding writes invalidate this snapshot for both migration and scaling.
		return ctrl.Result{RequeueAfter: observationDelay}, r.updateStatus(ctx, parent, status)
	}
	changed, moving, err := r.reconcileRebindings(ctx, parent, snapshot.Items)
	if err != nil {
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: observationDelay}, nil
		}
		return ctrl.Result{}, err
	}
	if changed {
		// Do not scale from a snapshot predating a binding mutation.
		return ctrl.Result{RequeueAfter: observationDelay}, r.updateStatus(ctx, parent, status)
	}
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
	if moving && (nextExpiry == 0 || nextExpiry > observationDelay) {
		nextExpiry = observationDelay
	}
	return ctrl.Result{RequeueAfter: nextExpiry}, nil
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

func unsupported(parent *agents.SessionService) string {
	template := &parent.Spec.SandboxTemplateSpec
	if template.Template == nil {
		return "template is required"
	}
	if parent.Spec.SessionPolicy.PauseAfterIdle != "" || parent.Spec.SessionPolicy.ExpireAfterIdle != "" {
		return "Idle pause and expiry are not implemented"
	}
	if value := parent.Spec.SessionPolicy.ExpireAfterCreation; value != "" {
		if duration, err := time.ParseDuration(value); err != nil || duration <= 0 {
			return "Session creation lifetime must be a positive duration"
		}
	}
	return ""
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

// memberPrefix is only a readable prefix; owner UID and labels establish identity.
func memberPrefix(parent *agents.SessionService) string {
	name := parent.Name[:min(len(parent.Name), 40)]
	return strings.TrimRight(name, "-.") + "-"
}

func revisionPrefix(parent *agents.SessionService) string {
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
	return revisionPrefix(parent) + fmt.Sprintf("%x", hash[:6])
}

func (r *Reconciler) ensureRevision(ctx context.Context, parent *agents.SessionService) (string, string, error) {
	data, err := json.Marshal(parent.Spec.SandboxTemplateSpec)
	if err != nil {
		return "", "", fmt.Errorf("encode SessionService creation input: %w", err)
	}
	name := provisionRevisionName(parent, data)
	var revisions appsv1.ControllerRevisionList
	if err := r.List(ctx, &revisions, client.InNamespace(parent.Namespace), client.MatchingFields{revisionUIDIndex: string(parent.UID)}); err != nil {
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
	surge, err := utils.SessionServiceMaxSurge(parent)
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

	return r.scaleDown(ctx, parent, revision, members, desired, space, status)
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

// reconcileMemberDeletions applies one deletion policy to shrink and rollout.
// Bindings do not protect members. Unready members are preferred, then obsolete
// revisions, with one shared budget preventing deletion below desired Ready capacity.
// Reconcile reaches this function only when it cannot first create target members.
func (r *Reconciler) scaleDown(ctx context.Context, parent *agents.SessionService, revision string, members memberRevisions, desired int32, space int64, status *agents.SessionServiceStatus) error {
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
	// Bindings are a preference, not a retention policy. Decode each candidate
	// once from the same snapshot used by conditional deletion.
	emptyBindings := make(map[*agents.Sandbox]bool, len(candidates))
	for _, sbx := range candidates {
		emptyBindings[sbx] = hasNoBindings(sbx)
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if emptyBindings[a] != emptyBindings[b] {
			return emptyBindings[a]
		}
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

// expireBindings cleans each owned member from one decision snapshot. It does not
// interpret migration phases: late migration writes retain an expired createdAt
// and remain unroutable until a later cleanup observes them.
func (r *Reconciler) expireBindings(ctx context.Context, parent *agents.SessionService, snapshot []agents.Sandbox, now time.Time) (changed bool, next time.Duration, err error) {
	lifetime := parent.Spec.SessionPolicy.ExpireAfterCreation
	if lifetime == "" {
		return false, 0, nil
	}
	for i := range snapshot {
		if ctx.Err() != nil {
			return changed, next, errors.Join(err, ctx.Err())
		}
		box := &snapshot[i]
		if !owned(parent, box) || !box.DeletionTimestamp.IsZero() {
			continue
		}
		records, decodeErr := sessionbinding.Decode(box.Annotations)
		if decodeErr != nil {
			err = errors.Join(err, fmt.Errorf("decode member %s expiry bindings: %w", box.Name, decodeErr))
			continue
		}
		remaining := maps.Clone(records)
		for id, record := range records {
			deadline, expiryErr := sessionbinding.ExpiresAt(record, lifetime)
			if expiryErr != nil {
				return changed, next, errors.Join(err, expiryErr)
			}
			if !now.Before(deadline) {
				delete(remaining, id)
				continue
			}
			if wait := deadline.Sub(now); next == 0 || wait < next {
				next = wait
			}
		}
		if len(remaining) == len(records) {
			continue
		}
		// Success or conflict requires another observation, never an RV-only replay.
		if next == 0 || next > observationDelay {
			next = observationDelay
		}
		if _, patchErr := r.patchBindings(ctx, box, remaining); patchErr != nil {
			if !apierrors.IsConflict(patchErr) && !apierrors.IsNotFound(patchErr) {
				err = errors.Join(err, patchErr)
			}
			continue
		}
		changed = true
	}
	return changed, next, err
}

// reconcileRebindings makes the source Moving record a durable migration journal.
// Gateway only requests a move. The elected controller persists its target before
// writing that target, then removes the source only after confirming the target.
// Each source advances its tasks as far as successful API responses allow;
// leader handoff resumes from CRs, not local state. UID/RV patches make retries
// conditional and idempotent, but are not a cross-object transaction or fencing
// against arbitrary old-leader work.
// Recorded targets are preferred recovery hints; duplicate Active copies from
// late writes are repaired on subsequent observations.
func (r *Reconciler) reconcileRebindings(ctx context.Context, parent *agents.SessionService, snapshot []agents.Sandbox) (changed, pending bool, err error) {
	bindings, sources, decodeErr := collectBindingMigrations(ctx, parent, snapshot)
	repaired, duplicates, repairErr := r.reconcileDuplicateBindings(ctx, bindings, decodeErr)
	if duplicates {
		// Repair may have changed members, or hit an unresolved conditional write.
		// Neither migration nor scaling may use this pre-repair snapshot.
		return repaired, true, repairErr
	}
	if len(sources) == 0 {
		return false, false, nil
	}
	if decodeErr != nil {
		return false, true, decodeErr
	}
	// Each source owns a private view. Successful API responses advance that view;
	// concurrent workers sharing a target instead conflict on its captured RV.
	const concurrency = 10
	workers := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, original := range sources {
		select {
		case workers <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return changed, true, errors.Join(err, ctx.Err())
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-workers }()
			taskChanged, taskErr := r.reconcileSourceBindings(ctx, parent, original, bindings)
			mu.Lock()
			changed = changed || taskChanged
			if taskErr != nil && !apierrors.IsConflict(taskErr) {
				err = errors.Join(err, taskErr)
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return changed, true, err
}

// reconcileDuplicateBindings also repairs late target writes after the Moving
// source is gone. UID ordering gives every observation the same winner; readiness
// and a former target hint do not reverse this choice.
func (r *Reconciler) reconcileDuplicateBindings(ctx context.Context, observed map[*agents.Sandbox]sessionbinding.Bindings, decodeErr error) (changed, pending bool, err error) {
	active := make(map[string][]*agents.Sandbox)
	view := make(map[*agents.Sandbox]sessionbinding.Bindings, len(observed))
	for box, records := range observed {
		copy := box.DeepCopy()
		view[copy] = maps.Clone(records)
		for id, record := range records {
			if record.Phase == "" {
				active[id] = append(active[id], copy)
			}
		}
	}
	var ids []string
	for id, members := range active {
		if len(members) > 1 {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return false, false, nil
	}
	if decodeErr != nil {
		return false, true, decodeErr
	}
	sort.Strings(ids)
	for _, id := range ids {
		members := active[id]
		sort.Slice(members, func(i, j int) bool { return members[i].UID < members[j].UID })
		winner := members[0]
		confirmed, patchErr := r.patchBindings(ctx, winner, view[winner])
		if patchErr != nil {
			return changed, true, patchErr
		}
		changed = true
		*winner = *confirmed
		view[winner], err = sessionbinding.Decode(winner.Annotations)
		if err != nil {
			return changed, true, err
		}
		for _, loser := range members[1:] {
			remaining := maps.Clone(view[loser])
			delete(remaining, id)
			updated, patchErr := r.patchBindings(ctx, loser, remaining)
			if patchErr != nil {
				return changed, true, patchErr
			}
			*loser = *updated
			view[loser], err = sessionbinding.Decode(loser.Annotations)
			if err != nil {
				return changed, true, err
			}
		}
	}
	return changed, true, nil
}

func collectBindingMigrations(ctx context.Context, parent *agents.SessionService, snapshot []agents.Sandbox) (map[*agents.Sandbox]sessionbinding.Bindings, []*agents.Sandbox, error) {
	bindings := make(map[*agents.Sandbox]sessionbinding.Bindings)
	var decodeErr error
	var sources []*agents.Sandbox
	for i := range snapshot {
		box := &snapshot[i]
		if !owned(parent, box) {
			continue
		}
		records, e := sessionbinding.Decode(box.Annotations)
		if e != nil {
			logf.FromContext(ctx).Error(e, "cannot inspect member bindings for migration", "sandbox", box.Name)
			decodeErr = fmt.Errorf("decode member %s bindings: %w", box.Name, e)
			continue
		}
		bindings[box] = records
		for _, record := range records {
			if record.Phase == sessionbinding.Moving {
				sources = append(sources, box)
				break
			}
		}
	}
	return bindings, sources, decodeErr
}

// reconcileSourceBindings advances sessions in order using a private member view.
func (r *Reconciler) reconcileSourceBindings(ctx context.Context, parent *agents.SessionService, original *agents.Sandbox, bindings map[*agents.Sandbox]sessionbinding.Bindings) (changed bool, err error) {
	view := make(map[*agents.Sandbox]sessionbinding.Bindings, len(bindings))
	var source *agents.Sandbox
	for box, records := range bindings {
		copy := box.DeepCopy()
		view[copy] = maps.Clone(records)
		if box == original {
			source = copy
		}
	}
	ids := make([]string, 0, len(view[source]))
	for id, record := range view[source] {
		if record.Phase == sessionbinding.Moving {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		migration := bindingMigration{
			reconciler: r, parent: parent, source: source,
			id: id, record: view[source][id], bindings: view,
		}
		taskErr := migration.reconcile(ctx)
		changed = changed || migration.changed
		// A failed decision needs a new informer snapshot for this source.
		// Independent source workers continue, including on RV conflicts.
		if taskErr != nil {
			return changed, taskErr
		}
	}
	return changed, nil
}

// bindingMigration owns one session's progress within its source worker's view.
type bindingMigration struct {
	reconciler      *Reconciler
	parent          *agents.SessionService
	source, target  *agents.Sandbox
	id              string
	record          sessionbinding.Record
	bindings        map[*agents.Sandbox]sessionbinding.Bindings
	changed         bool
	sourceConfirmed bool
}

func (m *bindingMigration) reconcile(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, err := sessionbinding.ExpiresAt(m.record, m.parent.Spec.SessionPolicy.ExpireAfterCreation)
	if err != nil {
		return err
	}
	if (!deadline.IsZero() && !time.Now().Before(deadline)) || !m.source.DeletionTimestamp.IsZero() {
		return nil
	}
	if err := m.ensureTargetRecorded(ctx); err != nil {
		return err
	}
	// No eligible destination is visible; retain the current hint and wait.
	if m.target == nil {
		return nil
	}
	active, err := m.ensureTargetActive(ctx)
	if err != nil || !active {
		return err
	}
	return m.cleanupSource(ctx)
}

func (m *bindingMigration) ensureTargetRecorded(ctx context.Context) error {
	var preferred, active *agents.Sandbox
	canonical := m.source
	for box, records := range m.bindings {
		if box == m.source {
			continue
		}
		if string(box.UID) == m.record.TargetSandboxUID {
			preferred = box
		}
		if other, exists := records[m.id]; exists {
			if other.Phase == sessionbinding.Moving {
				// Duplicate Active copies can independently become Moving through
				// partial Gateway observations. Only the lowest-UID source advances
				// while no Active destination exists; retain the other journals.
				if box.UID < canonical.UID {
					canonical = box
				}
				continue
			}
			if other.Phase != "" || active != nil {
				return fmt.Errorf("SessionBindingConflict: session %s has conflicting bindings", m.id)
			}
			active = box
		}
	}
	if active == nil && canonical != m.source {
		return nil
	}
	if active != nil {
		// Adopt an observed Active copy before creating another, even if a
		// previous target hint points elsewhere or this member is no longer Ready.
		m.target = active
	} else if preferred != nil && m.canAcceptBinding(preferred) {
		m.target = preferred
	} else {
		var candidates []*agents.Sandbox
		for box := range m.bindings {
			if box != m.source && m.canAcceptBinding(box) {
				candidates = append(candidates, box)
			}
		}
		sort.Slice(candidates, func(i, j int) bool {
			if len(m.bindings[candidates[i]]) != len(m.bindings[candidates[j]]) {
				return len(m.bindings[candidates[i]]) < len(m.bindings[candidates[j]])
			}
			return candidates[i].UID < candidates[j].UID
		})
		if len(candidates) == 0 {
			return nil
		}
		m.target = candidates[0]
	}
	if string(m.target.UID) == m.record.TargetSandboxUID {
		return nil
	}
	// This is a recovery preference, not an exclusive reservation. A late old
	// target write can temporarily duplicate Active bindings; repair converges it.
	m.record.TargetSandboxUID = string(m.target.UID)
	next := maps.Clone(m.bindings[m.source])
	next[m.id] = m.record
	return m.patchAndObserve(ctx, m.source, next)
}

func (m *bindingMigration) canAcceptBinding(box *agents.Sandbox) bool {
	// A Moving copy is another source journal, never a destination to overwrite.
	if _, exists := m.bindings[box][m.id]; exists {
		return false
	}
	if !bindingTargetReady(m.parent, box) {
		return false
	}
	limit := sessionbinding.MaxRecords
	if m.parent.Spec.SessionPolicy.MaxSessionsPerSandbox != nil {
		limit = int(*m.parent.Spec.SessionPolicy.MaxSessionsPerSandbox)
	}
	if len(m.bindings[box]) >= limit {
		return false
	}
	next := maps.Clone(m.bindings[box])
	next[m.id] = sessionbinding.Record{CreatedAt: m.record.CreatedAt}
	_, err := sessionbinding.Encode(box.Annotations, next)
	return err == nil
}

func (m *bindingMigration) ensureTargetActive(ctx context.Context) (bool, error) {
	if _, exists := m.bindings[m.target][m.id]; exists {
		// Confirm the observed target against the API with the same UID/RV before
		// removing its source, regardless of the target's current readiness.
		// No APIReader or uncached business Get is used.
		if err := m.patchAndObserve(ctx, m.target, m.bindings[m.target]); err != nil {
			return false, err
		}
		return true, nil
	}
	if !bindingTargetReady(m.parent, m.target) {
		return false, nil
	}
	limit := sessionbinding.MaxRecords
	if m.parent.Spec.SessionPolicy.MaxSessionsPerSandbox != nil {
		limit = int(*m.parent.Spec.SessionPolicy.MaxSessionsPerSandbox)
	}
	next := maps.Clone(m.bindings[m.target])
	next[m.id] = sessionbinding.Record{CreatedAt: m.record.CreatedAt}
	if len(m.bindings[m.target]) >= limit {
		// Capacity is checked locally, not by the API server. This does not
		// prove that an earlier target write failed; the next observation may
		// reuse it, reschedule, or repair a late duplicate.
		return false, nil
	}
	if _, err := sessionbinding.Encode(m.target.Annotations, next); err != nil {
		return false, nil
	}
	// Resume the persisted destination after restart or leader handoff.
	// Success changes the target RV, rejecting older writes to that snapshot.
	// Conflict may mean a previous write succeeded; observe again rather than
	// selecting another target. Unknown outcomes likewise retain the intent.
	if !m.sourceConfirmed {
		if err := m.patchAndObserve(ctx, m.source, m.bindings[m.source]); err != nil {
			return false, err
		}
	}
	if err := m.patchAndObserve(ctx, m.target, next); err != nil {
		return false, err
	}
	return true, nil
}

func (m *bindingMigration) cleanupSource(ctx context.Context) error {
	remaining := maps.Clone(m.bindings[m.source])
	delete(remaining, m.id)
	return m.patchAndObserve(ctx, m.source, remaining)
}

// patchAndObserve advances only this worker's private view from the complete API
// response. Partial success remains progress even if a later migration stage fails.
func (m *bindingMigration) patchAndObserve(ctx context.Context, box *agents.Sandbox, records sessionbinding.Bindings) error {
	updated, err := m.reconciler.patchBindings(ctx, box, records)
	if err != nil {
		return err
	}
	m.changed = true
	if box == m.source {
		m.sourceConfirmed = true
	}
	*box = *updated
	m.bindings[box], err = sessionbinding.Decode(box.Annotations)
	return err
}

func bindingTargetReady(parent *agents.SessionService, box *agents.Sandbox) bool {
	if !effectiveMember(parent, box) || !readyMember(box) || box.Status.PodInfo.PodIP == "" {
		return false
	}
	state, _ := utils.GetSandboxState(box)
	return state == agents.SandboxStateRunning
}

// patchBindings changes only the binding annotation and uses the decision's
// captured UID/RV. The returned API response confirms this particular snapshot;
// later reconciliation still rereads rather than replaying a captured patch.
func (r *Reconciler) patchBindings(ctx context.Context, box *agents.Sandbox, records sessionbinding.Bindings) (*agents.Sandbox, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	annotations, err := sessionbinding.Encode(box.Annotations, records)
	if err != nil {
		return nil, fmt.Errorf("encode member %s bindings: %w", box.Name, err)
	}
	raw, err := json.Marshal(map[string]any{"metadata": map[string]any{"uid": box.UID, "resourceVersion": box.ResourceVersion, "annotations": map[string]string{sessionbinding.BindingsAnnotation: annotations[sessionbinding.BindingsAnnotation]}}})
	if err != nil {
		return nil, fmt.Errorf("marshal binding patch: %w", err)
	}
	updated := box.DeepCopy()
	if err := r.Patch(ctx, updated, client.RawPatch(types.MergePatchType, raw)); err != nil {
		return nil, fmt.Errorf("patch member %s bindings: %w", box.Name, err)
	}
	return updated, nil
}
