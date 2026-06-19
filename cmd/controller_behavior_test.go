package main

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
)

// Regression tests for core PDB controller behavior (values, annotations, exclusions, unhealthy policy resolution).

func TestParsePDBValue(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want *intstr.IntOrString
	}{
		{"", nil},
		{"  ", nil},
		{"1", &intstr.IntOrString{Type: intstr.Int, IntVal: 1}},
		{" 2 ", &intstr.IntOrString{Type: intstr.Int, IntVal: 2}},
		{"50%", &intstr.IntOrString{Type: intstr.String, StrVal: "50%"}},
		{"100%", &intstr.IntOrString{Type: intstr.String, StrVal: "100%"}},
	}
	for _, tc := range cases {
		got := parsePDBValue(tc.in)
		if (got == nil) != (tc.want == nil) {
			t.Fatalf("parsePDBValue(%q) nil mismatch: got %v want %v", tc.in, got, tc.want)
		}
		if got != nil && tc.want != nil {
			if got.Type != tc.want.Type || got.IntVal != tc.want.IntVal || got.StrVal != tc.want.StrVal {
				t.Fatalf("parsePDBValue(%q) = %#v want %#v", tc.in, got, tc.want)
			}
		}
	}
	if parsePDBValue("not-a-number") != nil {
		t.Fatal("invalid int should return nil")
	}
	got := parsePDBValue("  25%  ")
	if got == nil || got.Type != intstr.String || got.StrVal != "25%" {
		t.Fatalf("trim percent: got %#v", got)
	}
}

func TestIsWorkloadExcluded_noRulesNeverExcludes(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)
	if isWorkloadExcluded("any", "thing", nil) {
		t.Fatal("with no exclusion rules, nothing should be excluded")
	}
}

func TestIsWorkloadExcluded_nameAndNamespaceBothRequired(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)
	defaultPDBConfigLock.Lock()
	defaultPDBConfig.Exclusions = []ExclusionRule{
		{NamespaceRegex: "^prod$", NameRegex: "^api$", Labels: map[string]string{}},
	}
	defaultPDBConfigLock.Unlock()
	if !isWorkloadExcluded("prod", "api", nil) {
		t.Fatal("prod/api should match")
	}
	if isWorkloadExcluded("prod", "api-v2", nil) {
		t.Fatal("prod/api-v2 should not match name regex")
	}
	if isWorkloadExcluded("staging", "api", nil) {
		t.Fatal("staging/api should not match namespace regex")
	}
}

func TestParseDurationFromConfigMap(t *testing.T) {
	t.Parallel()
	def := 5 * time.Minute
	data := map[string]string{
		"ok":    "2m",
		"bad":   "nope",
		"zero":  "0s",
		"empty": "",
	}
	if d := parseDurationFromConfigMap(data, "ok", def); d != 2*time.Minute {
		t.Fatalf("ok: got %v", d)
	}
	if d := parseDurationFromConfigMap(data, "missing", def); d != def {
		t.Fatalf("missing: got %v want %v", d, def)
	}
	if d := parseDurationFromConfigMap(data, "bad", def); d != def {
		t.Fatalf("bad: got %v want %v", d, def)
	}
	if d := parseDurationFromConfigMap(data, "zero", def); d != def {
		t.Fatalf("zero: got %v want %v", d, def)
	}
	if d := parseDurationFromConfigMap(data, "empty", def); d != def {
		t.Fatalf("empty: got %v want %v", d, def)
	}
}

func TestHasCustomPDBAnnotations_unhealthyKeyPresentCountsAsCustom(t *testing.T) {
	t.Parallel()
	// Key presence drives reconcile eligibility even if value is empty (caller may fix later).
	if !hasCustomPDBAnnotations(map[string]string{annotationUnhealthyPodEvictionPolicy: ""}) {
		t.Fatal("empty unhealthy annotation value should still count as custom PDB annotations")
	}
}

func TestHasCustomPDBAnnotations(t *testing.T) {
	t.Parallel()
	if hasCustomPDBAnnotations(nil) {
		t.Fatal("nil map")
	}
	if hasCustomPDBAnnotations(map[string]string{}) {
		t.Fatal("empty map")
	}
	if !hasCustomPDBAnnotations(map[string]string{annotationMinAvailable: "1"}) {
		t.Fatal("minAvailable")
	}
	if !hasCustomPDBAnnotations(map[string]string{annotationMaxUnavailable: "1"}) {
		t.Fatal("maxUnavailable")
	}
	if !hasCustomPDBAnnotations(map[string]string{annotationUnhealthyPodEvictionPolicy: "AlwaysAllow"}) {
		t.Fatal("unhealthy policy")
	}
	if hasCustomPDBAnnotations(map[string]string{"other": "x"}) {
		t.Fatal("unrelated key")
	}
}

func TestHasBypassAnnotation(t *testing.T) {
	t.Parallel()
	if hasBypassAnnotation(nil) || hasBypassAnnotation(map[string]string{}) {
		t.Fatal("nil/empty")
	}
	if hasBypassAnnotation(map[string]string{annotationBypass: "false"}) {
		t.Fatal("false should not bypass")
	}
	if !hasBypassAnnotation(map[string]string{annotationBypass: "true"}) {
		t.Fatal("true should bypass")
	}
	if hasBypassAnnotation(map[string]string{annotationBypass: "TRUE"}) {
		t.Fatal("only exact lowercase true bypasses")
	}
}

func TestIsWorkloadExcluded_namespaceAndNameRegex(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)

	defaultPDBConfigLock.Lock()
	defaultPDBConfig.Exclusions = []ExclusionRule{
		{NamespaceRegex: "^(istio-system|castai-agent)$", NameRegex: "", Labels: map[string]string{}},
		{NamespaceRegex: "", NameRegex: `.*-temp$`, Labels: map[string]string{}},
	}
	defaultPDBConfigLock.Unlock()

	if !isWorkloadExcluded("istio-system", "istiod", map[string]string{"app": "x"}) {
		t.Fatal("istio-system should match namespace rule")
	}
	if !isWorkloadExcluded("castai-agent", "agent", nil) {
		t.Fatal("castai-agent should match namespace rule")
	}
	if isWorkloadExcluded("app-ns", "api", nil) {
		t.Fatal("app-ns/api should not be excluded")
	}
	if !isWorkloadExcluded("app-ns", "api-temp", nil) {
		t.Fatal("api-temp should match name suffix rule")
	}
	if isWorkloadExcluded("app-ns", "api", map[string]string{"env": "prod"}) {
		t.Fatal("api without -temp should not match name-only rule")
	}
}

func TestIsWorkloadExcluded_labelRule(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)

	defaultPDBConfigLock.Lock()
	defaultPDBConfig.Exclusions = []ExclusionRule{
		{
			NamespaceRegex: "^app$",
			NameRegex:      "",
			Labels:         map[string]string{"skip-pdb": "^true$"},
		},
	}
	defaultPDBConfigLock.Unlock()

	if !isWorkloadExcluded("app", "svc", map[string]string{"skip-pdb": "true"}) {
		t.Fatal("label skip-pdb=true should exclude")
	}
	if isWorkloadExcluded("app", "svc", map[string]string{"skip-pdb": "false"}) {
		t.Fatal("skip-pdb=false should not exclude")
	}
	if isWorkloadExcluded("other", "svc", map[string]string{"skip-pdb": "true"}) {
		t.Fatal("wrong namespace should not match")
	}
}

func TestResolveUnhealthyPodEvictionPolicy_defaultsAndAnnotation(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)

	if p := resolveUnhealthyPodEvictionPolicy(nil); p != nil {
		t.Fatalf("no default: want nil, got %v", p)
	}

	aa := policyv1.AlwaysAllow
	defaultPDBConfigLock.Lock()
	defaultPDBConfig.UnhealthyPodEvictionPolicy = &aa
	defaultPDBConfigLock.Unlock()

	if p := resolveUnhealthyPodEvictionPolicy(nil); p == nil || *p != policyv1.AlwaysAllow {
		t.Fatalf("default AlwaysAllow: got %v", p)
	}

	ann := map[string]string{annotationUnhealthyPodEvictionPolicy: "IfHealthyBudget"}
	if p := resolveUnhealthyPodEvictionPolicy(ann); p == nil || *p != policyv1.IfHealthyBudget {
		t.Fatalf("annotation override: got %v", p)
	}

	annInvalid := map[string]string{annotationUnhealthyPodEvictionPolicy: "bogus"}
	if p := resolveUnhealthyPodEvictionPolicy(annInvalid); p == nil || *p != policyv1.AlwaysAllow {
		t.Fatalf("invalid annotation should fall back to default, got %v", p)
	}

	// Empty annotation value → invalid parse → fall back to default AlwaysAllow
	annEmpty := map[string]string{annotationUnhealthyPodEvictionPolicy: ""}
	if p := resolveUnhealthyPodEvictionPolicy(annEmpty); p == nil || *p != policyv1.AlwaysAllow {
		t.Fatalf("empty annotation should fall back to default, got %v", p)
	}

	// Whitespace in annotation value is trimmed by parser
	annSpaced := map[string]string{annotationUnhealthyPodEvictionPolicy: "  IfHealthyBudget  "}
	if p := resolveUnhealthyPodEvictionPolicy(annSpaced); p == nil || *p != policyv1.IfHealthyBudget {
		t.Fatalf("trimmed annotation: got %v", p)
	}
}

func TestEnrichSelectorWithAdditionalLabels(t *testing.T) {
	t.Parallel()

	baseSelector := func() *metav1.LabelSelector {
		return &metav1.LabelSelector{
			MatchLabels: map[string]string{"app": "myapp"},
		}
	}

	t.Run("empty additionalKeys returns selector unchanged", func(t *testing.T) {
		sel := baseSelector()
		got := enrichSelectorWithAdditionalLabels(sel, map[string]string{"env": "prod"}, nil)
		if len(got.MatchLabels) != 1 || got.MatchLabels["app"] != "myapp" {
			t.Fatalf("expected selector unchanged, got %v", got.MatchLabels)
		}
	})

	t.Run("empty podTemplateLabels returns selector unchanged", func(t *testing.T) {
		sel := baseSelector()
		got := enrichSelectorWithAdditionalLabels(sel, nil, []string{"env"})
		if len(got.MatchLabels) != 1 || got.MatchLabels["app"] != "myapp" {
			t.Fatalf("expected selector unchanged, got %v", got.MatchLabels)
		}
	})

	t.Run("label present in pod template is added to selector", func(t *testing.T) {
		sel := baseSelector()
		got := enrichSelectorWithAdditionalLabels(sel, map[string]string{"env": "prod", "other": "val"}, []string{"env"})
		if got.MatchLabels["env"] != "prod" {
			t.Fatalf("expected env=prod in selector, got %v", got.MatchLabels)
		}
		if got.MatchLabels["app"] != "myapp" {
			t.Fatalf("original app label should be preserved, got %v", got.MatchLabels)
		}
	})

	t.Run("label absent from pod template is silently skipped", func(t *testing.T) {
		sel := baseSelector()
		got := enrichSelectorWithAdditionalLabels(sel, map[string]string{"other": "val"}, []string{"env"})
		if _, ok := got.MatchLabels["env"]; ok {
			t.Fatalf("env label should not be present when absent from pod template, got %v", got.MatchLabels)
		}
	})

	t.Run("key already in selector MatchLabels is not overwritten", func(t *testing.T) {
		sel := &metav1.LabelSelector{
			MatchLabels: map[string]string{"app": "myapp", "env": "staging"},
		}
		got := enrichSelectorWithAdditionalLabels(sel, map[string]string{"env": "prod"}, []string{"env"})
		if got.MatchLabels["env"] != "staging" {
			t.Fatalf("existing selector key should not be overwritten, got %v", got.MatchLabels)
		}
	})

	t.Run("original selector is not mutated", func(t *testing.T) {
		sel := baseSelector()
		got := enrichSelectorWithAdditionalLabels(sel, map[string]string{"env": "prod"}, []string{"env"})
		if _, ok := sel.MatchLabels["env"]; ok {
			t.Fatal("original selector should not be mutated")
		}
		if got == sel {
			t.Fatal("returned selector should be a copy, not the same pointer")
		}
	})
}

// TestAdditionalSelectorLabels_RealWorld verifies enrichment using realistic
// label data. The deployment selector is {app, release} and the pod template
// carries many labels including role and role-name. When those keys are
// configured as additionalSelectorLabels, the PDB selector should gain them;
// when they are absent from the pod template, the selector should be left
// unchanged.
func TestAdditionalSelectorLabels_RealWorld(t *testing.T) {
	t.Parallel()

	// Labels that live on spec.template.metadata.labels of a typical deployment.
	podTemplateLabels := map[string]string{
		"app":       "my-app",
		"release":   "my-app",
		"chart":     "my-app-1.0.0",
		"heritage":  "Helm",
		"team":      "platform",
		"role":      "worker",
		"role-name": "worker-pool-a",
	}

	// The deployment's spec.selector.matchLabels — the baseline for the PDB.
	baseDeploymentSelector := func() *metav1.LabelSelector {
		return &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"app":     "my-app",
				"release": "my-app",
			},
		}
	}

	t.Run("both role and role-name present in pod template are added to PDB selector", func(t *testing.T) {
		sel := baseDeploymentSelector()
		got := enrichSelectorWithAdditionalLabels(
			sel,
			podTemplateLabels,
			[]string{"role", "role-name"},
		)
		if got.MatchLabels["role"] != "worker" {
			t.Errorf("expected role=worker, got %q", got.MatchLabels["role"])
		}
		if got.MatchLabels["role-name"] != "worker-pool-a" {
			t.Errorf("expected role-name=worker-pool-a, got %q", got.MatchLabels["role-name"])
		}
		// Original deployment selector labels must still be present.
		if got.MatchLabels["app"] != "my-app" || got.MatchLabels["release"] != "my-app" {
			t.Errorf("deployment selector labels should be preserved, got %v", got.MatchLabels)
		}
		// Only the two configured keys should be added — not every pod template label.
		if len(got.MatchLabels) != 4 {
			t.Errorf("expected exactly 4 keys in enriched selector, got %d: %v", len(got.MatchLabels), got.MatchLabels)
		}
	})

	t.Run("only role present in pod template when role-name is absent", func(t *testing.T) {
		// Simulate a deployment whose pod template only has role.
		partialPodLabels := map[string]string{
			"app":     "my-app",
			"release": "my-app",
			"role":    "worker",
			// role-name intentionally omitted
		}
		sel := baseDeploymentSelector()
		got := enrichSelectorWithAdditionalLabels(
			sel,
			partialPodLabels,
			[]string{"role", "role-name"},
		)
		if got.MatchLabels["role"] != "worker" {
			t.Errorf("expected role=worker, got %q", got.MatchLabels["role"])
		}
		if _, ok := got.MatchLabels["role-name"]; ok {
			t.Errorf("role-name should be absent when not on pod template, got %v", got.MatchLabels)
		}
	})

	t.Run("neither configured label in pod template leaves selector unchanged", func(t *testing.T) {
		labelsWithoutConfigured := map[string]string{
			"app":     "my-app",
			"release": "my-app",
			"team":    "platform",
		}
		sel := baseDeploymentSelector()
		got := enrichSelectorWithAdditionalLabels(
			sel,
			labelsWithoutConfigured,
			[]string{"role", "role-name"},
		)
		if len(got.MatchLabels) != 2 {
			t.Errorf("expected selector to stay at 2 keys, got %d: %v", len(got.MatchLabels), got.MatchLabels)
		}
		if got.MatchLabels["app"] != "my-app" || got.MatchLabels["release"] != "my-app" {
			t.Errorf("original deployment selector should be unchanged, got %v", got.MatchLabels)
		}
	})
}

func TestDeleteCastaiPDBIfUnderReplicated(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)

	existingPDB := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-myapp-pdb", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "myapp"}},
			MinAvailable: intstrPtr(intstr.FromInt32(1)),
		},
	}
	clientset := fake.NewSimpleClientset(existingPDB)
	ctx := context.Background()

	two := int32(2)
	if deleteCastaiPDBIfUnderReplicated(ctx, clientset, "default", "myapp", &two) {
		t.Fatal("two replicas should not trigger deletion")
	}
	if _, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(ctx, "castai-myapp-pdb", metav1.GetOptions{}); err != nil {
		t.Fatalf("PDB should still exist with 2 replicas: %v", err)
	}

	one := int32(1)
	if !deleteCastaiPDBIfUnderReplicated(ctx, clientset, "default", "myapp", &one) {
		t.Fatal("single replica should trigger deletion")
	}
	if _, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(ctx, "castai-myapp-pdb", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("PDB should be deleted for single replica, got err=%v", err)
	}
}

func TestWaitForPDBDeletion_returnsWhenPDBIsGone(t *testing.T) {
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-wait-pdb", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "wait"}},
			MinAvailable: intstrPtr(intstr.FromInt32(1)),
		},
	}
	clientset := fake.NewSimpleClientset(pdb)
	ctx := context.Background()

	if err := clientset.PolicyV1().PodDisruptionBudgets("default").Delete(ctx, "castai-wait-pdb", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("setup delete: %v", err)
	}

	start := time.Now()
	waitForPDBDeletion(ctx, clientset, "default", "castai-wait-pdb")
	if time.Since(start) > pdbDeletionPollTimeout {
		t.Fatalf("waitForPDBDeletion took longer than timeout")
	}
}

func TestUpdateExistingPDB_deletesWhenScaledBelowTwoReplicas(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)

	one := int32(1)
	existingPDB := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-myapp-pdb", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "myapp"}},
			MinAvailable: intstrPtr(intstr.FromInt32(1)),
		},
	}
	clientset := fake.NewSimpleClientset(existingPDB)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "myapp"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "myapp"}},
			},
		},
	}

	updateExistingPDB(context.Background(), clientset, existingPDB, nil, &one, "default", "myapp", dep)
	if _, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-myapp-pdb", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected PDB to be deleted on scale-down, got err=%v", err)
	}
}

func TestUpdateExistingPDB_recreatesWhenSelectorChanges(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)

	defaultPDBConfigLock.Lock()
	defaultPDBConfig.AdditionalSelectorLabels = []string{"role"}
	defaultPDBConfigLock.Unlock()

	two := int32(2)
	existingPDB := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-myapp-pdb", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "myapp"}},
			MinAvailable: intstrPtr(intstr.FromInt32(1)),
		},
	}
	clientset := fake.NewSimpleClientset(existingPDB)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &two,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "myapp"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "myapp", "role": "worker"}},
			},
		},
	}

	updateExistingPDB(context.Background(), clientset, existingPDB, nil, &two, "default", "myapp", dep)

	got, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-myapp-pdb", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected recreated PDB, got err=%v", err)
	}
	if got.Spec.Selector == nil || got.Spec.Selector.MatchLabels["role"] != "worker" {
		t.Fatalf("expected enriched selector with role=worker, got %#v", got.Spec.Selector)
	}
}

func intstrPtr(v intstr.IntOrString) *intstr.IntOrString {
	return &v
}
