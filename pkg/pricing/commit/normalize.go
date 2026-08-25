package commit

import (
	"strconv"
	"strings"
)

// FamilyOf returns the instance family of an instance type: "m5" for
// "m5.xlarge", "u7i-6tb" for "u7i-6tb.112xlarge". Returns "" if the type has
// no "." separator.
func FamilyOf(instanceType string) string {
	i := strings.IndexByte(instanceType, '.')
	if i <= 0 {
		return ""
	}
	return strings.ToLower(instanceType[:i])
}

// SizeOf returns the instance size of an instance type: "xlarge" for
// "m5.xlarge", "metal" for "i3.metal".
func SizeOf(instanceType string) string {
	i := strings.IndexByte(instanceType, '.')
	if i < 0 || i+1 >= len(instanceType) {
		return ""
	}
	return strings.ToLower(instanceType[i+1:])
}

// sizeUnits is the normalization-factor table from apply_ri.html, verbatim.
// The discounted rate of a regional Reserved Instance is applied to the
// normalized usage of the family using this scale.
var sizeUnits = map[string]float64{
	"nano": 0.25, "micro": 0.5, "small": 1, "medium": 2, "large": 4,
	"xlarge": 8, "2xlarge": 16, "3xlarge": 24, "4xlarge": 32, "6xlarge": 48,
	"8xlarge": 64, "9xlarge": 72, "10xlarge": 80, "12xlarge": 96,
	"16xlarge": 128, "18xlarge": 144, "24xlarge": 192, "32xlarge": 256,
	"48xlarge": 384, "56xlarge": 448, "96xlarge": 768, "112xlarge": 896,
}

// metalUnits is the bare-metal table from apply_ri.html. `metal` has no single
// normalization factor: a bare metal instance takes the factor of the
// equivalent virtualized size in its own family, so the mapping is per
// instance type, not per size.
//
// Transcribed as documented, including `r6d.metal` — which looks like an AWS
// typo for r6i/r6id but is reproduced rather than guessed at. An unlisted
// `.metal` type resolves to "unknown", which costs nothing: it merely denies
// that RI size flexibility, so the reservation applies on exact match only.
var metalUnits = map[string]float64{
	"a1.metal":   32,
	"m5zn.metal": 96, "x2iezn.metal": 96, "z1d.metal": 96,
	"c6g.metal": 128, "c6gd.metal": 128, "i3.metal": 128, "m6g.metal": 128,
	"m6gd.metal": 128, "r6g.metal": 128, "r6gd.metal": 128, "x2gd.metal": 128,
	"c5n.metal": 144,
	"c5.metal":  192, "c5d.metal": 192, "i3en.metal": 192, "m5.metal": 192,
	"m5d.metal": 192, "m5dn.metal": 192, "m5n.metal": 192, "r5.metal": 192,
	"r5b.metal": 192, "r5d.metal": 192, "r5dn.metal": 192, "r5n.metal": 192,
	"c6i.metal": 256, "c6id.metal": 256, "m6i.metal": 256, "m6id.metal": 256,
	"r6d.metal": 256, "r6id.metal": 256,
	"u-18tb1.metal": 448, "u-24tb1.metal": 448,
	"u-6tb1.metal": 896, "u-9tb1.metal": 896, "u-12tb1.metal": 896,
}

// sizeFlexExcluded lists the families AWS excludes from Reserved Instance size
// flexibility, from apply_ri.html's limitations section. The design doc's
// shorthand is "not G/P/Inf families"; this is the precise list, and it is a
// closed set rather than a prefix rule so that (for example) g5g is excluded
// while m6g is not.
var sizeFlexExcluded = map[string]bool{
	"g4ad": true, "g4dn": true, "g5": true, "g5g": true, "g6": true,
	"g6e": true, "g6f": true, "gr6": true, "gr6f": true, "hpc7a": true,
	"p5": true, "inf1": true, "inf2": true, "u7i-6tb": true, "u7i-8tb": true,
}

// SizeFlexExcluded reports whether a family is barred from RI size
// flexibility. See sizeFlexExcluded for the source list.
func SizeFlexExcluded(family string) bool { return sizeFlexExcluded[strings.ToLower(family)] }

// NormalizationUnits returns the normalization factor for an instance size.
//
// Sizes documented by AWS come from the table verbatim. An undocumented
// "<N>xlarge" is extrapolated as 8×N, which reproduces every documented entry
// from xlarge (8) through 112xlarge (896) exactly; this keeps a newly launched
// size from silently losing its RI coverage. Anything else — including a bare
// "metal", which is family-dependent — returns ok=false. Use [InstanceUnits]
// when you have a full instance type.
func NormalizationUnits(size string) (float64, bool) {
	size = strings.ToLower(strings.TrimSpace(size))
	if u, ok := sizeUnits[size]; ok {
		return u, true
	}
	if n, ok := strings.CutSuffix(size, "xlarge"); ok && n != "" {
		if v, err := strconv.Atoi(n); err == nil && v > 0 && v <= 1<<20 {
			return 8 * float64(v), true
		}
	}
	return 0, false
}

// InstanceUnits returns the normalization units of one instance-hour of the
// given instance type — "m5.xlarge" → 8, "i3.metal" → 128. ok=false means the
// type is unrecognized, in which case the caller must fall back to exact
// instance-type matching rather than guess.
func InstanceUnits(instanceType string) (float64, bool) {
	t := strings.ToLower(strings.TrimSpace(instanceType))
	if u, ok := metalUnits[t]; ok {
		return u, true
	}
	size := SizeOf(t)
	if size == "" || size == "metal" {
		return 0, false
	}
	return NormalizationUnits(size)
}

// linuxAliases are the platform spellings that mean "Linux/UNIX" for RI size
// flexibility. AWS's own examples use "Linux" and "Amazon Linux"
// interchangeably; Cost and Usage Reports say "Linux/UNIX". Windows, RHEL and
// SUSE are deliberately absent — they have no size flexibility.
var linuxAliases = map[string]bool{
	"": true, "linux": true, "unix": true, "linux/unix": true,
	"amazon linux": true, "amazon linux 2": true, "amazon linux 2023": true,
	"linux/unix (amazon vpc)": true,
}

// NormalizePlatform folds equivalent platform spellings so that matching is
// not a string-equality coin flip. An empty platform normalizes to
// Linux/UNIX: that is the overwhelmingly common case for the fleets Kilter
// observes, and collectors that do not know the platform should not
// accidentally deny a Linux RI its match. Unrecognized platforms are
// lower-cased and passed through, so they match only themselves.
func NormalizePlatform(p string) string {
	k := strings.ToLower(strings.TrimSpace(p))
	if linuxAliases[k] {
		return PlatformLinux
	}
	return k
}

// NormalizeTenancy folds tenancy spellings; empty means default tenancy.
// Dedicated and host tenancy have no RI size flexibility.
func NormalizeTenancy(t string) string {
	k := strings.ToLower(strings.TrimSpace(t))
	if k == "" || k == "default" || k == "shared" {
		return TenancyDefault
	}
	return k
}
