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
	"k8s.io/client-go/tools/cache"
)

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
	if rw.PodTemplateLabels["role"] != "api" {
		t.Fatalf("pod labels: %+v", rw.PodTemplateLabels)
	}
	if rw.ReadyReplicas != 3 || rw.AvailableReplicas != 3 {
		t.Fatalf("status ready=%d available=%d", rw.ReadyReplicas, rw.AvailableReplicas)
	}
}

func TestParseRolloutWorkload_availableDefaultsToReady(t *testing.T) {
	t.Parallel()
	u := newUnstructuredRollout("ns", "r", 2,
		map[string]string{"app": "r"},
		map[string]string{"app": "r"},
		2, 2,
	)
	delete(u.Object["status"].(map[string]interface{}), "availableReplicas")
	rw, err := parseRolloutWorkload(u)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if rw.AvailableReplicas != rw.ReadyReplicas {
		t.Fatalf("available=%d ready=%d", rw.AvailableReplicas, rw.ReadyReplicas)
	}
}

func TestAsWorkloadView_rollout(t *testing.T) {
	t.Parallel()
	replicas := int32(4)
	rw := &rolloutWorkload{
		Namespace:         "ns",
		Name:              "ro",
		Replicas:          &replicas,
		Selector:          &metav1.LabelSelector{MatchLabels: map[string]string{"app": "ro"}},
		Annotations:       map[string]string{annotationBypass: "false"},
		Labels:            map[string]string{"team": "a"},
		PodTemplateLabels: map[string]string{"app": "ro"},
	}
	wv, ok := asWorkloadView(rw)
	if !ok {
		t.Fatal("expected rolloutWorkload to map to workloadView")
	}
	if wv.namespace != "ns" || wv.name != "ro" || wv.replicas == nil || *wv.replicas != 4 {
		t.Fatalf("view: %+v", wv)
	}
}

func TestCreatePDBForWorkload_rollout(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)
	t.Cleanup(func() { setRolloutSupport(nil, false) })

	replicas := int32(2)
	rw := &rolloutWorkload{
		Namespace: "default",
		Name:      "canary-api",
		Replicas:  &replicas,
		Selector:  &metav1.LabelSelector{MatchLabels: map[string]string{"app": "canary-api"}},
		PodTemplateLabels: map[string]string{
			"app": "canary-api",
		},
		ReadyReplicas:     2,
		AvailableReplicas: 2,
	}

	clientset := fake.NewSimpleClientset()
	createPDBForWorkload(context.Background(), clientset, rw)

	pdb, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-canary-api-pdb", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected PDB created for Rollout: %v", err)
	}
	if pdb.Spec.Selector == nil || pdb.Spec.Selector.MatchLabels["app"] != "canary-api" {
		t.Fatalf("unexpected PDB selector: %+v", pdb.Spec.Selector)
	}
}

func TestUpdateExistingPDB_rolloutUpdatesMinAvailable(t *testing.T) {
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
	rw := &rolloutWorkload{
		Namespace:         "default",
		Name:              "canary-api",
		Replicas:          &two,
		Selector:          &metav1.LabelSelector{MatchLabels: map[string]string{"app": "canary-api"}},
		PodTemplateLabels: map[string]string{"app": "canary-api"},
		Annotations:       map[string]string{annotationMinAvailable: "2"},
		ReadyReplicas:     2,
		AvailableReplicas: 2,
	}

	updateExistingPDB(context.Background(), clientset, existing, rw.Annotations, &two, "default", "canary-api", rw)
	got, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-canary-api-pdb", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get PDB: %v", err)
	}
	if got.Spec.MinAvailable == nil || got.Spec.MinAvailable.IntVal != 2 {
		t.Fatalf("expected minAvailable=2, got %#v", got.Spec.MinAvailable)
	}
}

func TestHandleWorkloadUpdate_rolloutBypassRemovesPDB(t *testing.T) {
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
	}
	newRW := &rolloutWorkload{
		Namespace: "default", Name: "canary-api", Replicas: &two,
		Selector:          &metav1.LabelSelector{MatchLabels: map[string]string{"app": "canary-api"}},
		PodTemplateLabels: map[string]string{"app": "canary-api"},
		Annotations:       map[string]string{annotationBypass: "true"},
	}

	handleWorkloadUpdate(context.Background(), clientset, oldRW, newRW)
	_, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-canary-api-pdb", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected PDB deleted on bypass, got err=%v", err)
	}
}

func TestReconcileAllDefaultPDBs_createsForRollout(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)
	t.Cleanup(func() { setRolloutSupport(nil, false) })

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	clientset := fake.NewSimpleClientset(ns)
	u := newUnstructuredRollout("default", "canary-api", 2,
		map[string]string{"app": "canary-api"},
		map[string]string{"app": "canary-api"},
		2, 2,
	)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{getRolloutGVR(): "RolloutList"},
		u,
	)
	setRolloutSupport(dyn, true)

	reconcileAllDefaultPDBs(context.Background(), clientset)
	if _, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-canary-api-pdb", metav1.GetOptions{}); err != nil {
		t.Fatalf("expected reconcile to create Rollout PDB: %v", err)
	}
}

func TestScanAllPDBsForMultiplePDBs_rollout(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)
	t.Cleanup(func() { setRolloutSupport(nil, false) })

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	helmPDB := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-canary-api", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &intstr.IntOrString{Type: intstr.Int, IntVal: 1},
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "canary-api"}},
		},
	}
	leftover := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-canary-api-pdb", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &intstr.IntOrString{Type: intstr.Int, IntVal: 1},
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "canary-api"}},
		},
	}
	clientset := fake.NewSimpleClientset(ns, helmPDB, leftover)
	u := newUnstructuredRollout("default", "canary-api", 2,
		map[string]string{"app": "canary-api"},
		map[string]string{"app": "canary-api"},
		2, 2,
	)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{getRolloutGVR(): "RolloutList"},
		u,
	)
	setRolloutSupport(dyn, true)

	scanAllPDBsForMultiplePDBs(context.Background(), clientset)
	_, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-canary-api-pdb", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected leftover controller PDB deleted for Rollout, got err=%v", err)
	}
}

func TestScanAllPDBsForPoorConfig_rollout(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)
	t.Cleanup(func() { setRolloutSupport(nil, false) })

	three := int32(3)
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-canary-api-pdb", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &intstr.IntOrString{Type: intstr.Int, IntVal: 3},
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "canary-api"}},
		},
	}
	clientset := fake.NewSimpleClientset(pdb)
	u := newUnstructuredRollout("default", "canary-api", three,
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

	// Should not panic / error; poor-config path logs for matching Rollout selector.
	scanAllPDBsForPoorConfig(context.Background(), clientset)
}

func TestPickGCWorkloadReplicas_prefersSelectorMatch(t *testing.T) {
	t.Parallel()
	one := int32(1)
	three := int32(3)
	pdb := policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-shared-pdb", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			// Insert keys in a different order than the Rollout selector map literal below;
			// Semantic.DeepEqual must still treat them as equal.
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "api", "app": "rollout-app"}},
		},
	}
	got := pickGCWorkloadReplicas(pdb, "shared", []gcWorkloadCandidate{
		{
			kind:              "Deployment",
			replicas:          &one,
			selector:          &metav1.LabelSelector{MatchLabels: map[string]string{"app": "deploy-app"}},
			podTemplateLabels: map[string]string{"app": "deploy-app"},
		},
		{
			kind:     "Rollout",
			replicas: &three,
			selector: &metav1.LabelSelector{MatchLabels: map[string]string{
				"app":  "rollout-app",
				"tier": "api",
			}},
			podTemplateLabels: map[string]string{"app": "rollout-app", "tier": "api"},
		},
	})
	if got == nil || *got != 3 {
		t.Fatalf("expected Rollout replicas=3, got %v", got)
	}
}

func TestInt64ToInt32_overflow(t *testing.T) {
	t.Parallel()
	if _, err := int64ToInt32(math.MaxInt32+1, "spec.replicas"); err == nil {
		t.Fatal("expected overflow error")
	}
	got, err := int64ToInt32(3, "spec.replicas")
	if err != nil || got != 3 {
		t.Fatalf("got %d err=%v", got, err)
	}
}

func TestCreatePDBForWorkload_rolloutSkipsNilSelector(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)
	two := int32(2)
	rw := &rolloutWorkload{
		Namespace: "default", Name: "ref-ro", Replicas: &two,
		Selector:      nil, // workloadRef-style
		ReadyReplicas: 2, AvailableReplicas: 2,
	}
	clientset := fake.NewSimpleClientset()
	createPDBForWorkload(context.Background(), clientset, rw)
	if _, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-ref-ro-pdb", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected no PDB for nil-selector Rollout, got err=%v", err)
	}
}

func TestCreatePDBForWorkload_skipsOnNameCollision(t *testing.T) {
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
		t.Fatalf("expected skip on Deploy+Rollout name collision, got err=%v", err)
	}
}

func TestDeletePDBForWorkload_unwrapsTombstone(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)

	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-web-pdb", Namespace: "default"},
		Spec:       policyv1.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}},
	}
	clientset := fake.NewSimpleClientset(pdb)
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"}}
	deletePDBForWorkload(context.Background(), clientset, cache.DeletedFinalStateUnknown{Obj: dep})
	if _, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-web-pdb", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected PDB deleted via tombstone unwrap, got err=%v", err)
	}
}

func TestGarbageCollectOrphanedPDBs_skipsWhenRolloutSupportSuspended(t *testing.T) {
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
	suspendRolloutSupport()

	garbageCollectOrphanedPDBs(context.Background(), clientset)
	if _, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-only-ro-pdb", metav1.GetOptions{}); err != nil {
		t.Fatalf("PDB must be kept while Rollout support is suspended: %v", err)
	}
}

func TestPickGCWorkloadReplicas_prefersExactOverCoverAndMaxReplicas(t *testing.T) {
	t.Parallel()
	one := int32(1)
	three := int32(3)
	pdb := policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-shared-pdb", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "shared"}},
		},
	}
	// Coverage-only under-replicated Deploy vs exact-match multi-replica Rollout.
	got := pickGCWorkloadReplicas(pdb, "shared", []gcWorkloadCandidate{
		{
			kind: "Deployment", replicas: &one,
			selector:          &metav1.LabelSelector{MatchLabels: map[string]string{"app": "shared", "extra": "x"}},
			podTemplateLabels: map[string]string{"app": "shared", "extra": "x"},
		},
		{
			kind: "Rollout", replicas: &three,
			selector:          &metav1.LabelSelector{MatchLabels: map[string]string{"app": "shared"}},
			podTemplateLabels: map[string]string{"app": "shared"},
		},
	})
	if got == nil || *got != 3 {
		t.Fatalf("expected exact-match Rollout replicas=3, got %v", got)
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

	_, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-canary-api-pdb", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("PDB should be kept while Rollout exists: %v", err)
	}
}

func TestGarbageCollectOrphanedPDBs_nameCollisionUsesMatchingRollout(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)
	t.Cleanup(func() { setRolloutSupport(nil, false) })

	one := int32(1)
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-shared-pdb", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &intstr.IntOrString{Type: intstr.Int, IntVal: 1},
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "rollout-app"}},
		},
	}
	// Same name as Rollout, but different selector and under-replicated — must not win.
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "deploy-app"}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "deploy-app"}}},
		},
	}
	clientset := fake.NewSimpleClientset(pdb, dep)
	u := newUnstructuredRollout("default", "shared", 3,
		map[string]string{"app": "rollout-app"},
		map[string]string{"app": "rollout-app"},
		3, 3,
	)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{getRolloutGVR(): "RolloutList"},
		u,
	)
	setRolloutSupport(dyn, true)

	garbageCollectOrphanedPDBs(context.Background(), clientset)
	if _, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-shared-pdb", metav1.GetOptions{}); err != nil {
		t.Fatalf("PDB should be kept using matching Rollout replicas, got err=%v", err)
	}
}

func TestGarbageCollectOrphanedPDBs_deletesWhenRolloutUnderReplicated(t *testing.T) {
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

	u := newUnstructuredRollout("default", "canary-api", 1,
		map[string]string{"app": "canary-api"},
		map[string]string{"app": "canary-api"},
		1, 1,
	)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{getRolloutGVR(): "RolloutList"},
		u,
	)
	setRolloutSupport(dyn, true)

	garbageCollectOrphanedPDBs(context.Background(), clientset)

	_, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-canary-api-pdb", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected under-replicated Rollout PDB to be deleted, got err=%v", err)
	}
}

func TestListRollouts_noopWhenSupportDisabled(t *testing.T) {
	t.Cleanup(func() { setRolloutSupport(nil, false) })
	setRolloutSupport(nil, false)
	got, err := listRollouts(context.Background(), "default")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil list when disabled, got %d items", len(got))
	}
}

func TestDiscoverRolloutGVR_prefersV1Alpha1(t *testing.T) {
	t.Parallel()
	fd := &fakediscovery.FakeDiscovery{Fake: &clienttesting.Fake{}}
	fd.Resources = []*metav1.APIResourceList{{
		GroupVersion: "argoproj.io/v1alpha1",
		APIResources: []metav1.APIResource{{Name: "rollouts", Kind: "Rollout", Namespaced: true}},
	}}
	gvr, ok, err := discoverRolloutGVR(fd)
	if err != nil || !ok {
		t.Fatalf("expected found, got ok=%v err=%v", ok, err)
	}
	if gvr.Group != rolloutGroup || gvr.Version != rolloutVersionPreferred || gvr.Resource != rolloutResource {
		t.Fatalf("unexpected gvr: %#v", gvr)
	}
}

func TestDiscoverRolloutGVR_fallbackOtherVersion(t *testing.T) {
	t.Parallel()
	fd := &fakediscovery.FakeDiscovery{Fake: &clienttesting.Fake{}}
	fd.Resources = []*metav1.APIResourceList{{
		GroupVersion: "argoproj.io/v1beta1",
		APIResources: []metav1.APIResource{{Name: "rollouts", Kind: "Rollout", Namespaced: true}},
	}}
	gvr, ok, err := discoverRolloutGVR(fd)
	if err != nil || !ok {
		t.Fatalf("expected fallback found, got ok=%v err=%v", ok, err)
	}
	if gvr.Version != "v1beta1" {
		t.Fatalf("expected v1beta1 fallback, got %#v", gvr)
	}
}

func TestDiscoverRolloutGVR_missing(t *testing.T) {
	t.Parallel()
	fd := &fakediscovery.FakeDiscovery{Fake: &clienttesting.Fake{}}
	gvr, ok, err := discoverRolloutGVR(fd)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ok || gvr.Resource != "" {
		t.Fatalf("expected missing, got ok=%v gvr=%#v", ok, gvr)
	}
}

func TestMaybeStartRolloutInformer_skipsWhenAPIMissing(t *testing.T) {
	t.Cleanup(func() { setRolloutSupport(nil, false) })
	clientset := fake.NewSimpleClientset()
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	synced, stop := maybeStartRolloutInformer(context.Background(), clientset, dyn)
	if synced != nil || stop != nil {
		t.Fatalf("expected nil sync/stop when API missing, got synced=%v stop=%v", synced != nil, stop != nil)
	}
	if _, ok := getRolloutSupport(); ok {
		t.Fatal("support should remain disabled")
	}
}

func TestMaybeStartRolloutInformer_startsAndStopDisablesCleanly(t *testing.T) {
	t.Cleanup(func() { setRolloutSupport(nil, false) })

	clientset := fake.NewSimpleClientset()
	disco := clientset.Discovery().(*fakediscovery.FakeDiscovery)
	disco.Resources = []*metav1.APIResourceList{{
		GroupVersion: "argoproj.io/v1alpha1",
		APIResources: []metav1.APIResource{{
			Name: "rollouts", Kind: "Rollout", Namespaced: true,
			Verbs: []string{"get", "list", "watch"},
		}},
	}}
	u := newUnstructuredRollout("default", "canary-api", 2,
		map[string]string{"app": "canary-api"},
		map[string]string{"app": "canary-api"},
		2, 2,
	)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{getRolloutGVR(): "RolloutList"},
		u,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	synced, stop := maybeStartRolloutInformer(ctx, clientset, dyn)
	if synced == nil || stop == nil {
		t.Fatal("expected informer sync func and stop cancel")
	}
	if _, ok := getRolloutSupport(); !ok {
		t.Fatal("expected Rollout support enabled after successful start")
	}

	// Mimic sync-timeout recovery: disable support then stop informer.
	setRolloutSupport(nil, false)
	stop()
	if _, ok := getRolloutSupport(); ok {
		t.Fatal("expected support disabled after stop path")
	}
}

func newUnstructuredRollout(namespace, name string, replicas int32, matchLabels, podLabels map[string]string, ready, available int32) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": rolloutGroup + "/" + rolloutVersionPreferred,
			"kind":       "Rollout",
			"metadata": map[string]interface{}{
				"namespace": namespace,
				"name":      name,
			},
			"spec": map[string]interface{}{
				"replicas": int64(replicas),
				"selector": map[string]interface{}{
					"matchLabels": stringMapToInterfaceMap(matchLabels),
				},
				"template": map[string]interface{}{
					"metadata": map[string]interface{}{
						"labels": stringMapToInterfaceMap(podLabels),
					},
				},
			},
			"status": map[string]interface{}{
				"readyReplicas":     int64(ready),
				"availableReplicas": int64(available),
			},
		},
	}
}

func stringMapToInterfaceMap(in map[string]string) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
