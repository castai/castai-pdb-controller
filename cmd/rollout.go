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
	rolloutGroup    = "argoproj.io"
	rolloutVersion  = "v1alpha1"
	rolloutResource = "rollouts"
)

var rolloutGVR = schema.GroupVersionResource{
	Group:    rolloutGroup,
	Version:  rolloutVersion,
	Resource: rolloutResource,
}

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

// rolloutsCRDAvailable reports whether the Argo Rollouts API is present on the cluster.
// Missing CRD is not an error — callers should skip Rollout management and log.
func rolloutsCRDAvailable(discoveryClient discovery.DiscoveryInterface) (bool, error) {
	gv := rolloutGroup + "/" + rolloutVersion
	list, err := discoveryClient.ServerResourcesForGroupVersion(gv)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		// Aggregated discovery can report partial group failures when the API is absent.
		if discovery.IsGroupDiscoveryFailedError(err) {
			return false, nil
		}
		return false, err
	}
	for _, r := range list.APIResources {
		if r.Name == rolloutResource {
			return true, nil
		}
	}
	return false, nil
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

// maybeStartRolloutInformer checks for the Rollouts CRD and, when present, watches Rollouts
// with the same PDB create/update/delete handlers used for Deployments/StatefulSets.
// Returns the informer's HasSynced func when started, or nil when Rollouts are unavailable.
func maybeStartRolloutInformer(ctx context.Context, clientset *kubernetes.Clientset, dynClient dynamic.Interface) cache.InformerSynced {
	available, err := rolloutsCRDAvailable(clientset.Discovery())
	if err != nil {
		logWarnf("Failed to check for Argo Rollouts CRD (%s/%s, resource=%s): %v; skipping Rollout PDB management",
			rolloutGroup, rolloutVersion, rolloutResource, err)
		setRolloutSupport(nil, false)
		return nil
	}
	if !available {
		logInfof("Argo Rollouts CRD (%s/%s, resource=%s) not found; skipping Rollout PDB management",
			rolloutGroup, rolloutVersion, rolloutResource)
		setRolloutSupport(nil, false)
		return nil
	}

	setRolloutSupport(dynClient, true)
	logInfof("Argo Rollouts CRD detected; enabling Rollout PDB management")

	factory := dynamicinformer.NewDynamicSharedInformerFactory(dynClient, 10*time.Minute)
	informer := factory.ForResource(rolloutGVR).Informer()
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			rw, err := rolloutFromInformerObj(obj)
			if err != nil {
				logWarnf("Failed to parse Rollout on add: %v", err)
				return
			}
			createPDBForWorkload(ctx, clientset, rw)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
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
			handleWorkloadUpdate(ctx, clientset, oldRW, newRW)
		},
		DeleteFunc: func(obj interface{}) {
			rw, err := rolloutFromInformerObj(obj)
			if err != nil {
				logWarnf("Failed to parse Rollout on delete: %v", err)
				return
			}
			deletePDBForWorkload(ctx, clientset, rw)
		},
	})
	factory.Start(ctx.Done())
	return informer.HasSynced
}

func listRollouts(ctx context.Context, namespace string) ([]*rolloutWorkload, error) {
	dyn, ok := getRolloutSupport()
	if !ok || dyn == nil {
		return nil, nil
	}
	list, err := dyn.Resource(rolloutGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
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
	if !ok || dyn == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: rolloutGroup, Resource: rolloutResource}, name)
	}
	u, err := dyn.Resource(rolloutGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return parseRolloutWorkload(u)
}
