package main

import (
	"context"
	"fmt"
	"math"
	"sync"
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
	"k8s.io/client-go/tools/cache"
)

const (
	rolloutGroup            = "argoproj.io"
	rolloutVersionPreferred = "v1alpha1"
	rolloutResource         = "rollouts"
	rolloutListPreflightTO  = 10 * time.Second
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

var (
	rolloutSupportMu sync.RWMutex
	rolloutClient    dynamic.Interface
	rolloutEnabled   bool // informer handlers active
	rolloutKnown     bool // CRD discovered; GC must not treat Rollouts as absent
	rolloutStore     cache.Store
	activeRolloutGVR = schema.GroupVersionResource{
		Group:    rolloutGroup,
		Version:  rolloutVersionPreferred,
		Resource: rolloutResource,
	}
)

func setRolloutSupport(client dynamic.Interface, enabled bool) {
	rolloutSupportMu.Lock()
	defer rolloutSupportMu.Unlock()
	if !enabled {
		rolloutClient = nil
		rolloutEnabled = false
		rolloutKnown = false
		rolloutStore = nil
		return
	}
	rolloutClient = client
	rolloutEnabled = true
	rolloutKnown = true
}

// setRolloutCRDKnown records that the Rollouts API exists but leaves the informer off
// (e.g. preflight list failed). GC and same-name checks can still use the client.
func setRolloutCRDKnown(client dynamic.Interface) {
	rolloutSupportMu.Lock()
	defer rolloutSupportMu.Unlock()
	rolloutClient = client
	rolloutKnown = true
	rolloutEnabled = false
	rolloutStore = nil
}

// disableRolloutInformer keeps the CRD "known" for GC/collision lookups but stops
// treating the informer as active (e.g. after a sync timeout).
func disableRolloutInformer() {
	rolloutSupportMu.Lock()
	defer rolloutSupportMu.Unlock()
	rolloutEnabled = false
	rolloutStore = nil
}

func setRolloutStore(store cache.Store) {
	rolloutSupportMu.Lock()
	defer rolloutSupportMu.Unlock()
	rolloutStore = store
}

// rolloutExistsInStore reports (exists, storeAvailable). When storeAvailable is false,
// callers should fall back to a direct API get.
func rolloutExistsInStore(namespace, name string) (exists bool, storeAvailable bool) {
	rolloutSupportMu.RLock()
	store := rolloutStore
	rolloutSupportMu.RUnlock()
	if store == nil {
		return false, false
	}
	_, exists, err := store.GetByKey(namespace + "/" + name)
	if err != nil {
		return false, true
	}
	return exists, true
}

// clearRolloutSupportIfAPIGone disables Rollout support when the API group/version
// is gone (CRD uninstalled). Object-level NotFound must not clear support.
func clearRolloutSupportIfAPIGone(err error) {
	if err == nil || !meta.IsNoMatchError(err) {
		return
	}
	logWarnf("Argo Rollouts API no longer available (%v); disabling Rollout PDB management", err)
	setRolloutSupport(nil, false)
}

func rolloutInformerEnabled() bool {
	rolloutSupportMu.RLock()
	defer rolloutSupportMu.RUnlock()
	return rolloutEnabled
}

func rolloutCRDKnown() bool {
	rolloutSupportMu.RLock()
	defer rolloutSupportMu.RUnlock()
	return rolloutKnown
}

func getRolloutClient() (dynamic.Interface, bool) {
	rolloutSupportMu.RLock()
	defer rolloutSupportMu.RUnlock()
	if rolloutClient == nil || !rolloutKnown {
		return nil, false
	}
	return rolloutClient, true
}

func getRolloutGVR() schema.GroupVersionResource {
	rolloutSupportMu.RLock()
	defer rolloutSupportMu.RUnlock()
	return activeRolloutGVR
}

func setRolloutGVR(gvr schema.GroupVersionResource) {
	rolloutSupportMu.Lock()
	defer rolloutSupportMu.Unlock()
	activeRolloutGVR = gvr
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

// maybeStartRolloutInformer enables Rollout watches when the CRD is present and listable.
// On sync failure the caller should stop the returned cancel and call disableRolloutInformer
// so GC does not treat Rollouts as absent while the CRD remains installed.
func maybeStartRolloutInformer(ctx context.Context, clientset kubernetes.Interface, dynClient dynamic.Interface) (cache.InformerSynced, context.CancelFunc) {
	gvr, available, err := discoverRolloutGVR(clientset.Discovery())
	if err != nil {
		logWarnf("Failed to discover Argo Rollouts API (%s, resource=%s): %v; skipping Rollout PDB management",
			rolloutGroup, rolloutResource, err)
		setRolloutSupport(nil, false)
		return nil, nil
	}
	if !available {
		logInfof("Argo Rollouts API (%s, resource=%s) not found; skipping Rollout PDB management",
			rolloutGroup, rolloutResource)
		setRolloutSupport(nil, false)
		return nil, nil
	}
	setRolloutGVR(gvr)

	listCtx, cancel := context.WithTimeout(ctx, rolloutListPreflightTO)
	defer cancel()
	if _, err := dynClient.Resource(gvr).Namespace(metav1.NamespaceAll).List(listCtx, metav1.ListOptions{Limit: 1}); err != nil {
		// CRD is installed but list failed (RBAC, timeout). Keep it known so GC/collision
		// checks do not treat Rollouts as absent; leave the informer disabled.
		setRolloutCRDKnown(dynClient)
		logWarnf("Argo Rollouts API %s is present but listing Rollouts failed (%v); CRD marked known for GC/collision checks, informer disabled so Deployment/StatefulSet management can proceed",
			gvr.String(), err)
		return nil, nil
	}

	rolloutCtx, stopRollouts := context.WithCancel(ctx)
	setRolloutSupport(dynClient, true)
	logInfof("Argo Rollouts API detected (%s); enabling Rollout PDB management", gvr.String())

	factory := dynamicinformer.NewDynamicSharedInformerFactory(dynClient, 10*time.Minute)
	informer := factory.ForResource(gvr).Informer()
	setRolloutStore(informer.GetStore())
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if !rolloutInformerEnabled() {
				return
			}
			rw, err := rolloutFromInformerObj(obj)
			if err != nil {
				logWarnf("Failed to parse Rollout on add: %v", err)
				return
			}
			createPDBForWorkload(rolloutCtx, clientset, rw)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			if !rolloutInformerEnabled() {
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
			handleWorkloadUpdate(rolloutCtx, clientset, oldRW, newRW)
		},
		DeleteFunc: func(obj interface{}) {
			if !rolloutInformerEnabled() {
				return
			}
			rw, err := rolloutFromInformerObj(obj)
			if err != nil {
				logWarnf("Failed to parse Rollout on delete: %v", err)
				return
			}
			deletePDBForWorkload(rolloutCtx, clientset, rw)
		},
	})
	factory.Start(rolloutCtx.Done())
	return informer.HasSynced, stopRollouts
}

func listRollouts(ctx context.Context, namespace string) ([]*rolloutWorkload, error) {
	dyn, ok := getRolloutClient()
	if !ok {
		return nil, nil
	}
	gvr := getRolloutGVR()
	list, err := dyn.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		clearRolloutSupportIfAPIGone(err)
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

func getRollout(ctx context.Context, namespace, name string) (*rolloutWorkload, error) {
	dyn, ok := getRolloutClient()
	gvr := getRolloutGVR()
	if !ok {
		return nil, apierrors.NewNotFound(gvr.GroupResource(), name)
	}
	u, err := dyn.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		clearRolloutSupportIfAPIGone(err)
		return nil, err
	}
	return parseRolloutWorkload(u)
}
