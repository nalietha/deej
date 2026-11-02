package deej

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/omriharel/deej/pkg/deej/util"
)

const (
	potTypeSlider = "slider"
	potTypeKnob   = "knob"

	potCurveLinear = "linear"
	potCurveAudio  = "audio" // squared response - more precision at quiet volumes

	potRawMin = 0
	potRawMax = 1023

	defaultProfileName = "default"

	maxProfileNameLength = 40
)

// PotConfig describes a single physical potentiometer (slider or knob) within a profile
type PotConfig struct {
	Name    string     `yaml:"name,omitempty" json:"name"`
	Type    string     `yaml:"type,omitempty" json:"type"`
	Color   string     `yaml:"color,omitempty" json:"color"`
	Invert  bool       `yaml:"invert,omitempty" json:"invert"`
	Curve   string     `yaml:"curve,omitempty" json:"curve"`
	Min     int        `yaml:"min,omitempty" json:"min"`
	Max     int        `yaml:"max,omitempty" json:"max"`
	Targets stringList `yaml:"targets" json:"targets"`
}

// Profile is a named set of pot configurations that can be switched between on the fly
type Profile struct {
	// process names that, while running, cause this profile to activate automatically
	AutoActivate stringList         `yaml:"auto_activate,omitempty" json:"auto_activate"`
	Pots         map[int]*PotConfig `yaml:"pots" json:"pots"`
}

// stringList accepts either a single string or a list of strings, in both YAML and JSON
type stringList []string

func (s *stringList) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var list []string
	if err := unmarshal(&list); err == nil {
		*s = list
		return nil
	}

	var single string
	if err := unmarshal(&single); err != nil {
		return err
	}

	*s = stringList{single}
	return nil
}

func (s *stringList) UnmarshalJSON(data []byte) error {
	var list []string
	if err := json.Unmarshal(data, &list); err == nil {
		*s = list
		return nil
	}

	var single string
	if err := json.Unmarshal(data, &single); err != nil {
		return err
	}

	*s = stringList{single}
	return nil
}

func (s stringList) MarshalJSON() ([]byte, error) {
	if s == nil {
		return []byte("[]"), nil
	}

	return json.Marshal([]string(s))
}

// cleaned trims every entry and drops empty ones and duplicates (case-insensitive)
func (s stringList) cleaned() stringList {
	result := stringList{}
	seen := map[string]bool{}

	for _, entry := range s {
		entry = strings.TrimSpace(entry)
		key := strings.ToLower(entry)

		if entry == "" || seen[key] {
			continue
		}

		seen[key] = true
		result = append(result, entry)
	}

	return result
}

func (p *PotConfig) normalize() {
	p.Name = strings.TrimSpace(p.Name)
	p.Type = strings.ToLower(strings.TrimSpace(p.Type))
	p.Curve = strings.ToLower(strings.TrimSpace(p.Curve))
	p.Targets = p.Targets.cleaned()

	if p.Type != potTypeKnob {
		p.Type = potTypeSlider
	}

	if p.Curve != potCurveAudio {
		p.Curve = potCurveLinear
	}

	// an omitted max means "uncalibrated"
	if p.Max == 0 {
		p.Max = potRawMax
	}

	p.Min = clampInt(p.Min, potRawMin, potRawMax)
	p.Max = clampInt(p.Max, potRawMin, potRawMax)

	// calibration needs some actual travel to be meaningful
	if p.Max-p.Min < 16 {
		p.Min, p.Max = potRawMin, potRawMax
	}
}

// apply turns a raw reading (0-1023) into a volume scalar according to this pot's calibration, direction and curve
func (p PotConfig) apply(raw int) float32 {
	v := float64(raw-p.Min) / float64(p.Max-p.Min)
	v = math.Max(0, math.Min(1, v))

	if p.Invert {
		v = 1 - v
	}

	if p.Curve == potCurveAudio {
		v = v * v
	}

	return util.NormalizeScalar(float32(v))
}

func (pr *Profile) normalize() {
	pr.AutoActivate = pr.AutoActivate.cleaned()

	if pr.Pots == nil {
		pr.Pots = map[int]*PotConfig{}
	}

	for idx, pot := range pr.Pots {
		if pot == nil || idx < 0 {
			delete(pr.Pots, idx)
			continue
		}

		pot.normalize()
	}
}

func validateProfileName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("profile name can't be empty")
	}

	if len(name) > maxProfileNameLength {
		return fmt.Errorf("profile name too long (max %d characters)", maxProfileNameLength)
	}

	return nil
}

// sortedProfileNames returns profile names alphabetically, with the default profile always first
func sortedProfileNames(profiles map[string]*Profile) []string {
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}

	sort.Slice(names, func(i, j int) bool {
		if names[i] == defaultProfileName || names[j] == defaultProfileName {
			return names[i] == defaultProfileName
		}

		return strings.ToLower(names[i]) < strings.ToLower(names[j])
	})

	return names
}

func newDefaultProfile() *Profile {
	return &Profile{
		Pots: map[int]*PotConfig{
			0: {Name: "Master", Targets: stringList{masterSessionName}},
			1: {Name: "Browser"},
			2: {Name: "Music"},
			3: {Name: "Voice"},
			4: {Name: "Mic", Targets: stringList{inputSessionName}},
		},
	}
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}

	if v > hi {
		return hi
	}

	return v
}
