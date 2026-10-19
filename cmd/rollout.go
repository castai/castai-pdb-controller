package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	rolloutSupportMu        sync.RWMutex
	rolloutSupportAvailable bool
	rolloutDynamicClient    dynamic.Interface
	activeRolloutGVR        = schema.GroupVersionResource{
		Group:    rolloutGroup,
		Version:  rolloutVersionPreferred,
		Resource: rolloutResource,
	}
)

func setRolloutSupport(client dynamic.Interface, available bool) {
	rolloutSupportMu.Lock()
	defer rolloutSupportMu.Unlock()
	rolloutDynamicClient = client
	rolloutSupportAvailable = available
}

func getRolloutSupport() (dynamic.Interface, bool) {
	rolloutSupportMu.RLock()
	defer rolloutSupportMu.RUnlock()
	return rolloutDynamicClient, rolloutSupportAvailable
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

// discoverRolloutGVR finds a usable Rollouts GVR via discovery.
// Prefers argoproj.io/v1alpha1 (current Argo Rollouts), otherwise any argoproj.io version that serves rollouts.
func discoverRolloutGVR(discoveryClient discovery.DiscoveryInterface) (schema.GroupVersionResource, bool, error) {
	preferred := schema.GroupVersionResource{
		Group:    rolloutGroup,
		Version:  rolloutVersionPreferred,
		Resource: rolloutResource,
	}
	if list, err := discoveryClient.ServerResourcesForGroupVersion(preferred.GroupVersion().String()); err == nil {
		for _, r := range list.APIResources {
			if r.Name == rolloutResource {
				return preferred, true, nil
			}
		}
	} else if err != nil && !apierrors.IsNotFound(err) && !discovery.IsGroupDiscoveryFailedError(err) {
		// Fall through to a full scan; only hard-fail if that also fails.
		logDebugf("Preferred Rollout GV %s lookup failed: %v; scanning discovery", preferred.GroupVersion().String(), err)
	}

	_, resourceLists, err := discoveryClient.ServerGroupsAndResources()
	if err != nil && len(resourceLists) == 0 {
		if apierrors.IsNotFound(err) || discovery.IsGroupDiscoveryFailedError(err) {
			return schema.GroupVersionResource{}, false, nil
		}
		return schema.GroupVersionResource{}, false, err
	}

	var fallback schema.GroupVersionResource
	for _, rl := range resourceLists {
		gv, parseErr := schema.ParseGroupVersion(rl.GroupVersion)
		if parseErr != nil || gv.Group != rolloutGroup {
			continue
		}
		for _, r := range rl.APIResources {
			if r.Name != rolloutResource {
				continue
			}
			candidate := schema.GroupVersionResource{Group: gv.Group, Version: gv.Version, Resource: r.Name}
			if gv.Version == rolloutVersionPreferred {
				return candidate, true, nil
			}
			if fallback.Resource == "" {
				fallback = candidate
			}
		}
	}
	if fallback.Resource != "" {
		return fallback, true, nil
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
		r := int32(replicas)
		rw.Replicas = &r
	}

	if selMap, found, err := unstructured.NestedMap(u.Object, "spec", "selector"); err != nil {
		return nil, fmt.Errorf("spec.selector: %w", err)
	} else if found && selMap != nil {
		var sel metav1.LabelSelector
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(selMap, &sel); err != nil {
			return nil, fmt.Errorf("decode spec.selector: %w", err)
		}
		rw.Selector = &sel
	}

	if labels, found, err := unstructured.NestedStringMap(u.Object, "spec", "template", "metadata", "labels"); err != nil {
		return nil, fmt.Errorf("spec.template.metadata.labels: %w", err)
	} else if found {
		rw.PodTemplateLabels = labels
	}

	if ready, found, err := unstructured.NestedInt64(u.Object, "status", "readyReplicas"); err != nil {
		return nil, fmt.Errorf("status.readyReplicas: %w", err)
	} else if found {
		rw.ReadyReplicas = int32(ready)
	}

	if available, found, err := unstructured.NestedInt64(u.Object, "status", "availableReplicas"); err != nil {
		return nil, fmt.Errorf("status.availableReplicas: %w", err)
	} else if found {
		rw.AvailableReplicas = int32(available)
	} else {
		// When available is unset, treat ready as available (same idea as StatefulSets).
		rw.AvailableReplicas = rw.ReadyReplicas
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

// maybeStartRolloutInformer checks for the Rollouts API and, when usable, watches Rollouts
// with the same PDB create/update/delete handlers used for Deployments/StatefulSets.
// On success it returns (HasSynced, stop) where stop cancels the Rollout informer context.
// Callers must invoke stop if they later disable Rollout support (e.g. sync timeout) so
// handlers cannot keep mutating PDBs while GC treats Rollouts as absent.
// A failed list preflight (e.g. missing RBAC) disables Rollouts instead of blocking startup.
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

	// Preflight list: if RBAC/list fails, WaitForCacheSync would hang forever and block
	// Deployment/StatefulSet informer startup. Fail open by skipping Rollouts only.
	listCtx, cancel := context.WithTimeout(ctx, rolloutListPreflightTO)
	defer cancel()
	if _, err := dynClient.Resource(gvr).Namespace(metav1.NamespaceAll).List(listCtx, metav1.ListOptions{Limit: 1}); err != nil {
		logWarnf("Argo Rollouts API %s is present but listing Rollouts failed (%v); skipping Rollout PDB management so Deployment/StatefulSet management can proceed",
			gvr.String(), err)
		setRolloutSupport(nil, false)
		return nil, nil
	}

	// Child context so Rollout informers can be stopped independently of the main controller.
	rolloutCtx, stopRollouts := context.WithCancel(ctx)

	setRolloutSupport(dynClient, true)
	logInfof("Argo Rollouts API detected (%s); enabling Rollout PDB management", gvr.String())

	factory := dynamicinformer.NewDynamicSharedInformerFactory(dynClient, 10*time.Minute)
	informer := factory.ForResource(gvr).Informer()
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if _, ok := getRolloutSupport(); !ok {
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
			if _, ok := getRolloutSupport(); !ok {
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
			if _, ok := getRolloutSupport(); !ok {
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
	dyn, ok := getRolloutSupport()
	if !ok || dyn == nil {
		return nil, nil
	}
	gvr := getRolloutGVR()
	list, err := dyn.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
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
	dyn, ok := getRolloutSupport()
	gvr := getRolloutGVR()
	if !ok || dyn == nil {
		return nil, apierrors.NewNotFound(gvr.GroupResource(), name)
	}
	u, err := dyn.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return parseRolloutWorkload(u)
}
