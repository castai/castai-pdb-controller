package main

import (
	"testing"

	policyv1 "k8s.io/api/policy/v1"
)

func TestParseUnhealthyPodEvictionPolicy(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want *policyv1.UnhealthyPodEvictionPolicyType
	}{
		{"empty", "", nil},
		{"spaces", "  ", nil},
		{"AlwaysAllow", "AlwaysAllow", ptrPolicy(policyv1.AlwaysAllow)},
		{"alwaysallow_lower", "alwaysallow", ptrPolicy(policyv1.AlwaysAllow)},
		{"IfHealthyBudget", "IfHealthyBudget", ptrPolicy(policyv1.IfHealthyBudget)},
		{"ifhealthybudget_lower", "ifhealthybudget", ptrPolicy(policyv1.IfHealthyBudget)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseUnhealthyPodEvictionPolicy(tc.in)
			if !unhealthyPodEvictionPoliciesEqual(got, tc.want) {
				t.Fatalf("parseUnhealthyPodEvictionPolicy(%q) = %v want %v", tc.in, strPtr(got), strPtr(tc.want))
			}
		})
	}
	if p := parseUnhealthyPodEvictionPolicy("bogus"); p != nil {
		t.Fatalf("invalid value should return nil, got %v", *p)
	}
}

func ptrPolicy(p policyv1.UnhealthyPodEvictionPolicyType) *policyv1.UnhealthyPodEvictionPolicyType {
	return &p
}

func strPtr(p *policyv1.UnhealthyPodEvictionPolicyType) string {
	if p == nil {
		return "<nil>"
	}
	return string(*p)
}

func TestUnhealthyPodEvictionPoliciesEqual(t *testing.T) {
	a := policyv1.AlwaysAllow
	b := policyv1.IfHealthyBudget
	if !unhealthyPodEvictionPoliciesEqual(nil, nil) {
		t.Fatal("nil nil")
	}
	if unhealthyPodEvictionPoliciesEqual(&a, nil) {
		t.Fatal("a nil")
	}
	if !unhealthyPodEvictionPoliciesEqual(&a, &a) {
		t.Fatal("a a")
	}
	if unhealthyPodEvictionPoliciesEqual(&a, &b) {
		t.Fatal("a b")
	}
}
