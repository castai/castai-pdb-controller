package main

import (
	"testing"
	"time"

	policyv1 "k8s.io/api/policy/v1"
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
