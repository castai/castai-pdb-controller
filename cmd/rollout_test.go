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
	"k8s.io/apimachinery/pkg/api/meta"
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

// Regression: FixPoorPDBs must honor Rollout bypass (previously only Deploy/STS were checked).
func TestLogAndFixPoorPDBConfig_rolloutBypassRespected(t *testing.T) {
	rt := newControllerRuntime()
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)

	defaultPDBConfigLock.Lock()
	defaultPDBConfig.FixPoorPDBs = true
	defaultPDBConfigLock.Unlock()

	two := int32(2)
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-canary-pdb", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "canary"}},
			MinAvailable: &intstr.IntOrString{Type: intstr.Int, IntVal: 2}, // poor: equals replicas
		},
	}
	clientset := fake.NewSimpleClientset(pdb)
	rw := &rolloutWorkload{
		Namespace: "default", Name: "canary", Replicas: &two,
		Selector:          &metav1.LabelSelector{MatchLabels: map[string]string{"app": "canary"}},
		PodTemplateLabels: map[string]string{"app": "canary"},
		Annotations:       map[string]string{annotationBypass: "true"},
		ReadyReplicas:     2, AvailableReplicas: 2,
	}

	logAndFixPoorPDBConfig(context.Background(), clientset, rt, pdb, "canary", two, "default", rw)

	got, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-canary-pdb", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("bypassed Rollout PDB must not be deleted by FixPoorPDBs: %v", err)
	}
	if got.Spec.MinAvailable == nil || got.Spec.MinAvailable.IntVal != 2 {
		t.Fatalf("bypassed Rollout PDB must be left unchanged, got %#v", got.Spec.MinAvailable)
	}
}

func TestLogAndFixPoorPDBConfig_rolloutWithoutBypassStillFixes(t *testing.T) {
	rt := newControllerRuntime()
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)

	defaultPDBConfigLock.Lock()
	defaultPDBConfig.FixPoorPDBs = true
	defaultPDBConfig.MinAvailable = &intstr.IntOrString{Type: intstr.Int, IntVal: 1}
	defaultPDBConfigLock.Unlock()

	two := int32(2)
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-canary-pdb", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "canary"}},
			MinAvailable: &intstr.IntOrString{Type: intstr.Int, IntVal: 2},
		},
	}
	clientset := fake.NewSimpleClientset(pdb)
	rw := &rolloutWorkload{
		Namespace: "default", Name: "canary", Replicas: &two,
		Selector:          &metav1.LabelSelector{MatchLabels: map[string]string{"app": "canary"}},
		PodTemplateLabels: map[string]string{"app": "canary"},
		ReadyReplicas:     2, AvailableReplicas: 2,
	}

	logAndFixPoorPDBConfig(context.Background(), clientset, rt, pdb, "canary", two, "default", rw)

	got, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-canary-pdb", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected PDB recreated with defaults: %v", err)
	}
	if got.Spec.MinAvailable == nil || got.Spec.MinAvailable.IntVal != 1 {
		t.Fatalf("expected FixPoorPDBs to recreate with default minAvailable=1, got %#v", got.Spec.MinAvailable)
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

func TestGarbageCollectOrphanedPDBs_usesDeployListerNotAPIGet(t *testing.T) {
	rt := newControllerRuntime()
	t.Cleanup(func() {
		rt.setWorkloadListers(nil, nil)
		rt.setRolloutSupport(nil, false)
	})

	two := int32(2)
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-myapp-pdb", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "myapp"}},
			MinAvailable: &intstr.IntOrString{Type: intstr.Int, IntVal: 1},
		},
	}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "default"},
		Spec:       appsv1.DeploymentSpec{Replicas: &two},
	}
	depIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{
		cache.NamespaceIndex: cache.MetaNamespaceIndexFunc,
	})
	stsIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{
		cache.NamespaceIndex: cache.MetaNamespaceIndexFunc,
	})
	if err := depIndexer.Add(dep); err != nil {
		t.Fatal(err)
	}
	rt.setWorkloadListers(appsv1listers.NewDeploymentLister(depIndexer), appsv1listers.NewStatefulSetLister(stsIndexer))

	clientset := fake.NewSimpleClientset(pdb)
	clientset.Fake.PrependReactor("get", "deployments", func(action clienttesting.Action) (bool, runtime.Object, error) {
		t.Fatal("GC must not call Deployment API Get when lister is set")
		return true, nil, nil
	})
	clientset.Fake.PrependReactor("get", "statefulsets", func(action clienttesting.Action) (bool, runtime.Object, error) {
		t.Fatal("GC must not call StatefulSet API Get when lister is set")
		return true, nil, nil
	})

	garbageCollectOrphanedPDBs(context.Background(), clientset, rt)
	if _, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-myapp-pdb", metav1.GetOptions{}); err != nil {
		t.Fatalf("expected PDB kept via lister replicas: %v", err)
	}
}

func TestGarbageCollectOrphanedPDBs_usesRolloutStoreHit(t *testing.T) {
	rt := newControllerRuntime()
	t.Cleanup(func() { rt.setRolloutSupport(nil, false) })

	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-canary-pdb", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "canary"}},
			MinAvailable: &intstr.IntOrString{Type: intstr.Int, IntVal: 1},
		},
	}
	clientset := fake.NewSimpleClientset(pdb)
	u := newUnstructuredRollout("default", "canary", 2,
		map[string]string{"app": "canary"},
		map[string]string{"app": "canary"},
		2, 2,
	)
	// Empty dynamic client: store hit must satisfy GC without API Get.
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{rt.getRolloutGVR(): "RolloutList"},
	)
	rt.setRolloutSupport(dyn, true)
	store := cache.NewStore(cache.MetaNamespaceKeyFunc)
	if err := store.Add(u); err != nil {
		t.Fatal(err)
	}
	rt.setRolloutStore(store)

	garbageCollectOrphanedPDBs(context.Background(), clientset, rt)
	if _, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-canary-pdb", metav1.GetOptions{}); err != nil {
		t.Fatalf("expected PDB kept via Rollout store hit: %v", err)
	}
}

func TestRolloutExistsForConflict_storeMissFallsBackToAPI(t *testing.T) {
	rt := newControllerRuntime()
	t.Cleanup(func() { rt.setRolloutSupport(nil, false) })

	u := newUnstructuredRollout("default", "shared", 2,
		map[string]string{"app": "shared"},
		map[string]string{"app": "shared"},
		2, 2,
	)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{rt.getRolloutGVR(): "RolloutList"},
		u,
	)
	rt.setRolloutSupport(dyn, true)
	// Empty store mimics Deploy/STS handlers racing ahead of Rollout cache sync.
	rt.setRolloutStore(cache.NewStore(cache.MetaNamespaceKeyFunc))

	exists, err := rolloutExistsForConflict(context.Background(), rt, "default", "shared")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exists {
		t.Fatal("store miss must fall back to API and observe the Rollout")
	}
}

func TestRolloutExistsForConflict_storeHitSkipsAPI(t *testing.T) {
	rt := newControllerRuntime()
	t.Cleanup(func() { rt.setRolloutSupport(nil, false) })

	store := cache.NewStore(cache.MetaNamespaceKeyFunc)
	u := newUnstructuredRollout("default", "shared", 2,
		map[string]string{"app": "shared"},
		map[string]string{"app": "shared"},
		2, 2,
	)
	if err := store.Add(u); err != nil {
		t.Fatal(err)
	}
	// No dynamic client — if we incorrectly fall through to API on a hit, Get fails.
	rt.setRolloutCRDKnown(nil)
	rt.setRolloutStore(store)

	exists, err := rolloutExistsForConflict(context.Background(), rt, "default", "shared")
	if err != nil {
		t.Fatalf("store hit should not need API: %v", err)
	}
	if !exists {
		t.Fatal("expected store hit to report Rollout present")
	}
}

func TestClearRolloutSupportIfAPIGone(t *testing.T) {
	rt := newControllerRuntime()
	t.Cleanup(func() { rt.setRolloutSupport(nil, false) })

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{rt.getRolloutGVR(): "RolloutList"},
	)
	rt.setRolloutSupport(dyn, true)
	rt.clearRolloutSupportIfAPIGone(fmt.Errorf("some other error"), true)
	if !rt.rolloutCRDKnown() || !rt.rolloutInformerEnabled() {
		t.Fatal("non-NoMatch errors must leave Rollout support enabled")
	}

	// Get-path NotFound is a missing object — must not disable CRD support.
	rt.clearRolloutSupportIfAPIGone(apierrors.NewNotFound(rt.getRolloutGVR().GroupResource(), "missing"), false)
	if !rt.rolloutCRDKnown() || !rt.rolloutInformerEnabled() {
		t.Fatal("Get NotFound must not disable Rollout support")
	}

	// List-path NotFound typically means the resource type/CRD was removed.
	rt.clearRolloutSupportIfAPIGone(apierrors.NewNotFound(rt.getRolloutGVR().GroupResource(), ""), true)
	if rt.rolloutCRDKnown() || rt.rolloutInformerEnabled() {
		t.Fatal("List NotFound must disable Rollout support")
	}

	rt.setRolloutSupport(dyn, true)
	rt.clearRolloutSupportIfAPIGone(&meta.NoKindMatchError{
		GroupKind: schema.GroupKind{Group: rolloutGroup, Kind: "Rollout"},
	}, false)
	if rt.rolloutCRDKnown() || rt.rolloutInformerEnabled() {
		t.Fatal("NoMatchError must disable Rollout support")
	}
}

func TestSetRolloutSupport_enabledClearsStaleStore(t *testing.T) {
	rt := newControllerRuntime()
	t.Cleanup(func() { rt.setRolloutSupport(nil, false) })

	store := cache.NewStore(cache.MetaNamespaceKeyFunc)
	u := newUnstructuredRollout("default", "stale", 2,
		map[string]string{"app": "stale"},
		map[string]string{"app": "stale"},
		2, 2,
	)
	if err := store.Add(u); err != nil {
		t.Fatal(err)
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{rt.getRolloutGVR(): "RolloutList"},
	)
	rt.setRolloutSupport(dyn, true)
	rt.setRolloutStore(store)
	rt.setRolloutSupport(dyn, true) // re-enable without disableRolloutInformer
	if _, ok := rt.rolloutExistsInStore("default", "stale"); ok {
		t.Fatal("enabled setRolloutSupport must clear stale store until setRolloutStore")
	}
}

func TestMaybeStartRolloutInformer_absentCRDLeavesUnknown(t *testing.T) {
	rt := newControllerRuntime()
	t.Cleanup(func() { rt.setRolloutSupport(nil, false) })

	clientset := fake.NewSimpleClientset()
	disco := clientset.Discovery().(*fakediscovery.FakeDiscovery)
	disco.Resources = []*metav1.APIResourceList{}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{rt.getRolloutGVR(): "RolloutList"},
	)

	synced, stop := rt.maybeStartRolloutInformer(context.Background(), clientset, dyn)
	if synced != nil || stop != nil {
		t.Fatal("expected no informer when CRD absent")
	}
	if rt.rolloutCRDKnown() || rt.rolloutInformerEnabled() {
		t.Fatal("absent CRD must leave Rollout support unknown/disabled for later rediscovery")
	}
}

func TestDisableRolloutInformer_keepsCRDKnownClearsStore(t *testing.T) {
	rt := newControllerRuntime()
	t.Cleanup(func() { rt.setRolloutSupport(nil, false) })

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{rt.getRolloutGVR(): "RolloutList"},
	)
	rt.setRolloutSupport(dyn, true)
	rt.setRolloutStore(cache.NewStore(cache.MetaNamespaceKeyFunc))
	rt.disableRolloutInformer()

	if !rt.rolloutCRDKnown() {
		t.Fatal("sync-timeout path must keep CRD known for GC/conflict API fallback")
	}
	if rt.rolloutInformerEnabled() {
		t.Fatal("informer must be disabled after sync timeout")
	}
	if _, ok := rt.rolloutExistsInStore("default", "x"); ok {
		t.Fatal("store must be cleared so conflict checks use API")
	}
}
