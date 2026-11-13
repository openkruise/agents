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

package sandboxserving

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"strconv"
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
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	agents "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/autopause"
	"github.com/openkruise/agents/pkg/discovery"
	sessionbinding "github.com/openkruise/agents/pkg/servingsession/binding"
	"github.com/openkruise/agents/pkg/utils/expectations"
	webhookutils "github.com/openkruise/agents/pkg/webhook/utils"
)

var controllerKind = agents.GroupVersion.WithKind("SandboxServing")

const observationDelay = time.Second

// Add registers the SandboxServing controller when its API is available.
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
	// Both maps are scoped to this reconciler. Queue serialization protects one
	// Serving key; sync.Map also permits workers to reconcile different Servings.
	servingUIDs sync.Map // namespace/name -> types.UID
	deleteUIDs  sync.Map // expectation key + NUL + member name -> types.UID
}

func NewReconciler(c client.Client) *Reconciler {
	return &Reconciler{
		Client:       c,
		Expectations: expectations.NewScaleExpectations(),
	}
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("sandboxserving").
		For(&agents.SandboxServing{}).Owns(&agents.Sandbox{}).
		Owns(&appsv1.ControllerRevision{}).Complete(r)
}

// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxservings,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxservings/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxservings/finalizers,verbs=update
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxservings/scale,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxes,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=apps,resources=controllerrevisions,verbs=get;list;watch;create

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	parent := &agents.SandboxServing{}
	if err := r.Get(ctx, req.NamespacedName, parent); err != nil {
		if apierrors.IsNotFound(err) {
			r.forgetServing(req.Namespace + "/" + req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	r.trackServing(parent)
	if !parent.DeletionTimestamp.IsZero() {
		// Owner references delegate member deletion to Kubernetes garbage collection.
		r.forgetServing(req.Namespace + "/" + req.Name)
		return ctrl.Result{}, nil
	}
	log := logf.FromContext(ctx).WithValues("serving", req.NamespacedName, "servingUID", parent.UID)
	ctx = logf.IntoContext(ctx, log)
	var snapshot agents.SandboxList
	if err := r.List(ctx, &snapshot, client.InNamespace(parent.Namespace), client.MatchingLabels{sessionbinding.ServingUIDLabel: string(parent.UID)}); err != nil {
		return ctrl.Result{}, fmt.Errorf("list Serving members: %w", err)
	}
	status := parent.Status.DeepCopy()
	status.ObservedGeneration = parent.Generation
	status.Selector = labels.Set{sessionbinding.ServingUIDLabel: string(parent.UID)}.AsSelector().String()
	status.Replicas = 0
	status.ReadyReplicas = 0
	status.ReplicasByRevision = map[string]int32{}
	var active []*agents.Sandbox
	for i := range snapshot.Items {
		sbx := &snapshot.Items[i]
		if !effectiveMember(parent, sbx) {
			continue
		}
		active = append(active, sbx)
		status.Replicas++
		if sbx.Status.Phase == agents.SandboxRunning && meta.IsStatusConditionTrue(sbx.Status.Conditions, string(agents.SandboxConditionReady)) {
			status.ReadyReplicas++
		}
		status.ReplicasByRevision[sbx.Labels[sessionbinding.ServingRevisionLabel]]++
	}
	setCondition(status, parent, "ScaleDownBlocked", false, "NotBlocked", "No binding blocks scale-down")
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
	status.CurrentRevision = revision
	status.UpdateRevision = revision
	setCondition(status, parent, "Supported", true, "Supported", "Fixed-replica template provisioning is supported")
	r.observeExpectations(ctx, parent, snapshot.Items)
	if satisfied, elapsed, _ := r.Expectations.SatisfiedExpectations(expectationKey(parent)); !satisfied {
		if elapsed >= expectations.ExpectationTimeout {
			if err := r.recoverExpectations(ctx, parent, revision); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: observationDelay}, r.updateStatus(ctx, parent, status)
	}
	desired := int32(1)
	if parent.Spec.Replicas != nil {
		desired = *parent.Spec.Replicas
	}
	switch {
	case status.Replicas < desired:
		if err := r.scaleUp(ctx, parent, revision, snapshot.Items, min(desired-status.Replicas, 16)); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return ctrl.Result{RequeueAfter: observationDelay}, r.updateStatus(ctx, parent, status)
			}
			return ctrl.Result{}, err
		}
	case status.Replicas > desired:
		remaining, err := r.scaleDown(ctx, parent, active, status.Replicas-desired)
		if err != nil {
			if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
				log.Info("member delete requires fresh cached judgment", "error", err)
				return ctrl.Result{RequeueAfter: observationDelay}, r.updateStatus(ctx, parent, status)
			}
			return ctrl.Result{}, err
		}
		if remaining > 0 {
			setCondition(status, parent, "ScaleDownBlocked", true, "BindingsPresentOrInvalid", "Remaining members have occupied or invalid bindings")
		}
	}
	if err := r.updateStatus(ctx, parent, status); err != nil {
		return ctrl.Result{}, err
	}
	if satisfied, _, _ := r.Expectations.SatisfiedExpectations(expectationKey(parent)); !satisfied {
		return ctrl.Result{RequeueAfter: observationDelay}, nil
	}
	return ctrl.Result{}, nil
}

// scaleUp submits at most count creations from the current member snapshot.
func (r *Reconciler) scaleUp(ctx context.Context, parent *agents.SandboxServing, revision string, snapshot []agents.Sandbox, count int32) error {
	next := nextMemberIndex(parent, snapshot)
	for i := int32(0); i < count; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		sbx := newMember(parent, revision, next+int64(i))
		r.Expectations.ExpectScale(expectationKey(parent), expectations.Create, sbx.Name)
		if err := r.Create(ctx, sbx); err != nil {
			r.Expectations.ObserveScale(expectationKey(parent), expectations.Create, sbx.Name)
			return fmt.Errorf("create Serving member %s: %w", sbx.Name, err)
		}
		logf.FromContext(ctx).Info("created Serving member", "sandbox", sbx.Name, "sandboxUID", sbx.UID, "revision", revision)
	}
	return nil
}

// scaleDown deletes only empty effective members from the caller's snapshot.
// Remaining capacity is reported to Reconcile; this helper does not update status.
func (r *Reconciler) scaleDown(ctx context.Context, parent *agents.SandboxServing, active []*agents.Sandbox, remaining int32) (int32, error) {
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
		uid, rv := sbx.UID, sbx.ResourceVersion
		r.deleteUIDs.Store(expectationKey(parent)+"\x00"+sbx.Name, uid)
		r.Expectations.ExpectScale(expectationKey(parent), expectations.Delete, sbx.Name)
		logf.FromContext(ctx).Info("deleting empty Serving member", "sandbox", sbx.Name, "sandboxUID", uid, "resourceVersion", rv)
		err := r.Delete(ctx, sbx, client.Preconditions{UID: &uid, ResourceVersion: &rv})
		if err != nil {
			r.deleteUIDs.Delete(expectationKey(parent) + "\x00" + sbx.Name)
			r.Expectations.ObserveScale(expectationKey(parent), expectations.Delete, sbx.Name)
			return remaining, fmt.Errorf("delete empty Serving member %s: %w", sbx.Name, err)
		}
		remaining--
	}
	return remaining, nil
}

func unsupported(parent *agents.SandboxServing) string {
	if parent.Spec.Provision.SandboxTemplate == nil || parent.Spec.Provision.SandboxSetRef != nil {
		return "Only direct sandboxTemplate provisioning is implemented"
	}
	template := parent.Spec.Provision.SandboxTemplate
	if template.Template == nil {
		return "sandboxTemplate.template is required"
	}
	if template.Backend != "" && template.Backend != "k8s" {
		return "Only the k8s template backend is supported"
	}
	if template.BuildOptions != nil {
		return "buildOptions is not supported by SandboxServing"
	}
	if errs := validateTemplate(template); len(errs) > 0 {
		return errs.ToAggregate().Error()
	}
	if parent.Spec.SessionPolicy.PauseAfterIdle != "" || parent.Spec.SessionPolicy.ExpireAfterIdle != "" {
		return "Idle pause and expiry are not implemented"
	}
	return ""
}

func validateTemplate(template *agents.SandboxTemplateSpec) field.ErrorList {
	path := field.NewPath("spec", "provision", "sandboxTemplate")
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

func owned(parent *agents.SandboxServing, obj metav1.Object) bool {
	owner := metav1.GetControllerOf(obj)
	return obj.GetNamespace() == parent.Namespace &&
		owner != nil &&
		owner.UID == parent.UID &&
		owner.Name == parent.Name &&
		owner.Kind == controllerKind.Kind &&
		owner.APIVersion == controllerKind.GroupVersion().String() &&
		obj.GetLabels()[sessionbinding.ServingUIDLabel] == string(parent.UID)
}

func effectiveMember(parent *agents.SandboxServing, sbx *agents.Sandbox) bool {
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

func expectationKey(parent *agents.SandboxServing) string {
	return parent.Namespace + "/" + parent.Name + "/" + string(parent.UID)
}

func (r *Reconciler) observeExpectations(ctx context.Context, parent *agents.SandboxServing, snapshot []agents.Sandbox) {
	key := expectationKey(parent)
	seen := map[string]*agents.Sandbox{}
	for i := range snapshot {
		seen[snapshot[i].Name] = &snapshot[i]
		r.Expectations.ObserveScale(key, expectations.Create, snapshot[i].Name)
	}
	for name := range r.Expectations.GetExpectations(key)[expectations.Delete] {
		uid, known := r.deleteUIDs.Load(key + "\x00" + name)
		if sbx := seen[name]; sbx == nil || !sbx.DeletionTimestamp.IsZero() || !owned(parent, sbx) || (known && sbx.UID != uid.(types.UID)) {
			var observedUID types.UID
			if sbx != nil {
				observedUID = sbx.UID
			}
			logf.FromContext(ctx).Info("observed completed Serving deletion", "sandbox", name, "deletedUID", uid, "observedUID", observedUID)
			r.Expectations.ObserveScale(key, expectations.Delete, name)
			r.deleteUIDs.Delete(key + "\x00" + name)
		}
	}
}

// recoverExpectations never converts missing cached creates into a fresh scale
// budget. Retrying only submitted names is safe even with a partial cache: an
// existing but unseen member returns AlreadyExists instead of adding a replica.
func (r *Reconciler) recoverExpectations(ctx context.Context, parent *agents.SandboxServing, revision string) error {
	key := expectationKey(parent)
	pending := r.Expectations.GetExpectations(key)
	for _, name := range pending[expectations.Create].List() {
		if err := ctx.Err(); err != nil {
			return err
		}
		sbx := newMember(parent, revision, 0)
		sbx.Name = name
		if err := r.Create(ctx, sbx); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("recover unobserved Serving creation %s: %w", name, err)
		}
		logf.FromContext(ctx).Info("retried unobserved Serving creation", "sandbox", name, "sandboxUID", sbx.UID)
	}
	// A timed-out delete grants no new deletion permission. Drop its wait and
	// let the next reconciliation recheck the complete cached UID/RV/bindings.
	for name := range pending[expectations.Delete] {
		uid, _ := r.deleteUIDs.Load(key + "\x00" + name)
		logf.FromContext(ctx).Info("expired Serving delete observation; rechecking next snapshot", "sandbox", name, "deletedUID", uid)
		r.deleteUIDs.Delete(key + "\x00" + name)
	}
	r.Expectations.DeleteExpectations(key)
	for name := range pending[expectations.Create] {
		r.Expectations.ExpectScale(key, expectations.Create, name)
	}
	return nil
}

func (r *Reconciler) trackServing(parent metav1.Object) {
	requestKey := parent.GetNamespace() + "/" + parent.GetName()
	if previous, exists := r.servingUIDs.Swap(requestKey, parent.GetUID()); exists && previous.(types.UID) != parent.GetUID() {
		r.clearExpectations(requestKey + "/" + string(previous.(types.UID)))
	}
}

func (r *Reconciler) forgetServing(requestKey string) {
	if uid, exists := r.servingUIDs.LoadAndDelete(requestKey); exists {
		r.clearExpectations(requestKey + "/" + string(uid.(types.UID)))
	}
}

func (r *Reconciler) clearExpectations(key string) {
	r.Expectations.DeleteExpectations(key)
	r.deleteUIDs.Range(func(entry, _ any) bool {
		if strings.HasPrefix(entry.(string), key+"\x00") {
			r.deleteUIDs.Delete(entry)
		}
		return true
	})
}

func memberPrefix(parent *agents.SandboxServing) string {
	name := parent.Name
	if len(name) > 40 {
		name = name[:40]
	}
	hash := sha256.Sum256([]byte(parent.UID))
	return strings.TrimRight(name, "-.") + fmt.Sprintf("-%x-", hash[:4])
}

// Stable names make a repeated create after a lost response an AlreadyExists,
// even when a process restart or cache delay has discarded the expectation.
func nextMemberIndex(parent *agents.SandboxServing, snapshot []agents.Sandbox) int64 {
	prefix := memberPrefix(parent)
	next := int64(0)
	for i := range snapshot {
		if !strings.HasPrefix(snapshot[i].Name, prefix) {
			continue
		}
		index, err := strconv.ParseInt(strings.TrimPrefix(snapshot[i].Name, prefix), 10, 64)
		if err == nil && index >= next {
			next = index + 1
		}
	}
	return next
}

func newMember(parent *agents.SandboxServing, revision string, index int64) *agents.Sandbox {
	creation := parent.Spec.Provision.SandboxTemplate.DeepCopy()
	template := creation.Template
	if template.Labels == nil {
		template.Labels = map[string]string{}
	}
	template.Labels[sessionbinding.ServingNameLabel] = parent.Name
	template.Labels[sessionbinding.ServingUIDLabel] = string(parent.UID)
	template.Labels[sessionbinding.ServingRevisionLabel] = revision
	annotations := maps.Clone(template.Annotations)
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[sessionbinding.BindingsAnnotation] = "{}"
	return &agents.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:            memberPrefix(parent) + strconv.FormatInt(index, 10),
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
			EmbeddedSandboxTemplate: agents.EmbeddedSandboxTemplate{
				Template:             template,
				VolumeClaimTemplates: creation.VolumeClaimTemplates,
			},
		},
	}
}

func (r *Reconciler) ensureRevision(ctx context.Context, parent *agents.SandboxServing) (string, string, error) {
	data, err := json.Marshal(parent.Spec.Provision)
	if err != nil {
		return "", "", fmt.Errorf("encode Serving creation input: %w", err)
	}
	hash := sha256.Sum256(append([]byte(string(parent.UID)+"\x00"), data...))
	name := memberPrefix(parent) + fmt.Sprintf("%x", hash[:6])
	if parent.Status.CurrentRevision != "" && parent.Status.CurrentRevision != name {
		return "", "Template creation input is immutable in this stage", nil
	}
	var revisions appsv1.ControllerRevisionList
	if err := r.List(ctx, &revisions, client.InNamespace(parent.Namespace), client.MatchingLabels{sessionbinding.ServingUIDLabel: string(parent.UID)}); err != nil {
		return "", "", fmt.Errorf("list Serving revisions: %w", err)
	}
	found := false
	for i := range revisions.Items {
		revision := &revisions.Items[i]
		if !owned(parent, revision) {
			continue
		}
		if revision.Name != name || !bytes.Equal(revision.Data.Raw, data) {
			return "", "Template creation input differs from the initial immutable Revision", nil
		}
		found = true
	}
	if found {
		return name, "", nil
	}
	revision := &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: parent.Namespace,
			Labels: map[string]string{
				sessionbinding.ServingUIDLabel:  string(parent.UID),
				sessionbinding.ServingNameLabel: parent.Name,
			},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(parent, controllerKind)},
		},
		Revision: 1,
		Data:     runtime.RawExtension{Raw: data},
	}
	if err := r.Create(ctx, revision); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return "", "", fmt.Errorf("create initial Serving revision: %w", err)
		}
		var existing appsv1.ControllerRevision
		if err := r.Get(ctx, client.ObjectKeyFromObject(revision), &existing); err != nil {
			return "", "", err
		}
		if !owned(parent, &existing) || !bytes.Equal(existing.Data.Raw, data) {
			return "", "Initial Revision name is occupied by different ownership or content", nil
		}
	}
	return name, "", nil
}

func setCondition(status *agents.SandboxServingStatus, parent *agents.SandboxServing, kind string, value bool, reason, message string) {
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

func (r *Reconciler) updateStatus(ctx context.Context, parent *agents.SandboxServing, status *agents.SandboxServingStatus) error {
	if apiequality.Semantic.DeepEqual(parent.Status, *status) {
		return nil
	}
	updated := parent.DeepCopy()
	updated.Status = *status.DeepCopy()
	if err := r.Status().Update(ctx, updated); err != nil {
		return fmt.Errorf("update Serving status: %w", err)
	}
	return nil
}
