package main

import (
	"testing"
	"time"

	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
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

// TestAdditionalSelectorLabels_ShiptRealWorld verifies enrichment using realistic
// label data mirroring the surge-pay deployment. The deployment selector is
// {app, release} and the pod template carries many labels including
// shipt-app-role and shipt-app-role-name. When those keys are configured as
// additionalSelectorLabels, the PDB selector should gain them; when they are
// absent from the pod template, the selector should be left unchanged.
func TestAdditionalSelectorLabels_ShiptRealWorld(t *testing.T) {
	t.Parallel()

	// Labels that live on spec.template.metadata.labels of the surge-pay deployment.
	surgepayPodTemplateLabels := map[string]string{
		"app":                          "surge-pay",
		"release":                      "surge-pay",
		"chart":                        "surge-pay-1.0.0",
		"heritage":                     "Helm",
		"pipeline":                     "gitlab-ci",
		"team":                         "payments",
		"shipt-app-role":               "worker",
		"shipt-app-role-name":          "aggregated-bundles-worker",
		"tags.datadoghq.com/env":       "prod",
		"tags.datadoghq.com/service":   "surge-pay",
		"tags.datadoghq.com/version":   "abc123",
	}

	// The deployment's spec.selector.matchLabels — the baseline for the PDB.
	baseDeploymentSelector := func() *metav1.LabelSelector {
		return &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"app":     "surge-pay",
				"release": "surge-pay",
			},
		}
	}

	t.Run("both shipt-app-role and shipt-app-role-name present in pod template are added to PDB selector", func(t *testing.T) {
		sel := baseDeploymentSelector()
		got := enrichSelectorWithAdditionalLabels(
			sel,
			surgepayPodTemplateLabels,
			[]string{"shipt-app-role", "shipt-app-role-name"},
		)
		if got.MatchLabels["shipt-app-role"] != "worker" {
			t.Errorf("expected shipt-app-role=worker, got %q", got.MatchLabels["shipt-app-role"])
		}
		if got.MatchLabels["shipt-app-role-name"] != "aggregated-bundles-worker" {
			t.Errorf("expected shipt-app-role-name=aggregated-bundles-worker, got %q", got.MatchLabels["shipt-app-role-name"])
		}
		// Original deployment selector labels must still be present.
		if got.MatchLabels["app"] != "surge-pay" || got.MatchLabels["release"] != "surge-pay" {
			t.Errorf("deployment selector labels should be preserved, got %v", got.MatchLabels)
		}
		// Only the two configured keys should be added — not every pod template label.
		if len(got.MatchLabels) != 4 {
			t.Errorf("expected exactly 4 keys in enriched selector, got %d: %v", len(got.MatchLabels), got.MatchLabels)
		}
	})

	t.Run("only shipt-app-role present in pod template when shipt-app-role-name is absent", func(t *testing.T) {
		// Simulate a deployment whose pod template only has shipt-app-role.
		partialPodLabels := map[string]string{
			"app":            "surge-pay",
			"release":        "surge-pay",
			"shipt-app-role": "worker",
			// shipt-app-role-name intentionally omitted
		}
		sel := baseDeploymentSelector()
		got := enrichSelectorWithAdditionalLabels(
			sel,
			partialPodLabels,
			[]string{"shipt-app-role", "shipt-app-role-name"},
		)
		if got.MatchLabels["shipt-app-role"] != "worker" {
			t.Errorf("expected shipt-app-role=worker, got %q", got.MatchLabels["shipt-app-role"])
		}
		if _, ok := got.MatchLabels["shipt-app-role-name"]; ok {
			t.Errorf("shipt-app-role-name should be absent when not on pod template, got %v", got.MatchLabels)
		}
	})

	t.Run("neither shipt label in pod template leaves selector unchanged", func(t *testing.T) {
		labelsWithoutShipt := map[string]string{
			"app":     "surge-pay",
			"release": "surge-pay",
			"team":    "payments",
		}
		sel := baseDeploymentSelector()
		got := enrichSelectorWithAdditionalLabels(
			sel,
			labelsWithoutShipt,
			[]string{"shipt-app-role", "shipt-app-role-name"},
		)
		if len(got.MatchLabels) != 2 {
			t.Errorf("expected selector to stay at 2 keys, got %d: %v", len(got.MatchLabels), got.MatchLabels)
		}
		if got.MatchLabels["app"] != "surge-pay" || got.MatchLabels["release"] != "surge-pay" {
			t.Errorf("original deployment selector should be unchanged, got %v", got.MatchLabels)
		}
	})
}
