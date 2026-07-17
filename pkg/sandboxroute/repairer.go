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

package sandboxroute

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/time/rate"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/metrics"
	"github.com/openkruise/agents/pkg/utils"
)

const (
	DefaultMaintenanceInterval = time.Second
	DefaultRepairWorkers       = 1
	DefaultRepairQPS           = 5
	DefaultRepairBurst         = 1
	DefaultRepairBaseDelay     = 100 * time.Millisecond
	DefaultRepairMaxDelay      = 30 * time.Second
)

const (
	repairLogReasonNone                  = "none"
	repairLogReasonRateLimit             = "rate_limit"
	repairLogReasonObservationGet        = "observation_get"
	repairLogReasonObservationProjection = "observation_projection"
	repairLogReasonInvalidObservation    = "invalid_observation"
)

// ObserveFunc performs one scoped direct read and projection for an ObjectKey.
// The callback must return Present=false for NotFound, deleting, or excluded objects.
type ObserveFunc func(context.Context, types.NamespacedName) (AuthoritativeObservation, error)

// ObservationErrorKind classifies which stage of a direct observation failed.
type ObservationErrorKind string

const (
	ObservationErrorGet        ObservationErrorKind = "get"
	ObservationErrorProjection ObservationErrorKind = "projection"
)

// ObservationError preserves a direct-read or projection error cause.
type ObservationError struct {
	Kind ObservationErrorKind
	Err  error
}

// Error implements error.
func (e *ObservationError) Error() string {
	if e == nil || e.Err == nil {
		return "route observation failed"
	}
	return fmt.Sprintf("route observation %s failed: %v", e.Kind, e.Err)
}

// Unwrap exposes the observation error cause.
func (e *ObservationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// NewGetObservationError classifies a direct-reader failure.
func NewGetObservationError(err error) error {
	if err == nil {
		return nil
	}
	return &ObservationError{Kind: ObservationErrorGet, Err: err}
}

// NewProjectionObservationError classifies a projection or inclusion failure.
func NewProjectionObservationError(err error) error {
	if err == nil {
		return nil
	}
	return &ObservationError{Kind: ObservationErrorProjection, Err: err}
}

// RepairerOptions configures bounded targeted repair execution.
type RepairerOptions struct {
	Workers             int
	QPS                 float64
	Burst               int
	BaseDelay           time.Duration
	MaxDelay            time.Duration
	MaintenanceInterval time.Duration
	RateLimiter         workqueue.TypedRateLimiter[types.NamespacedName]
	Queue               workqueue.TypedRateLimitingInterface[types.NamespacedName]
}

// Repairer deduplicates ambiguous ObjectKeys and applies scoped direct observations.
type Repairer struct {
	store               *Store
	observe             ObserveFunc
	workers             int
	maintenanceInterval time.Duration
	queue               workqueue.TypedRateLimitingInterface[types.NamespacedName]
	requestLimiter      *rate.Limiter

	mu      sync.Mutex
	pending map[types.NamespacedName]uint64
}

// NewRepairer creates a bounded targeted Repairer.
func NewRepairer(store *Store, observe ObserveFunc, options RepairerOptions) (*Repairer, error) {
	if store == nil {
		return nil, errors.New("route Repairer Store must not be nil")
	}
	if observe == nil {
		return nil, errors.New("route Repairer observer must not be nil")
	}

	workers, qps, burst, baseDelay, maxDelay, maintenanceInterval, err := repairerDefaults(options)
	if err != nil {
		return nil, err
	}
	queue := options.Queue
	if queue == nil {
		rateLimiter := options.RateLimiter
		if rateLimiter == nil {
			rateLimiter = workqueue.NewTypedItemExponentialFailureRateLimiter[types.NamespacedName](baseDelay, maxDelay)
		}
		queue = workqueue.NewTypedRateLimitingQueue(rateLimiter)
	}

	repairer := &Repairer{
		store:               store,
		observe:             observe,
		workers:             workers,
		maintenanceInterval: maintenanceInterval,
		queue:               queue,
		requestLimiter:      rate.NewLimiter(rate.Limit(qps), burst),
		pending:             make(map[types.NamespacedName]uint64),
	}
	metrics.SetSandboxRouteRepairQueueDepth(string(store.surface), 0)
	return repairer, nil
}

// Enqueue adds all targeted requests carried by a Store mutation.
func (r *Repairer) Enqueue(result MutationResult) {
	r.EnqueueRequests(result.RepairRequests)
}

// EnqueueRequests adds targeted requests while retaining each ObjectKey's newest generation.
func (r *Repairer) EnqueueRequests(requests []RepairRequest) {
	for _, request := range requests {
		r.EnqueueRequest(request)
	}
}

// EnqueueRequest adds one valid targeted repair request.
func (r *Repairer) EnqueueRequest(request RepairRequest) {
	if r == nil || request.ObjectKey.Namespace == "" || request.ObjectKey.Name == "" || request.Generation == 0 {
		return
	}
	r.mu.Lock()
	current, exists := r.pending[request.ObjectKey]
	if exists && current >= request.Generation {
		r.mu.Unlock()
		return
	}
	r.pending[request.ObjectKey] = request.Generation
	metrics.SetSandboxRouteRepairQueueDepth(string(r.store.surface), len(r.pending))
	r.mu.Unlock()

	r.queue.Add(request.ObjectKey)
}

// Pending returns the number of deduplicated ObjectKeys awaiting completion.
func (r *Repairer) Pending() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending)
}

// Start runs maintenance and fixed-concurrency workers until context cancellation.
func (r *Repairer) Start(ctx context.Context) error {
	var workers sync.WaitGroup
	for index := 0; index < r.workers; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for r.processNext(ctx) {
			}
		}()
	}

	ticker := time.NewTicker(r.maintenanceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.queue.ShutDown()
			workers.Wait()
			r.syncPendingDepth()
			return nil
		case <-ticker.C:
			r.EnqueueRequests(r.store.Maintenance())
		}
	}
}

func (r *Repairer) processNext(ctx context.Context) bool {
	key, shutdown := r.queue.Get()
	if shutdown {
		return false
	}
	defer func() {
		r.queue.Done(key)
	}()

	generation, exists := r.pendingGeneration(key)
	if !exists {
		r.queue.Forget(key)
		return true
	}
	if err := r.requestLimiter.Wait(ctx); err != nil {
		if ctx.Err() != nil {
			r.queue.Forget(key)
			return true
		}
		r.logRepair(ctx, key, generation, RepairResultGetError, repairLogReasonRateLimit, MutationResult{}, true, err)
		r.retry(key, generation)
		return true
	}

	observation, err := r.observe(ctx, key)
	if ctx.Err() != nil {
		r.queue.Forget(key)
		return true
	}
	if err != nil {
		repairResult, reason := classifyObservationError(err)
		r.logRepair(ctx, key, generation, repairResult, reason, MutationResult{}, true, err)
		r.retry(key, generation)
		return true
	}
	if err := validateObservation(key, observation); err != nil {
		r.logRepair(ctx, key, generation, RepairResultProjectionError, repairLogReasonInvalidObservation, MutationResult{}, true, err)
		r.retry(key, generation)
		return true
	}

	result := r.store.ApplyAuthoritativeRepair(
		RepairRequest{ObjectKey: key, Generation: generation},
		observation,
	)
	r.Enqueue(result)
	switch {
	case result.Result == EventResultInvalid:
		r.logRepair(ctx, key, generation, RepairResultProjectionError, normalizedRepairReason(result.Reason), result, true, nil)
		r.retry(key, generation)
	case result.Reason == ReasonStaleRepairGeneration:
		r.logRepair(ctx, key, generation, RepairResultStale, normalizedRepairReason(result.Reason), result, false, nil)
		r.complete(key, generation)
	default:
		r.logRepair(ctx, key, generation, RepairResultSuccess, normalizedRepairReason(result.Reason), result, false, nil)
		r.complete(key, generation)
	}
	return true
}

func (r *Repairer) pendingGeneration(key types.NamespacedName) (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	generation, exists := r.pending[key]
	return generation, exists
}

func (r *Repairer) complete(key types.NamespacedName, generation uint64) {
	r.mu.Lock()
	current, exists := r.pending[key]
	if exists && current == generation {
		delete(r.pending, key)
	}
	newerPending := exists && current > generation
	metrics.SetSandboxRouteRepairQueueDepth(string(r.store.surface), len(r.pending))
	r.mu.Unlock()

	r.queue.Forget(key)
	if newerPending {
		r.queue.Add(key)
	}
}

func (r *Repairer) syncPendingDepth() {
	r.mu.Lock()
	defer r.mu.Unlock()
	metrics.SetSandboxRouteRepairQueueDepth(string(r.store.surface), len(r.pending))
}

func (r *Repairer) retry(key types.NamespacedName, generation uint64) {
	r.mu.Lock()
	current, exists := r.pending[key]
	newerPending := exists && current > generation
	r.mu.Unlock()
	if newerPending {
		r.queue.Forget(key)
		r.queue.Add(key)
		return
	}
	r.queue.AddRateLimited(key)
}

func classifyObservationError(err error) (RepairResult, string) {
	var observationError *ObservationError
	if errors.As(err, &observationError) && observationError.Kind == ObservationErrorProjection {
		return RepairResultProjectionError, repairLogReasonObservationProjection
	}
	return RepairResultGetError, repairLogReasonObservationGet
}

func (r *Repairer) logRepair(
	ctx context.Context,
	key types.NamespacedName,
	generation uint64,
	result RepairResult,
	reason string,
	mutation MutationResult,
	retry bool,
	err error,
) {
	values := []any{
		"surface", r.store.surface,
		"namespace", key.Namespace,
		"name", key.Name,
		"generation", generation,
		"result", result,
		"reason", reason,
		"retry", retry,
	}
	if mutation.Result != "" {
		values = append(values, "mutationResult", mutation.Result, "mutationReason", mutation.Reason)
	}
	log := klog.FromContext(ctx)
	if err != nil {
		log.Error(err, "targeted route repair failed", values...)
		return
	}
	log.V(utils.DebugLogLevel).Info("targeted route repair completed", values...)
}

func normalizedRepairReason(reason Reason) string {
	if reason == ReasonNone {
		return repairLogReasonNone
	}
	return string(reason)
}

func validateObservation(key types.NamespacedName, observation AuthoritativeObservation) error {
	if !observation.Present {
		return nil
	}
	if err := observation.Route.Validate(); err != nil {
		return err
	}
	shape, err := observation.Route.Shape()
	if err != nil {
		return err
	}
	if shape != ShapeFull {
		return errors.New("authoritative route must be full")
	}
	routeKey, _ := observation.Route.ObjectKey()
	if routeKey != key {
		return fmt.Errorf("authoritative route ObjectKey %s does not match requested %s", routeKey, key)
	}
	return nil
}

func repairerDefaults(options RepairerOptions) (int, float64, int, time.Duration, time.Duration, time.Duration, error) {
	if options.Workers < 0 || options.QPS < 0 || options.Burst < 0 || options.BaseDelay < 0 ||
		options.MaxDelay < 0 || options.MaintenanceInterval < 0 {
		return 0, 0, 0, 0, 0, 0, errors.New("route Repairer options must not be negative")
	}
	workers := options.Workers
	if workers == 0 {
		workers = DefaultRepairWorkers
	}
	qps := options.QPS
	if qps == 0 {
		qps = DefaultRepairQPS
	}
	burst := options.Burst
	if burst == 0 {
		burst = DefaultRepairBurst
	}
	baseDelay := options.BaseDelay
	if baseDelay == 0 {
		baseDelay = DefaultRepairBaseDelay
	}
	maxDelay := options.MaxDelay
	if maxDelay == 0 {
		maxDelay = DefaultRepairMaxDelay
	}
	maintenanceInterval := options.MaintenanceInterval
	if maintenanceInterval == 0 {
		maintenanceInterval = DefaultMaintenanceInterval
	}
	if maxDelay < baseDelay {
		return 0, 0, 0, 0, 0, 0, errors.New("route Repairer max delay must not be less than base delay")
	}
	return workers, qps, burst, baseDelay, maxDelay, maintenanceInterval, nil
}
