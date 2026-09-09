package router

import (
	"testing"

	"github.com/aws-samples/sample-llm-gateway/internal/controlplane"
)

func TestAttemptsOrderByPriorityThenWeight(t *testing.T) {
	r := New()
	r.Load(&controlplane.Routes{Version: "v1", Models: []controlplane.ModelRoute{{
		ModelCode: "m",
		Providers: []controlplane.ProviderRoute{
			{ProviderCode: "backup", ProviderModelCode: "b", Priority: 20, Weight: 100},
			{ProviderCode: "a1", ProviderModelCode: "a1", Priority: 10, Weight: 90},
			{ProviderCode: "a2", ProviderModelCode: "a2", Priority: 10, Weight: 10},
		},
	}}})
	firstCounts := map[string]int{}
	for i := 0; i < 2000; i++ {
		at := r.Attempts("m")
		if len(at) != 3 {
			t.Fatalf("want 3 attempts, got %d", len(at))
		}
		if at[2].ProviderCode != "backup" {
			t.Fatalf("lower priority tier must come last: %+v", at)
		}
		firstCounts[at[0].ProviderCode]++
	}
	if firstCounts["a1"] < 1600 || firstCounts["a2"] < 100 {
		t.Errorf("weighted pick skewed unexpectedly: %v", firstCounts)
	}
	if r.Attempts("unknown") != nil {
		t.Error("unknown model should yield nil")
	}
	if got := r.Models(); len(got) != 1 || got[0] != "m" {
		t.Errorf("Models() = %v", got)
	}
}
