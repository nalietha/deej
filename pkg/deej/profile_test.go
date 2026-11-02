package deej

import (
	"testing"

	"gopkg.in/yaml.v2"
)

func TestPotApply(t *testing.T) {
	cases := []struct {
		name string
		pot  PotConfig
		raw  int
		want float32
	}{
		{"linear full", PotConfig{}, 1023, 1},
		{"linear zero", PotConfig{}, 0, 0},
		{"inverted", PotConfig{Invert: true}, 1023, 0},
		{"audio curve", PotConfig{Curve: potCurveAudio}, 512, 0.25},
		{"calibrated top reaches 100%", PotConfig{Min: 40, Max: 980}, 990, 1},
		{"calibrated bottom reaches 0%", PotConfig{Min: 40, Max: 980}, 30, 0},
	}

	for _, c := range cases {
		c.pot.normalize()
		if got := c.pot.apply(c.raw); got != c.want {
			t.Errorf("%s: apply(%d) = %v, want %v", c.name, c.raw, got, c.want)
		}
	}
}

func TestLegacyConfigMigrates(t *testing.T) {
	raw := `
slider_mapping:
  0: master
  1:
    - chrome.exe
    - firefox.exe
invert_sliders: true
com_port: COM4
`
	file := &ConfigFile{}
	if err := yaml.Unmarshal([]byte(raw), file); err != nil {
		t.Fatal(err)
	}

	file.normalize()

	profile := file.Profiles[defaultProfileName]
	if profile == nil || file.ActiveProfile != defaultProfileName {
		t.Fatalf("expected a default profile to be created and active, got %+v", file)
	}

	if got := profile.Pots[1].Targets; len(got) != 2 || got[1] != "firefox.exe" {
		t.Errorf("pot 1 targets = %v", got)
	}

	if !profile.Pots[0].Invert || file.SliderMapping != nil || file.InvertSliders {
		t.Errorf("legacy keys weren't migrated cleanly: %+v", file)
	}

	if file.COMPort != "COM4" || file.BaudRate != defaultBaudRate || file.NoiseReduction != noiseReductionDefault {
		t.Errorf("connection defaults wrong: %+v", file)
	}
}

func TestExampleConfigParses(t *testing.T) {
	file, err := readConfigFile("../../config.yaml")
	if err != nil {
		t.Fatal(err)
	}

	file.normalize()

	if file.COMPort != autoCOMPort || len(file.Profiles) != 2 || !file.WebUI.Enabled {
		t.Errorf("unexpected example config: %+v", file)
	}

	if pot := file.Profiles["gaming"].Pots[1]; pot.Type != potTypeKnob || len(pot.Targets) != 2 {
		t.Errorf("gaming pot 1 = %+v", pot)
	}
}

func TestSortedProfileNamesDefaultFirst(t *testing.T) {
	names := sortedProfileNames(map[string]*Profile{"zed": {}, "Alpha": {}, defaultProfileName: {}})
	if names[0] != defaultProfileName || names[1] != "Alpha" || names[2] != "zed" {
		t.Errorf("got %v", names)
	}
}
