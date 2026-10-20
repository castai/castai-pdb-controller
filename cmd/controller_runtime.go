package main

import (
	"context"
	"sync"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	appsv1listers "k8s.io/client-go/listers/apps/v1"
	"k8s.io/client-go/tools/cache"
)

// controllerRuntime holds process dependencies for PDB reconciliation: Deploy/STS
// informer listers and Argo Rollout support state. Constructed once in runController
// (and per-test in unit tests) so helpers do not rely on package-level mutable globals.
type controllerRuntime struct {
	listerMu     sync.RWMutex
	deployLister appsv1listers.DeploymentLister
	stsLister    appsv1listers.StatefulSetLister

	rolloutMu      sync.RWMutex
	rolloutClient  dynamic.Interface
	rolloutEnabled bool // informer handlers active
	rolloutKnown   bool // CRD discovered; GC must not treat Rollouts as absent
	rolloutStore   cache.Store
	rolloutGVR     schema.GroupVersionResource
	rolloutStop    context.CancelFunc // cancels the active Rollout informer context
}

func newControllerRuntime() *controllerRuntime {
	return &controllerRuntime{
		rolloutGVR: schema.GroupVersionResource{
			Group:    rolloutGroup,
			Version:  rolloutVersionPreferred,
			Resource: rolloutResource,
		},
	}
}

func (rt *controllerRuntime) setWorkloadListers(deploy appsv1listers.DeploymentLister, sts appsv1listers.StatefulSetLister) {
	rt.listerMu.Lock()
	defer rt.listerMu.Unlock()
	rt.deployLister = deploy
	rt.stsLister = sts
}

func (rt *controllerRuntime) getDeployLister() appsv1listers.DeploymentLister {
	rt.listerMu.RLock()
	defer rt.listerMu.RUnlock()
	return rt.deployLister
}

func (rt *controllerRuntime) getSTSLister() appsv1listers.StatefulSetLister {
	rt.listerMu.RLock()
	defer rt.listerMu.RUnlock()
	return rt.stsLister
}

// takeRolloutStop clears and returns the informer cancel func (caller invokes it unlocked).
func (rt *controllerRuntime) takeRolloutStopLocked() context.CancelFunc {
	stop := rt.rolloutStop
	rt.rolloutStop = nil
	return stop
}

func (rt *controllerRuntime) setRolloutSupport(client dynamic.Interface, enabled bool) {
	rt.rolloutMu.Lock()
	var stop context.CancelFunc
	if !enabled {
		stop = rt.takeRolloutStopLocked()
		rt.rolloutClient = nil
		rt.rolloutEnabled = false
		rt.rolloutKnown = false
		rt.rolloutStore = nil
		rt.rolloutMu.Unlock()
		if stop != nil {
			stop()
		}
		return
	}
	// Replacing an active informer: cancel the previous one first.
	stop = rt.takeRolloutStopLocked()
	rt.rolloutClient = client
	rt.rolloutEnabled = true
	rt.rolloutKnown = true
	// Drop any prior store; the new informer installs a fresh one via setRolloutStore.
	rt.rolloutStore = nil
	rt.rolloutMu.Unlock()
	if stop != nil {
		stop()
	}
}

// setRolloutInformerStop records the cancel func for the active Rollout informer.
// Any previously registered stop is cancelled first.
func (rt *controllerRuntime) setRolloutInformerStop(stop context.CancelFunc) {
	rt.rolloutMu.Lock()
	prev := rt.takeRolloutStopLocked()
	rt.rolloutStop = stop
	rt.rolloutMu.Unlock()
	if prev != nil {
		prev()
	}
}

// stopRolloutInformer cancels the active Rollout informer if any.
func (rt *controllerRuntime) stopRolloutInformer() {
	rt.rolloutMu.Lock()
	stop := rt.takeRolloutStopLocked()
	rt.rolloutMu.Unlock()
	if stop != nil {
		stop()
	}
}

// setRolloutCRDKnown records that the Rollouts API exists but leaves the informer off
// (e.g. preflight list failed). GC and same-name checks can still use the client.
func (rt *controllerRuntime) setRolloutCRDKnown(client dynamic.Interface) {
	rt.rolloutMu.Lock()
	stop := rt.takeRolloutStopLocked()
	rt.rolloutClient = client
	rt.rolloutKnown = true
	rt.rolloutEnabled = false
	rt.rolloutStore = nil
	rt.rolloutMu.Unlock()
	if stop != nil {
		stop()
	}
}

// disableRolloutInformer keeps the CRD "known" for GC/collision lookups but stops
// treating the informer as active (e.g. after a sync timeout) and cancels its context.
func (rt *controllerRuntime) disableRolloutInformer() {
	rt.rolloutMu.Lock()
	stop := rt.takeRolloutStopLocked()
	rt.rolloutEnabled = false
	rt.rolloutStore = nil
	rt.rolloutMu.Unlock()
	if stop != nil {
		stop()
	}
}

func (rt *controllerRuntime) setRolloutStore(store cache.Store) {
	rt.rolloutMu.Lock()
	defer rt.rolloutMu.Unlock()
	rt.rolloutStore = store
}

// rolloutExistsInStore reports (exists, storeAvailable). Callers may trust a positive
// hit. A miss or storeAvailable=false must fall back to the API — the store can lag
// Deploy/STS handlers during initial Rollout informer sync (including GetByKey errors).
func (rt *controllerRuntime) rolloutExistsInStore(namespace, name string) (exists bool, storeAvailable bool) {
	rt.rolloutMu.RLock()
	defer rt.rolloutMu.RUnlock()
	if rt.rolloutStore == nil {
		return false, false
	}
	_, exists, err := rt.rolloutStore.GetByKey(namespace + "/" + name)
	if err != nil {
		return false, false
	}
	return exists, true
}

// getRolloutFromStore returns a parsed Rollout on a store hit. ok=false means miss,
// unavailable store, or parse failure — callers should fall back to the API.
func (rt *controllerRuntime) getRolloutFromStore(namespace, name string) (rw *rolloutWorkload, ok bool) {
	rt.rolloutMu.RLock()
	defer rt.rolloutMu.RUnlock()
	if rt.rolloutStore == nil {
		return nil, false
	}
	obj, exists, err := rt.rolloutStore.GetByKey(namespace + "/" + name)
	if err != nil || !exists {
		return nil, false
	}
	rw, err = rolloutFromInformerObj(obj)
	if err != nil {
		return nil, false
	}
	return rw, true
}

func (rt *controllerRuntime) rolloutInformerEnabled() bool {
	rt.rolloutMu.RLock()
	defer rt.rolloutMu.RUnlock()
	return rt.rolloutEnabled
}

func (rt *controllerRuntime) rolloutCRDKnown() bool {
	rt.rolloutMu.RLock()
	defer rt.rolloutMu.RUnlock()
	return rt.rolloutKnown
}

func (rt *controllerRuntime) getRolloutClient() (dynamic.Interface, bool) {
	dyn, _, ok := rt.getRolloutClientAndGVR()
	return dyn, ok
}

func (rt *controllerRuntime) getRolloutGVR() schema.GroupVersionResource {
	_, gvr, _ := rt.getRolloutClientAndGVR()
	return gvr
}

// getRolloutClientAndGVR returns the dynamic client and GVR under one lock so
// callers cannot observe a cleared client paired with a stale GVR (or vice versa).
func (rt *controllerRuntime) getRolloutClientAndGVR() (dynamic.Interface, schema.GroupVersionResource, bool) {
	rt.rolloutMu.RLock()
	defer rt.rolloutMu.RUnlock()
	gvr := rt.rolloutGVR
	if rt.rolloutClient == nil || !rt.rolloutKnown {
		return nil, gvr, false
	}
	return rt.rolloutClient, gvr, true
}

func (rt *controllerRuntime) setRolloutGVR(gvr schema.GroupVersionResource) {
	rt.rolloutMu.Lock()
	defer rt.rolloutMu.Unlock()
	rt.rolloutGVR = gvr
}
