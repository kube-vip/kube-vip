// Package matrix defines the pairwise e2e configuration matrix.
package matrix

import (
	"fmt"
	"strconv"
	"strings"
)

// Mode selects the mechanism used to advertise a VIP.
type Mode string

const (
	ModeARP       Mode = "arp"
	ModeBGP       Mode = "bgp"
	ModeRT        Mode = "rt"
	ModeWireGuard Mode = "wireguard"
)

// Function selects which kube-vip function a combo exercises.
type Function string

const (
	FunctionCP   Function = "cp"
	FunctionSvc  Function = "svc"
	FunctionBoth Function = "both"
)

// Family selects the Kubernetes and VIP address families.
type Family string

const (
	FamilyV4   Family = "v4"
	FamilyV6   Family = "v6"
	FamilyDual Family = "dual"
)

// Election selects the leader-election arrangement for a combo.
type Election string

const (
	ElectionGlobal     Election = "global"
	ElectionPerService Election = "per-service"
	ElectionOnDemand   Election = "on-demand"
	ElectionNone       Election = "none"
)

// Shape selects whether kube-vip runs as a static pod or a daemonset.
type Shape string

const (
	ShapeStaticPod Shape = "static-pod"
	ShapeDaemonSet Shape = "daemonset"
)

// Provider selects the Kubernetes endpoint resource watched by kube-vip.
type Provider string

const (
	ProviderSlices    Provider = "slices"
	ProviderEndpoints Provider = "endpoints"
)

// ETP selects the Service external traffic policy.
type ETP string

const (
	ETPCluster ETP = "Cluster"
	ETPLocal   ETP = "Local"
)

// Combo is one complete configuration in the matrix.
type Combo struct {
	Mode     Mode
	Function Function
	Family   Family
	Election Election
	Shape    Shape
	Provider Provider
	ETP      ETP
}

// String returns the stable name used in test output and generated ordering.
func (c Combo) String() string {
	return fmt.Sprintf("mode=%s,function=%s,family=%s,election=%s,shape=%s,provider=%s,etp=%s",
		c.Mode, c.Function, c.Family, c.Election, c.Shape, c.Provider, c.ETP)
}

// Key is an alias for String, useful when a combo is used as a map key.
func (c Combo) Key() string {
	return c.String()
}

// Axis describes one named matrix axis. Values are returned as strings so the
// complete axis table can be inspected without exposing mutable package state.
type Axis struct {
	Name   string
	Values []string
}

var axisTable = []Axis{
	{Name: "mode", Values: []string{string(ModeARP), string(ModeBGP), string(ModeRT), string(ModeWireGuard)}},
	{Name: "function", Values: []string{string(FunctionCP), string(FunctionSvc), string(FunctionBoth)}},
	{Name: "family", Values: []string{string(FamilyV4), string(FamilyV6), string(FamilyDual)}},
	{Name: "election", Values: []string{string(ElectionGlobal), string(ElectionPerService), string(ElectionOnDemand), string(ElectionNone)}},
	{Name: "shape", Values: []string{string(ShapeStaticPod), string(ShapeDaemonSet)}},
	{Name: "provider", Values: []string{string(ProviderSlices), string(ProviderEndpoints)}},
	{Name: "etp", Values: []string{string(ETPCluster), string(ETPLocal)}},
}

// Axes returns the ordered axes and a copy of every value list.
func Axes() []Axis {
	axes := make([]Axis, len(axisTable))
	for i, axis := range axisTable {
		axes[i] = Axis{Name: axis.Name, Values: append([]string(nil), axis.Values...)}
	}
	return axes
}

// FullCrossProductCount returns the number of combinations before exclusions
// and pairwise reduction are applied.
func FullCrossProductCount() int {
	count := 1
	for _, axis := range axisTable {
		count *= len(axis.Values)
	}
	return count
}

// CrossProductCount is kept as a descriptive alias for callers reporting the
// unreduced matrix size.
func CrossProductCount() int {
	return FullCrossProductCount()
}

// ValidCrossProductCount returns the number of allowed combinations before
// pairwise reduction is applied.
func ValidCrossProductCount() int {
	return len(validCrossProduct())
}

type exclusionRule struct {
	name  string
	match func(Combo) bool
}

// exclusions are deliberately explicit. They document product limitations
// next to the generator instead of hiding them in the greedy selector.
var exclusions = []exclusionRule{
	{
		name: "bgp does not use the no-election service arrangement",
		match: func(c Combo) bool {
			return c.Mode == ModeBGP && c.Election == ElectionNone
		},
	},
	{
		name: "wireguard currently covers services, not control-plane-only or hybrid combos",
		match: func(c Combo) bool {
			return c.Mode == ModeWireGuard && c.Function != FunctionSvc
		},
	},
	{
		name: "routing-table and BGP service elections require a service function",
		match: func(c Combo) bool {
			return (c.Mode == ModeRT || c.Mode == ModeBGP) && c.Function == FunctionCP &&
				(c.Election == ElectionPerService || c.Election == ElectionOnDemand)
		},
	},
	{
		name: "per-service election requires a service function",
		match: func(c Combo) bool {
			return c.Mode != ModeRT && c.Mode != ModeBGP && c.Function == FunctionCP &&
				(c.Election == ElectionPerService || c.Election == ElectionOnDemand)
		},
	},
}

// ExclusionReason returns the first matching exclusion description, or an
// empty string when the combo is allowed.
func ExclusionReason(c Combo) string {
	for _, rule := range exclusions {
		if rule.match(c) {
			return rule.name
		}
	}
	return ""
}

// IsExcluded reports whether a combo is intentionally omitted from the
// matrix. The exclusions include the RT and BGP service-election constraint:
// those election modes are only meaningful when a service is present.
func IsExcluded(c Combo) bool {
	return ExclusionReason(c) != ""
}

// Valid reports whether a combo is emitted by the generator.
func Valid(c Combo) bool {
	return !IsExcluded(c)
}

// Generate emits a deterministic, pairwise-complete set of valid combos.
//
// It first enumerates the allowed cross-product in axis order. It then uses a
// deterministic greedy set-cover pass: at each step, select the first
// remaining combo that covers the most uncovered value pairs. Pair
// requirements that cannot exist because of an exclusion are not requested;
// every feasible value pair is still covered.
func Generate() []Combo {
	all := validCrossProduct()
	remaining := requiredPairs(all)
	selected := make([]Combo, 0)
	used := make([]bool, len(all))

	for len(remaining) > 0 {
		bestIndex := -1
		bestScore := -1
		for i, combo := range all {
			if used[i] {
				continue
			}

			score := 0
			for pair := range comboPairs(combo) {
				if _, ok := remaining[pair]; ok {
					score++
				}
			}
			if score > bestScore {
				bestIndex = i
				bestScore = score
			}
		}

		if bestIndex < 0 || bestScore == 0 {
			panic("matrix: pairwise requirements cannot be covered")
		}

		used[bestIndex] = true
		combo := all[bestIndex]
		selected = append(selected, combo)
		for pair := range comboPairs(combo) {
			delete(remaining, pair)
		}
	}

	return selected
}

// Combos is a descriptive alias for Generate.
func Combos() []Combo {
	return Generate()
}

type valuePair struct {
	firstAxis  int
	secondAxis int
	first      string
	second     string
}

func validCrossProduct() []Combo {
	combos := make([]Combo, 0, FullCrossProductCount())
	for _, mode := range []Mode{ModeARP, ModeBGP, ModeRT, ModeWireGuard} {
		for _, function := range []Function{FunctionCP, FunctionSvc, FunctionBoth} {
			for _, family := range []Family{FamilyV4, FamilyV6, FamilyDual} {
				for _, election := range []Election{ElectionGlobal, ElectionPerService, ElectionOnDemand, ElectionNone} {
					for _, shape := range []Shape{ShapeStaticPod, ShapeDaemonSet} {
						for _, provider := range []Provider{ProviderSlices, ProviderEndpoints} {
							for _, etp := range []ETP{ETPCluster, ETPLocal} {
								combo := Combo{
									Mode:     mode,
									Function: function,
									Family:   family,
									Election: election,
									Shape:    shape,
									Provider: provider,
									ETP:      etp,
								}
								if Valid(combo) {
									combos = append(combos, combo)
								}
							}
						}
					}
				}
			}
		}
	}
	return combos
}

func comboValues(combo Combo) []string {
	return []string{
		string(combo.Mode),
		string(combo.Function),
		string(combo.Family),
		string(combo.Election),
		string(combo.Shape),
		string(combo.Provider),
		string(combo.ETP),
	}
}

func comboPairs(combo Combo) map[valuePair]struct{} {
	values := comboValues(combo)
	pairs := make(map[valuePair]struct{})
	for firstAxis := 0; firstAxis < len(values); firstAxis++ {
		for secondAxis := firstAxis + 1; secondAxis < len(values); secondAxis++ {
			pairs[valuePair{
				firstAxis:  firstAxis,
				secondAxis: secondAxis,
				first:      values[firstAxis],
				second:     values[secondAxis],
			}] = struct{}{}
		}
	}
	return pairs
}

func requiredPairs(combos []Combo) map[valuePair]struct{} {
	allPairs := make(map[valuePair]struct{})
	for _, combo := range combos {
		for pair := range comboPairs(combo) {
			allPairs[pair] = struct{}{}
		}
	}
	return allPairs
}

// FormatValues is useful to produce compact, stable table entry names.
func FormatValues(values ...string) string {
	cleaned := make([]string, 0, len(values))
	for _, value := range values {
		cleaned = append(cleaned, strings.ReplaceAll(value, " ", "-"))
	}
	return strings.Join(cleaned, "-")
}

// ShardName returns a stable one-based shard label for human-readable output.
func ShardName(index, count int) string {
	return "shard-" + strconv.Itoa(index) + "-of-" + strconv.Itoa(count)
}
