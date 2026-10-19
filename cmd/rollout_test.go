package main

import (
	"context"
	"fmt"
	"math"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	appsv1listers "k8s.io/client-go/listers/apps/v1"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

func newUnstructuredRollout(ns, name string, replicas int64, selector, podLabels map[string]string, ready, available int64) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   rolloutGroup,
		Version: rolloutVersionPreferred,
		Kind:    "Rollout",
	})
	u.SetNamespace(ns)
	u.SetName(name)
	_ = unstructured.SetNestedField(u.Object, replicas, "spec", "replicas")
	sel := map[string]interface{}{"matchLabels": map[string]interface{}{}}
	for k, v := range selector {
		sel["matchLabels"].(map[string]interface{})[k] = v
	}
	_ = unstructured.SetNestedMap(u.Object, sel, "spec", "selector")
	_ = unstructured.SetNestedStringMap(u.Object, podLabels, "spec", "template", "metadata", "labels")
	_ = unstructured.SetNestedField(u.Object, ready, "status", "readyReplicas")
	_ = unstructured.SetNestedField(u.Object, available, "status", "availableReplicas")
	return u
}

func TestParseRolloutWorkload(t *testing.T) {
	t.Parallel()
	u := newUnstructuredRollout("ns", "my-rollout", 3,
		map[string]string{"app": "my-rollout"},
		map[string]string{"app": "my-rollout", "role": "api"},
		3, 3,
	)
	rw, err := parseRolloutWorkload(u)
	if err != nil {
		t.Fatalf("parseRolloutWorkload: %v", err)
	}
	if rw.Namespace != "ns" || rw.Name != "my-rollout" {
		t.Fatalf("identity: %+v", rw)
	}
	if rw.Replicas == nil || *rw.Replicas != 3 {
		t.Fatalf("replicas: %v", rw.Replicas)
	}
	if rw.Selector == nil || rw.Selector.MatchLabels["app"] != "my-rollout" {
		t.Fatalf("selector: %+v", rw.Selector)
	}
	if rw.ReadyReplicas != 3 || rw.AvailableReplicas != 3 {
		t.Fatalf("status ready=%d available=%d", rw.ReadyReplicas, rw.AvailableReplicas)
	}
}

func TestInt64ToInt32_overflow(t *testing.T) {
	t.Parallel()
	if _, err := int64ToInt32(math.MaxInt32+1, "spec.replicas"); err == nil {
		t.Fatal("expected overflow error")
	}
}

func TestCreatePDBForWorkload_rolloutHappyPath(t *testing.T) {
	rt := newControllerRuntime()
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)
	t.Cleanup(func() { rt.setRolloutSupport(nil, false) })

	two := int32(2)
	rw := &rolloutWorkload{
		Namespace: "default", Name: "canary-api", Replicas: &two,
		Selector:          &metav1.LabelSelector{MatchLabels: map[string]string{"app": "canary-api"}},
		PodTemplateLabels: map[string]string{"app": "canary-api"},
		ReadyReplicas:     2, AvailableReplicas: 2,
	}
	clientset := fake.NewSimpleClientset()
	createPDBForWorkload(context.Background(), clientset, rt, rw)

	got, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-canary-api-pdb", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected PDB created: %v", err)
	}
	if got.Spec.Selector == nil || got.Spec.Selector.MatchLabels["app"] != "canary-api" {
		t.Fatalf("unexpected selector: %#v", got.Spec.Selector)
	}
}

func TestCreatePDBForWorkload_rolloutSkipsNilSelector(t *testing.T) {
	rt := newControllerRuntime()
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)

	two := int32(2)
	rw := &rolloutWorkload{
		Namespace: "default", Name: "ref-ro", Replicas: &two,
		Selector: nil, ReadyReplicas: 2, AvailableReplicas: 2,
	}
	clientset := fake.NewSimpleClientset()
	createPDBForWorkload(context.Background(), clientset, rt, rw)
	if _, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-ref-ro-pdb", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected no PDB for nil-selector Rollout, got err=%v", err)
	}
}

func TestCreatePDBForWorkload_skipsOnSameNameConflict(t *testing.T) {
	rt := newControllerRuntime()
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)
	t.Cleanup(func() { rt.setRolloutSupport(nil, false) })

	two := int32(2)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &two,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "dep"}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "dep"}}},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 2, AvailableReplicas: 2},
	}
	clientset := fake.NewSimpleClientset(dep)
	u := newUnstructuredRollout("default", "shared", 2,
		map[string]string{"app": "ro"},
		map[string]string{"app": "ro"},
		2, 2,
	)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{rt.getRolloutGVR(): "RolloutList"},
		u,
	)
	rt.setRolloutSupport(dyn, true)

	rw, err := parseRolloutWorkload(u)
	if err != nil {
		t.Fatal(err)
	}
	createPDBForWorkload(context.Background(), clientset, rt, rw)
	if _, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-shared-pdb", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected skip on Deploy+Rollout same name, got err=%v", err)
	}

	// Under-replicated Deploy must not delete a PDB that would belong to a same-named Rollout.
	one := int32(1)
	dep.Spec.Replicas = &one
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-shared-pdb", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &intstr.IntOrString{Type: intstr.Int, IntVal: 1},
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "ro"}},
		},
	}
	clientset2 := fake.NewSimpleClientset(dep, pdb)
	createPDBForWorkload(context.Background(), clientset2, rt, dep)
	if _, err := clientset2.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-shared-pdb", metav1.GetOptions{}); err != nil {
		t.Fatalf("same-name guard must run before under-replicated delete: %v", err)
	}
}

func TestGarbageCollectOrphanedPDBs_keepsWhenRolloutExists(t *testing.T) {
	rt := newControllerRuntime()
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)
	t.Cleanup(func() { rt.setRolloutSupport(nil, false) })

	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-canary-api-pdb", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &intstr.IntOrString{Type: intstr.Int, IntVal: 1},
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "canary-api"}},
		},
	}
	clientset := fake.NewSimpleClientset(pdb)
	u := newUnstructuredRollout("default", "canary-api", 3,
		map[string]string{"app": "canary-api"},
		map[string]string{"app": "canary-api"},
		3, 3,
	)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{rt.getRolloutGVR(): "RolloutList"},
		u,
	)
	rt.setRolloutSupport(dyn, true)

	garbageCollectOrphanedPDBs(context.Background(), clientset, rt)
	if _, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-canary-api-pdb", metav1.GetOptions{}); err != nil {
		t.Fatalf("PDB should be kept while Rollout exists: %v", err)
	}
}

func TestGarbageCollectOrphanedPDBs_skipsOrphanDeleteWhenInformerDisabledButCRDKnown(t *testing.T) {
	rt := newControllerRuntime()
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)
	t.Cleanup(func() { rt.setRolloutSupport(nil, false) })

	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-only-ro-pdb", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &intstr.IntOrString{Type: intstr.Int, IntVal: 1},
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "only-ro"}},
		},
	}
	clientset := fake.NewSimpleClientset(pdb)
	u := newUnstructuredRollout("default", "only-ro", 2,
		map[string]string{"app": "only-ro"},
		map[string]string{"app": "only-ro"},
		2, 2,
	)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{rt.getRolloutGVR(): "RolloutList"},
		u,
	)
	rt.setRolloutSupport(dyn, true)
	rt.disableRolloutInformer()

	garbageCollectOrphanedPDBs(context.Background(), clientset, rt)
	if _, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-only-ro-pdb", metav1.GetOptions{}); err != nil {
		t.Fatalf("PDB must be kept when CRD known and Rollout still exists: %v", err)
	}
	if rt.rolloutInformerEnabled() {
		t.Fatal("expected informer disabled")
	}
	if !rt.rolloutCRDKnown() {
		t.Fatal("expected CRD still known")
	}
}

func TestDiscoverRolloutGVR_missingGroup(t *testing.T) {
	t.Parallel()
	clientset := fake.NewSimpleClientset()
	disco := clientset.Discovery().(*fakediscovery.FakeDiscovery)
	disco.Resources = []*metav1.APIResourceList{}
	_, available, err := discoverRolloutGVR(disco)
	if err != nil {
		t.Fatal(err)
	}
	if available {
		t.Fatal("expected Rollouts unavailable")
	}
}

func TestDiscoverRolloutGVR_preferredVersion(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	disco := clientset.Discovery().(*fakediscovery.FakeDiscovery)
	disco.Resources = []*metav1.APIResourceList{
		{
			GroupVersion: "argoproj.io/v1alpha1",
			APIResources: []metav1.APIResource{{Name: "rollouts", Kind: "Rollout", Namespaced: true, Verbs: []string{"get", "list", "watch"}}},
		},
	}
	gvr, available, err := discoverRolloutGVR(disco)
	if err != nil {
		t.Fatal(err)
	}
	if !available {
		t.Fatal("expected Rollouts available at preferred version")
	}
	if gvr.Group != rolloutGroup || gvr.Version != rolloutVersionPreferred || gvr.Resource != rolloutResource {
		t.Fatalf("unexpected gvr: %#v", gvr)
	}
}

func TestDiscoverRolloutGVR_fallbackVersion(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	disco := clientset.Discovery().(*fakediscovery.FakeDiscovery)
	// Preferred v1alpha1 missing; older/other version still serves rollouts.
	disco.Resources = []*metav1.APIResourceList{
		{
			GroupVersion: "argoproj.io/v1alpha1",
			APIResources: []metav1.APIResource{{Name: "analyses", Kind: "AnalysisRun", Namespaced: true}},
		},
		{
			GroupVersion: "argoproj.io/v1beta1",
			APIResources: []metav1.APIResource{{Name: "rollouts", Kind: "Rollout", Namespaced: true, Verbs: []string{"get", "list", "watch"}}},
		},
	}
	gvr, available, err := discoverRolloutGVR(disco)
	if err != nil {
		t.Fatal(err)
	}
	if !available {
		t.Fatal("expected fallback discovery to find rollouts")
	}
	if gvr.Group != rolloutGroup || gvr.Resource != rolloutResource || gvr.Version != "v1beta1" {
		t.Fatalf("expected v1beta1 fallback, got %#v", gvr)
	}
}

func TestCreatePDBForWorkload_rolloutHonorsMinAvailableAnnotation(t *testing.T) {
	rt := newControllerRuntime()
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)

	two := int32(2)
	rw := &rolloutWorkload{
		Namespace: "default", Name: "anno-ro", Replicas: &two,
		Selector:          &metav1.LabelSelector{MatchLabels: map[string]string{"app": "anno-ro"}},
		PodTemplateLabels: map[string]string{"app": "anno-ro"},
		Annotations:       map[string]string{annotationMinAvailable: "2"},
		ReadyReplicas:     2, AvailableReplicas: 2,
	}
	clientset := fake.NewSimpleClientset()
	createPDBForWorkload(context.Background(), clientset, rt, rw)
	got, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-anno-ro-pdb", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected PDB: %v", err)
	}
	if got.Spec.MinAvailable == nil || got.Spec.MinAvailable.IntVal != 2 {
		t.Fatalf("expected minAvailable=2 from annotation, got %#v", got.Spec.MinAvailable)
	}
}

func TestDeletePDBForWorkload_keepsPDBWhenSameNamedRolloutExists(t *testing.T) {
	rt := newControllerRuntime()
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)
	t.Cleanup(func() { rt.setRolloutSupport(nil, false) })

	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-shared-pdb", Namespace: "default"},
		Spec:       policyv1.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "ro"}}},
	}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "default"}}
	clientset := fake.NewSimpleClientset(pdb, dep)
	u := newUnstructuredRollout("default", "shared", 2,
		map[string]string{"app": "ro"},
		map[string]string{"app": "ro"},
		2, 2,
	)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{rt.getRolloutGVR(): "RolloutList"},
		u,
	)
	rt.setRolloutSupport(dyn, true)

	deletePDBForWorkload(context.Background(), clientset, rt, dep)
	if _, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-shared-pdb", metav1.GetOptions{}); err != nil {
		t.Fatalf("PDB must remain when same-named Rollout still exists: %v", err)
	}
}

func TestHandleWorkloadUpdate_rolloutAnnotationUpdatesPDB(t *testing.T) {
	rt := newControllerRuntime()
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)

	two := int32(2)
	existing := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-canary-api-pdb", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "canary-api"}},
			MinAvailable: &intstr.IntOrString{Type: intstr.Int, IntVal: 1},
		},
	}
	clientset := fake.NewSimpleClientset(existing)
	oldRW := &rolloutWorkload{
		Namespace: "default", Name: "canary-api", Replicas: &two,
		Selector:          &metav1.LabelSelector{MatchLabels: map[string]string{"app": "canary-api"}},
		PodTemplateLabels: map[string]string{"app": "canary-api"},
		ReadyReplicas:     2, AvailableReplicas: 2,
	}
	newRW := &rolloutWorkload{
		Namespace: "default", Name: "canary-api", Replicas: &two,
		Selector:          &metav1.LabelSelector{MatchLabels: map[string]string{"app": "canary-api"}},
		PodTemplateLabels: map[string]string{"app": "canary-api"},
		Annotations:       map[string]string{annotationMinAvailable: "2"},
		ReadyReplicas:     2, AvailableReplicas: 2,
	}
	handleWorkloadUpdate(context.Background(), clientset, rt, oldRW, newRW)
	got, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-canary-api-pdb", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get PDB: %v", err)
	}
	if got.Spec.MinAvailable == nil || got.Spec.MinAvailable.IntVal != 2 {
		t.Fatalf("expected updated minAvailable=2, got %#v", got.Spec.MinAvailable)
	}
}

func TestWorkloadReplicaHealth_rollout(t *testing.T) {
	t.Parallel()
	two := int32(2)
	rw := &rolloutWorkload{Replicas: &two, ReadyReplicas: 2, AvailableReplicas: 2}
	desired, ready, available, ok := workloadReplicaHealth(rw)
	if !ok || desired != 2 || ready != 2 || available != 2 {
		t.Fatalf("got desired=%d ready=%d available=%d ok=%v", desired, ready, available, ok)
	}
}

func TestSetRolloutCRDKnown_keepsKnownWithoutInformer(t *testing.T) {
	rt := newControllerRuntime()
	t.Cleanup(func() { rt.setRolloutSupport(nil, false) })
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{rt.getRolloutGVR(): "RolloutList"},
	)
	rt.setRolloutCRDKnown(dyn)
	if !rt.rolloutCRDKnown() {
		t.Fatal("expected CRD known")
	}
	if rt.rolloutInformerEnabled() {
		t.Fatal("informer must stay disabled")
	}
	if _, ok := rt.getRolloutClient(); !ok {
		t.Fatal("lookup client should remain available")
	}
}

func TestSameNameConflicts_usesDeployListerNotAPIGet(t *testing.T) {
	rt := newControllerRuntime()
	t.Cleanup(func() {
		rt.setWorkloadListers(nil, nil)
		rt.setRolloutSupport(nil, false)
	})

	depIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{
		cache.NamespaceIndex: cache.MetaNamespaceIndexFunc,
	})
	stsIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{
		cache.NamespaceIndex: cache.MetaNamespaceIndexFunc,
	})
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "default"}}
	if err := depIndexer.Add(dep); err != nil {
		t.Fatal(err)
	}
	rt.setWorkloadListers(appsv1listers.NewDeploymentLister(depIndexer), appsv1listers.NewStatefulSetLister(stsIndexer))

	clientset := fake.NewSimpleClientset()
	clientset.Fake.PrependReactor("get", "deployments", func(action clienttesting.Action) (bool, runtime.Object, error) {
		t.Fatal("sameNameConflicts must not call Deployment API Get when lister is set")
		return true, nil, nil
	})

	others, err := sameNameConflicts(context.Background(), clientset, rt, "default", "shared", "Rollout")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(others) != 1 || others[0] != "Deployment" {
		t.Fatalf("expected [Deployment] from lister, got %v", others)
	}
}

func TestRolloutExistsInStore(t *testing.T) {
	rt := newControllerRuntime()
	t.Cleanup(func() { rt.setRolloutSupport(nil, false) })

	store := cache.NewStore(cache.MetaNamespaceKeyFunc)
	u := newUnstructuredRollout("default", "shared", 2,
		map[string]string{"app": "ro"},
		map[string]string{"app": "ro"},
		2, 2,
	)
	if err := store.Add(u); err != nil {
		t.Fatal(err)
	}
	rt.setRolloutStore(store)

	exists, ok := rt.rolloutExistsInStore("default", "shared")
	if !ok || !exists {
		t.Fatalf("expected store hit, ok=%v exists=%v", ok, exists)
	}
	exists, ok = rt.rolloutExistsInStore("default", "missing")
	if !ok || exists {
		t.Fatalf("expected store miss, ok=%v exists=%v", ok, exists)
	}
}

type errStore struct{ cache.Store }

func (errStore) GetByKey(string) (interface{}, bool, error) {
	return nil, false, fmt.Errorf("indexer boom")
}

func TestRolloutExistsInStore_errorFallsBack(t *testing.T) {
	rt := newControllerRuntime()
	t.Cleanup(func() { rt.setRolloutSupport(nil, false) })
	rt.setRolloutStore(errStore{Store: cache.NewStore(cache.MetaNamespaceKeyFunc)})
	exists, ok := rt.rolloutExistsInStore("default", "shared")
	if ok || exists {
		t.Fatalf("GetByKey error must force API fallback (ok=false), got ok=%v exists=%v", ok, exists)
	}
}
