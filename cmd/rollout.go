package main

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

const (
	rolloutGroup                 = "argoproj.io"
	rolloutVersionPreferred      = "v1alpha1"
	rolloutResource              = "rollouts"
	rolloutListPreflightTO       = 10 * time.Second
	rolloutInformerRetryInterval = 2 * time.Minute
)

// rolloutWorkload is a typed view of an Argo Rollout for PDB management.
// Parsed from unstructured so we do not take a hard dependency on the Rollouts SDK.
type rolloutWorkload struct {
	Namespace         string
	Name              string
	Labels            map[string]string
	Annotations       map[string]string
	Replicas          *int32
	Selector          *metav1.LabelSelector
	PodTemplateLabels map[string]string
	ReadyReplicas     int32
	AvailableReplicas int32
}

func int64ToInt32(v int64, field string) (int32, error) {
	if v < math.MinInt32 || v > math.MaxInt32 {
		return 0, fmt.Errorf("%s value %d overflows int32", field, v)
	}
	return int32(v), nil
}

func discoverRolloutGVR(disco discovery.DiscoveryInterface) (schema.GroupVersionResource, bool, error) {
	groups, err := disco.ServerGroups()
	if err != nil {
		return schema.GroupVersionResource{}, false, err
	}
	var hasGroup bool
	for _, g := range groups.Groups {
		if g.Name == rolloutGroup {
			hasGroup = true
			break
		}
	}
	if !hasGroup {
		return schema.GroupVersionResource{}, false, nil
	}

	preferred := schema.GroupVersionResource{
		Group:    rolloutGroup,
		Version:  rolloutVersionPreferred,
		Resource: rolloutResource,
	}
	if resources, err := disco.ServerResourcesForGroupVersion(preferred.GroupVersion().String()); err == nil {
		for _, r := range resources.APIResources {
			if r.Name == rolloutResource {
				return preferred, true, nil
			}
		}
	}

	// Fallback: any listed version that serves rollouts.
	for _, g := range groups.Groups {
		if g.Name != rolloutGroup {
			continue
		}
		for _, v := range g.Versions {
			gv := schema.GroupVersion{Group: rolloutGroup, Version: v.Version}
			resources, err := disco.ServerResourcesForGroupVersion(gv.String())
			if err != nil {
				continue
			}
			for _, r := range resources.APIResources {
				if r.Name == rolloutResource {
					return schema.GroupVersionResource{
						Group:    rolloutGroup,
						Version:  v.Version,
						Resource: rolloutResource,
					}, true, nil
				}
			}
		}
	}
	return schema.GroupVersionResource{}, false, nil
}

func parseRolloutWorkload(u *unstructured.Unstructured) (*rolloutWorkload, error) {
	if u == nil {
		return nil, fmt.Errorf("nil rollout object")
	}
	rw := &rolloutWorkload{
		Namespace:   u.GetNamespace(),
		Name:        u.GetName(),
		Labels:      u.GetLabels(),
		Annotations: u.GetAnnotations(),
	}

	if replicas, found, err := unstructured.NestedInt64(u.Object, "spec", "replicas"); err != nil {
		return nil, fmt.Errorf("spec.replicas: %w", err)
	} else if found {
		v, err := int64ToInt32(replicas, "spec.replicas")
		if err != nil {
			return nil, err
		}
		rw.Replicas = &v
	}

	if sel, found, err := unstructured.NestedMap(u.Object, "spec", "selector"); err != nil {
		return nil, fmt.Errorf("spec.selector: %w", err)
	} else if found && sel != nil {
		var labelSelector metav1.LabelSelector
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(sel, &labelSelector); err != nil {
			return nil, fmt.Errorf("spec.selector convert: %w", err)
		}
		rw.Selector = &labelSelector
	}

	if labels, found, err := unstructured.NestedStringMap(u.Object, "spec", "template", "metadata", "labels"); err != nil {
		return nil, fmt.Errorf("spec.template.metadata.labels: %w", err)
	} else if found {
		rw.PodTemplateLabels = labels
	}

	if ready, found, err := unstructured.NestedInt64(u.Object, "status", "readyReplicas"); err != nil {
		return nil, fmt.Errorf("status.readyReplicas: %w", err)
	} else if found {
		v, err := int64ToInt32(ready, "status.readyReplicas")
		if err != nil {
			return nil, err
		}
		rw.ReadyReplicas = v
	}
	if available, found, err := unstructured.NestedInt64(u.Object, "status", "availableReplicas"); err != nil {
		return nil, fmt.Errorf("status.availableReplicas: %w", err)
	} else if found {
		v, err := int64ToInt32(available, "status.availableReplicas")
		if err != nil {
			return nil, err
		}
		rw.AvailableReplicas = v
	}

	return rw, nil
}

func rolloutFromInformerObj(obj interface{}) (*rolloutWorkload, error) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, fmt.Errorf("expected *unstructured.Unstructured, got %T", obj)
	}
	return parseRolloutWorkload(u)
}

// clearRolloutSupportIfAPIGone disables Rollout management when the API type itself is gone.
// fromList: List NotFound usually means the resource type/CRD was removed (safe to clear).
// Get NotFound means the object is absent and must not clear support.
func (rt *controllerRuntime) clearRolloutSupportIfAPIGone(err error, fromList bool) {
	if err == nil {
		return
	}
	gone := meta.IsNoMatchError(err)
	if fromList && apierrors.IsNotFound(err) {
		gone = true
	}
	if !gone {
		return
	}
	logWarnf("Argo Rollouts API no longer available (%v); disabling Rollout PDB management", err)
	rt.setRolloutSupport(nil, false)
}

// maybeStartRolloutInformer enables Rollout watches when the CRD is present and listable.
// On sync failure the caller should stop the returned cancel and call disableRolloutInformer
// so GC does not treat Rollouts as absent while the CRD remains installed.
func (rt *controllerRuntime) maybeStartRolloutInformer(ctx context.Context, clientset kubernetes.Interface, dynClient dynamic.Interface) (cache.InformerSynced, context.CancelFunc) {
	gvr, available, err := discoverRolloutGVR(clientset.Discovery())
	if err != nil {
		logWarnf("Failed to discover Argo Rollouts API (%s, resource=%s): %v; skipping Rollout PDB management",
			rolloutGroup, rolloutResource, err)
		// If the CRD was already known, do not clear known on a transient discovery
		// error — that would let GC treat live Rollout PDBs as orphans.
		rt.preserveRolloutKnownOrClear(fmt.Sprintf("discovery error: %v", err))
		return nil, nil
	}
	if !available {
		logInfof("Argo Rollouts API (%s, resource=%s) not found; skipping Rollout PDB management",
			rolloutGroup, rolloutResource)
		// Absence after previously-known: keep known until List NotFound clears support
		// via clearRolloutSupportIfAPIGone (avoids rediscovery flakes orphaning PDBs).
		rt.preserveRolloutKnownOrClear("API not listed by discovery")
		return nil, nil
	}
	rt.setRolloutGVR(gvr)

	listCtx, cancel := context.WithTimeout(ctx, rolloutListPreflightTO)
	defer cancel()
	if _, err := dynClient.Resource(gvr).Namespace(metav1.NamespaceAll).List(listCtx, metav1.ListOptions{Limit: 1}); err != nil {
		// CRD is installed but list failed (RBAC, timeout). Keep it known so GC/collision
		// checks do not treat Rollouts as absent; leave the informer disabled.
		rt.setRolloutCRDKnown(dynClient)
		logWarnf("Argo Rollouts API %s is present but listing Rollouts failed (%v); CRD marked known for GC/collision checks, informer disabled so Deployment/StatefulSet management can proceed",
			gvr.String(), err)
		return nil, nil
	}

	rolloutCtx, stopRollouts := context.WithCancel(ctx)
	rt.setRolloutSupport(dynClient, true)
	rt.setRolloutInformerStop(stopRollouts)
	logInfof("Argo Rollouts API detected (%s); enabling Rollout PDB management", gvr.String())

	factory := dynamicinformer.NewDynamicSharedInformerFactory(dynClient, 10*time.Minute)
	informer := factory.ForResource(gvr).Informer()
	rt.setRolloutStore(informer.GetStore())
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if !rt.rolloutInformerEnabled() {
				return
			}
			rw, err := rolloutFromInformerObj(obj)
			if err != nil {
				logWarnf("Failed to parse Rollout on add: %v", err)
				return
			}
			createPDBForWorkload(rolloutCtx, clientset, rt, rw)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			if !rt.rolloutInformerEnabled() {
				return
			}
			oldRW, err := rolloutFromInformerObj(oldObj)
			if err != nil {
				logWarnf("Failed to parse old Rollout on update: %v", err)
				return
			}
			newRW, err := rolloutFromInformerObj(newObj)
			if err != nil {
				logWarnf("Failed to parse new Rollout on update: %v", err)
				return
			}
			handleWorkloadUpdate(rolloutCtx, clientset, rt, oldRW, newRW)
		},
		DeleteFunc: func(obj interface{}) {
			if !rt.rolloutInformerEnabled() {
				return
			}
			rw, err := rolloutFromInformerObj(obj)
			if err != nil {
				logWarnf("Failed to parse Rollout on delete: %v", err)
				return
			}
			deletePDBForWorkload(rolloutCtx, clientset, rt, rw)
		},
	})
	factory.Start(rolloutCtx.Done())
	return informer.HasSynced, stopRollouts
}

// rolloutInformerRetryWait returns the base retry interval plus up to 50% jitter so
// controllers that restart together do not stampede discovery/list in sync.
func rolloutInformerRetryWait() time.Duration {
	jitter := time.Duration(rand.Int63n(int64(rolloutInformerRetryInterval/2) + 1))
	return rolloutInformerRetryInterval + jitter
}

// retryRolloutInformerPeriodically keeps trying to enable the Rollout informer until
// it is active or ctx is cancelled. Covers: CRD installed after controller start,
// preflight list failure, and initial sync timeout. Tries immediately, then waits
// rolloutInformerRetryInterval plus jitter between attempts.
func (rt *controllerRuntime) retryRolloutInformerPeriodically(ctx context.Context, clientset kubernetes.Interface, restConfig *rest.Config, setStop func(context.CancelFunc)) {
	if restConfig == nil {
		return
	}
	attempt := func() bool {
		if rt.rolloutInformerEnabled() {
			return true
		}
		dynClient, err := dynamic.NewForConfig(restConfig)
		if err != nil {
			logWarnf("Rollout informer retry: dynamic client failed: %v", err)
			return false
		}
		synced, stop := rt.maybeStartRolloutInformer(ctx, clientset, dynClient)
		if synced == nil {
			if stop != nil {
				stop()
			}
			// CRD still absent, or present but not listable — try again later.
			return false
		}
		syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		ok := cache.WaitForCacheSync(syncCtx.Done(), synced)
		cancel()
		if !ok {
			logWarnf("Rollout informer retry: sync timed out; will try again with jittered backoff")
			// disableRolloutInformer cancels the informer context registered on rt.
			rt.disableRolloutInformer()
			return false
		}
		if setStop != nil {
			setStop(stop)
		}
		logInfof("Rollout informer recovered after retry; Rollout PDB management active")
		return true
	}

	// Immediate first attempt so a transient startup sync failure does not wait a full interval.
	if attempt() {
		return
	}

	for {
		wait := rolloutInformerRetryWait()
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			if attempt() {
				return
			}
		}
	}
}

func (rt *controllerRuntime) listRollouts(ctx context.Context, namespace string) ([]*rolloutWorkload, error) {
	dyn, gvr, ok := rt.getRolloutClientAndGVR()
	if !ok {
		return nil, nil
	}
	list, err := dyn.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		rt.clearRolloutSupportIfAPIGone(err, true)
		return nil, err
	}
	out := make([]*rolloutWorkload, 0, len(list.Items))
	for i := range list.Items {
		rw, err := parseRolloutWorkload(&list.Items[i])
		if err != nil {
			logWarnf("Failed to parse Rollout %s/%s: %v", list.Items[i].GetNamespace(), list.Items[i].GetName(), err)
			continue
		}
		out = append(out, rw)
	}
	return out, nil
}

func (rt *controllerRuntime) getRollout(ctx context.Context, namespace, name string) (*rolloutWorkload, error) {
	dyn, gvr, ok := rt.getRolloutClientAndGVR()
	if !ok {
		return nil, apierrors.NewNotFound(gvr.GroupResource(), name)
	}
	u, err := dyn.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		// Object NotFound must not disable CRD support; only type-level NoMatch does.
		rt.clearRolloutSupportIfAPIGone(err, false)
		return nil, err
	}
	return parseRolloutWorkload(u)
}
