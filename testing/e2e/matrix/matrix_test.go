package matrix

import (
	"reflect"
	"testing"
)

func TestGenerateIsPairwiseComplete(t *testing.T) {
	t.Parallel()

	combos := Generate()
	if len(combos) == 0 {
		t.Fatal("Generate returned no combos")
	}
	t.Logf("generated %d combos from %d valid combinations and full cross-product of %d", len(combos), ValidCrossProductCount(), FullCrossProductCount())

	seenCombos := make(map[string]struct{}, len(combos))
	for _, combo := range combos {
		if IsExcluded(combo) {
			t.Fatalf("excluded combo was emitted: %s (%s)", combo, ExclusionReason(combo))
		}
		if _, exists := seenCombos[combo.Key()]; exists {
			t.Fatalf("duplicate combo emitted: %s", combo)
		}
		seenCombos[combo.Key()] = struct{}{}
	}

	valid := validCrossProduct()
	required := requiredPairs(valid)
	covered := make(map[valuePair]struct{})
	for _, combo := range combos {
		for pair := range comboPairs(combo) {
			covered[pair] = struct{}{}
		}
	}

	for pair := range required {
		if _, exists := covered[pair]; !exists {
			t.Fatalf("pair is not covered: axes %d/%d values %q/%q",
				pair.firstAxis, pair.secondAxis, pair.first, pair.second)
		}
	}
}

func TestGenerateIsDeterministic(t *testing.T) {
	t.Parallel()

	first := Generate()
	second := Generate()
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("Generate returned different output on repeated calls")
	}
}

func TestGenerateRespectsKnownExclusions(t *testing.T) {
	t.Parallel()

	cases := []Combo{
		{Mode: ModeBGP, Function: FunctionSvc, Family: FamilyV4, Election: ElectionNone, Shape: ShapeStaticPod, Provider: ProviderSlices, ETP: ETPCluster},
		{Mode: ModeWireGuard, Function: FunctionCP, Family: FamilyV4, Election: ElectionGlobal, Shape: ShapeStaticPod, Provider: ProviderSlices, ETP: ETPCluster},
		{Mode: ModeARP, Function: FunctionCP, Family: FamilyV4, Election: ElectionPerService, Shape: ShapeStaticPod, Provider: ProviderSlices, ETP: ETPCluster},
		{Mode: ModeRT, Function: FunctionCP, Family: FamilyV4, Election: ElectionOnDemand, Shape: ShapeStaticPod, Provider: ProviderSlices, ETP: ETPCluster},
	}
	for _, combo := range cases {
		if !IsExcluded(combo) {
			t.Errorf("expected combo to be excluded: %s", combo)
		}
	}

	if IsExcluded(Combo{
		Mode:     ModeRT,
		Function: FunctionBoth,
		Family:   FamilyDual,
		Election: ElectionPerService,
		Shape:    ShapeDaemonSet,
		Provider: ProviderEndpoints,
		ETP:      ETPLocal,
	}) {
		t.Error("valid RT service-election combo was excluded")
	}
}

func TestFullCrossProductCount(t *testing.T) {
	if got, want := FullCrossProductCount(), 1152; got != want {
		t.Fatalf("FullCrossProductCount() = %d, want %d", got, want)
	}
}
