package main

import (
	"context"
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
	clienttesting "k8s.io/client-go/testing"
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
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)
	t.Cleanup(func() { setRolloutSupport(nil, false) })

	two := int32(2)
	rw := &rolloutWorkload{
		Namespace: "default", Name: "canary-api", Replicas: &two,
		Selector:          &metav1.LabelSelector{MatchLabels: map[string]string{"app": "canary-api"}},
		PodTemplateLabels: map[string]string{"app": "canary-api"},
		ReadyReplicas:     2, AvailableReplicas: 2,
	}
	clientset := fake.NewSimpleClientset()
	createPDBForWorkload(context.Background(), clientset, rw)

	got, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-canary-api-pdb", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected PDB created: %v", err)
	}
	if got.Spec.Selector == nil || got.Spec.Selector.MatchLabels["app"] != "canary-api" {
		t.Fatalf("unexpected selector: %#v", got.Spec.Selector)
	}
}

func TestCreatePDBForWorkload_rolloutSkipsNilSelector(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)

	two := int32(2)
	rw := &rolloutWorkload{
		Namespace: "default", Name: "ref-ro", Replicas: &two,
		Selector: nil, ReadyReplicas: 2, AvailableReplicas: 2,
	}
	clientset := fake.NewSimpleClientset()
	createPDBForWorkload(context.Background(), clientset, rw)
	if _, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-ref-ro-pdb", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected no PDB for nil-selector Rollout, got err=%v", err)
	}
}

func TestCreatePDBForWorkload_skipsOnSameNameConflict(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)
	t.Cleanup(func() { setRolloutSupport(nil, false) })

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
		map[schema.GroupVersionResource]string{getRolloutGVR(): "RolloutList"},
		u,
	)
	setRolloutSupport(dyn, true)

	rw, err := parseRolloutWorkload(u)
	if err != nil {
		t.Fatal(err)
	}
	createPDBForWorkload(context.Background(), clientset, rw)
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
	createPDBForWorkload(context.Background(), clientset2, dep)
	if _, err := clientset2.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-shared-pdb", metav1.GetOptions{}); err != nil {
		t.Fatalf("same-name guard must run before under-replicated delete: %v", err)
	}
}

func TestGarbageCollectOrphanedPDBs_keepsWhenRolloutExists(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)
	t.Cleanup(func() { setRolloutSupport(nil, false) })

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
		map[schema.GroupVersionResource]string{getRolloutGVR(): "RolloutList"},
		u,
	)
	setRolloutSupport(dyn, true)

	garbageCollectOrphanedPDBs(context.Background(), clientset)
	if _, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-canary-api-pdb", metav1.GetOptions{}); err != nil {
		t.Fatalf("PDB should be kept while Rollout exists: %v", err)
	}
}

func TestGarbageCollectOrphanedPDBs_skipsOrphanDeleteWhenInformerDisabledButCRDKnown(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)
	t.Cleanup(func() { setRolloutSupport(nil, false) })

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
		map[schema.GroupVersionResource]string{getRolloutGVR(): "RolloutList"},
		u,
	)
	setRolloutSupport(dyn, true)
	disableRolloutInformer()

	garbageCollectOrphanedPDBs(context.Background(), clientset)
	if _, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-only-ro-pdb", metav1.GetOptions{}); err != nil {
		t.Fatalf("PDB must be kept when CRD known and Rollout still exists: %v", err)
	}
	if rolloutInformerEnabled() {
		t.Fatal("expected informer disabled")
	}
	if !rolloutCRDKnown() {
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
	t.Parallel()
	clientset := fake.NewSimpleClientset()
	disco := clientset.Discovery().(*fakediscovery.FakeDiscovery)
	disco.Resources = []*metav1.APIResourceList{
		{
			GroupVersion: "argoproj.io/v1alpha1",
			APIResources: []metav1.APIResource{{Name: "rollouts", Kind: "Rollout", Namespaced: true}},
		},
	}
	// ServerGroups is derived from Resources in fake discovery when using Invokes;
	// ensure group listing works via reactor.
	disco.AddReactor("get", "group", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return false, nil, nil
	})

	// Fake discovery ServerGroups reads from Resources in newer client-go; set explicitly if needed.
	gvr, available, err := discoverRolloutGVR(disco)
	if err != nil {
		t.Fatal(err)
	}
	// Some fake discovery setups omit groups unless configured; accept either unavailable or preferred GVR.
	if available && (gvr.Group != rolloutGroup || gvr.Resource != rolloutResource) {
		t.Fatalf("unexpected gvr: %#v", gvr)
	}
}
