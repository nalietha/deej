package deej

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"go.uber.org/zap"
	"gopkg.in/yaml.v2"

	"github.com/omriharel/deej/pkg/deej/util"
)

// CanonicalConfig provides application-wide access to configuration fields,
// as well as loading/saving/file watching logic for deej's configuration file
type CanonicalConfig struct {
	SliderMapping *sliderMap

	ConnectionInfo struct {
		COMPort  string
		BaudRate int
	}

	NoiseReductionLevel string

	// set when no config file existed and a fresh one was written on load
	FirstRun bool

	logger             *zap.SugaredLogger
	notifier           Notifier
	stopWatcherChannel chan bool

	reloadConsumers []chan bool

	// guards file, version and the exported fields above
	lock    sync.RWMutex
	file    *ConfigFile
	version uint64

	// serializes saves, so concurrent writers (web UI, profile triggers, tray) can't interleave
	saveLock sync.Mutex

	// self-initiated writes shouldn't bounce back through the file watcher
	ignoreFileEventsUntil time.Time
}

// ConfigFile mirrors the structure of config.yaml. It's also what the web UI reads and writes
type ConfigFile struct {
	COMPort        string              `yaml:"com_port" json:"com_port"`
	BaudRate       int                 `yaml:"baud_rate" json:"baud_rate"`
	NoiseReduction string              `yaml:"noise_reduction" json:"noise_reduction"`
	WebUI          WebUIConfig         `yaml:"web_ui" json:"web_ui"`
	ActiveProfile  string              `yaml:"active_profile" json:"active_profile"`
	Profiles       map[string]*Profile `yaml:"profiles" json:"profiles"`

	// vanilla deej keys - still read, but migrated into a "default" profile and never written back
	SliderMapping map[int]stringList `yaml:"slider_mapping,omitempty" json:"-"`
	InvertSliders bool               `yaml:"invert_sliders,omitempty" json:"-"`
}

// WebUIConfig controls deej's local control panel
type WebUIConfig struct {
	Enabled     bool `yaml:"enabled" json:"enabled"`
	Port        int  `yaml:"port" json:"port"`
	OpenOnStart bool `yaml:"open_on_start" json:"open_on_start"`
}

const (
	userConfigFilepath     = "config.yaml"
	internalConfigFilepath = "preferences.yaml"

	autoCOMPort = "auto"

	defaultBaudRate = 9600
	defaultWebPort  = 7979

	noiseReductionLow     = "low"
	noiseReductionDefault = "default"
	noiseReductionHigh    = "high"

	configFileHeader = `# deej remix configuration
#
# this file is managed by the deej control panel (tray icon -> "Open control panel"),
# but you can still edit it by hand - changes are picked up live. comments below this header aren't kept.
#
# com_port:        "auto" finds your deej by itself, or name one explicitly (COM4, /dev/ttyUSB0)
# noise_reduction: low (excellent hardware), default, or high (bad, noisy hardware)
# pot targets:     process names (chrome.exe), master, mic, system, deej.unmapped, deej.current (windows),
#                  or a device's full name, i.e. "Speakers (Realtek High Definition Audio)" (windows)
# pot type:        slider | knob
# pot curve:       linear | audio (finer control at low volumes)
# pot min/max:     raw calibration (0-1023) for pots that don't quite reach their ends
# auto_activate:   process names - while any of them runs, that profile takes over
# important:       pot indexes start at 0, regardless of which analog pins you're using!

`
)

// has to be defined as a non-constant because we're using path.Join
var internalConfigPath = path.Join(".", logDirectory, internalConfigFilepath)

// NewConfig creates a config instance for the deej object
func NewConfig(logger *zap.SugaredLogger, notifier Notifier) (*CanonicalConfig, error) {
	logger = logger.Named("config")

	cc := &CanonicalConfig{
		logger:             logger,
		notifier:           notifier,
		reloadConsumers:    []chan bool{},
		stopWatcherChannel: make(chan bool),
		SliderMapping:      newSliderMap(),
	}

	logger.Debug("Created config instance")

	return cc, nil
}

// Load reads deej's config files from disk and tries to parse them.
// If no config file exists, a fresh one is created
func (cc *CanonicalConfig) Load() error {
	cc.logger.Debugw("Loading config", "path", userConfigFilepath)

	if !util.FileExists(userConfigFilepath) {
		cc.logger.Infow("Config file not found, creating a fresh one", "path", userConfigFilepath)

		if err := cc.writeFile(defaultConfigFile()); err != nil {
			cc.notifier.Notify("Can't create configuration!",
				fmt.Sprintf("Failed to write %s next to deej, check the logs for details.", userConfigFilepath))

			return fmt.Errorf("write fresh config: %w", err)
		}

		cc.FirstRun = true
	}

	file, err := readConfigFile(userConfigFilepath)
	if err != nil {
		cc.logger.Warnw("Failed to read user config", "error", err)

		// if the error is yaml-format-related, show a sensible error. otherwise, show 'em to the logs
		if strings.Contains(err.Error(), "yaml:") {
			cc.notifier.Notify("Invalid configuration!",
				fmt.Sprintf("Please make sure %s is in a valid YAML format.", userConfigFilepath))
		} else {
			cc.notifier.Notify("Error loading configuration!", "Please check deej's logs for more details.")
		}

		return fmt.Errorf("read user config: %w", err)
	}

	if len(file.SliderMapping) > 0 && len(file.Profiles) > 0 {
		cc.logger.Warn("Config has both slider_mapping and profiles, ignoring slider_mapping")
	}

	file.normalize()
	cc.apply(file)

	cc.logger.Info("Loaded config successfully")
	cc.logger.Infow("Config values",
		"activeProfile", file.ActiveProfile,
		"profiles", len(file.Profiles),
		"sliderMapping", cc.SliderMapping,
		"connectionInfo", cc.ConnectionInfo)

	return nil
}

// Save normalizes, validates and persists the given config, then applies it immediately
func (cc *CanonicalConfig) Save(file *ConfigFile) error {
	cc.saveLock.Lock()
	defer cc.saveLock.Unlock()

	for name := range file.Profiles {
		if err := validateProfileName(name); err != nil {
			return err
		}
	}

	file.normalize()

	if err := cc.writeFile(file); err != nil {
		return err
	}

	cc.apply(file)
	cc.logger.Infow("Saved config", "activeProfile", file.ActiveProfile)
	cc.onConfigReloaded()

	return nil
}

// SetActiveProfile switches to the named profile and persists that choice
func (cc *CanonicalConfig) SetActiveProfile(name string) error {
	file := cc.Snapshot()

	if _, ok := file.Profiles[name]; !ok {
		return fmt.Errorf("no such profile: %s", name)
	}

	if file.ActiveProfile == name {
		return nil
	}

	file.ActiveProfile = name

	return cc.Save(file)
}

// Snapshot returns a deep copy of the current configuration, safe to modify
func (cc *CanonicalConfig) Snapshot() *ConfigFile {
	cc.lock.RLock()
	defer cc.lock.RUnlock()

	// a json round trip is the cheapest way to deep copy maps of pointers. it can't fail for this type
	raw, _ := json.Marshal(cc.file)
	snapshot := &ConfigFile{}
	_ = json.Unmarshal(raw, snapshot)

	return snapshot
}

// Version increments every time the config changes, letting pollers detect changes cheaply
func (cc *CanonicalConfig) Version() uint64 {
	cc.lock.RLock()
	defer cc.lock.RUnlock()

	return cc.version
}

// Connection returns the currently configured serial port and baud rate
func (cc *CanonicalConfig) Connection() (string, int) {
	cc.lock.RLock()
	defer cc.lock.RUnlock()

	return cc.ConnectionInfo.COMPort, cc.ConnectionInfo.BaudRate
}

// NoiseReduction returns the configured noise reduction level
func (cc *CanonicalConfig) NoiseReduction() string {
	cc.lock.RLock()
	defer cc.lock.RUnlock()

	return cc.NoiseReductionLevel
}

// Pot returns the active profile's config for a given pot index, or sane defaults when it isn't configured
func (cc *CanonicalConfig) Pot(idx int) PotConfig {
	cc.lock.RLock()
	defer cc.lock.RUnlock()

	if cc.file != nil {
		if profile, ok := cc.file.Profiles[cc.file.ActiveProfile]; ok {
			if pot, ok := profile.Pots[idx]; ok {
				return *pot
			}
		}
	}

	pot := PotConfig{}
	pot.normalize()

	return pot
}

// SubscribeToChanges allows external components to receive updates when the config is reloaded
func (cc *CanonicalConfig) SubscribeToChanges() chan bool {
	c := make(chan bool)
	cc.reloadConsumers = append(cc.reloadConsumers, c)

	return c
}

// WatchConfigFileChanges starts watching for configuration file changes
// and attempts reloading the config when they happen
func (cc *CanonicalConfig) WatchConfigFileChanges() {
	cc.logger.Debugw("Starting to watch user config file for changes", "path", userConfigFilepath)

	// many editors write a file several times (or write elsewhere and rename) - wait for things to settle
	const settleDelay = time.Millisecond * 150

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		cc.logger.Warnw("Failed to create file watcher", "error", err)
		return
	}

	defer watcher.Close()

	// watch the directory rather than the file itself, so atomic saves (write + rename) are caught too
	configDir := filepath.Dir(userConfigFilepath)
	if err := watcher.Add(configDir); err != nil {
		cc.logger.Warnw("Failed to watch config directory", "error", err)
		return
	}

	var reloadTimer *time.Timer

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}

			if filepath.Base(event.Name) != filepath.Base(userConfigFilepath) ||
				event.Op&(fsnotify.Write|fsnotify.Create) == 0 {
				continue
			}

			if reloadTimer != nil {
				reloadTimer.Stop()
			}

			reloadTimer = time.AfterFunc(settleDelay, cc.reloadFromWatcher)

		case err := <-watcher.Errors:
			cc.logger.Debugw("File watcher error", "error", err)

		case <-cc.stopWatcherChannel:
			cc.logger.Debug("Stopping user config file watcher")
			return
		}
	}
}

// StopWatchingConfigFile signals our filesystem watcher to stop
func (cc *CanonicalConfig) StopWatchingConfigFile() {
	cc.stopWatcherChannel <- true
}

func (cc *CanonicalConfig) reloadFromWatcher() {
	cc.saveLock.Lock()
	defer cc.saveLock.Unlock()

	if time.Now().Before(cc.ignoreFileEventsUntil) {
		return
	}

	cc.logger.Debug("Config file modified, attempting reload")

	if err := cc.Load(); err != nil {
		cc.logger.Warnw("Failed to reload config file", "error", err)
		return
	}

	cc.logger.Info("Reloaded config successfully")
	cc.notifier.Notify("Configuration reloaded!", "Your changes have been applied.")

	cc.onConfigReloaded()
}

// apply makes the given (normalized) config the live one
func (cc *CanonicalConfig) apply(file *ConfigFile) {
	sliderMapping := sliderMapFromConfigs(file.activeSliderMapping(), readInternalSliderMapping())

	cc.lock.Lock()
	defer cc.lock.Unlock()

	cc.file = file
	cc.version++

	cc.SliderMapping = sliderMapping
	cc.ConnectionInfo.COMPort = file.COMPort
	cc.ConnectionInfo.BaudRate = file.BaudRate
	cc.NoiseReductionLevel = file.NoiseReduction
}

func (cc *CanonicalConfig) writeFile(file *ConfigFile) error {
	body, err := yaml.Marshal(file)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	// write next to the real file and swap it in, so a crash mid-write can't eat the config
	tempPath := userConfigFilepath + ".tmp"
	if err := ioutil.WriteFile(tempPath, append([]byte(configFileHeader), body...), 0644); err != nil {
		return fmt.Errorf("write temp config: %w", err)
	}

	cc.ignoreFileEventsUntil = time.Now().Add(time.Second)

	if err := os.Rename(tempPath, userConfigFilepath); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}

	return nil
}

func (cc *CanonicalConfig) onConfigReloaded() {
	cc.logger.Debug("Notifying consumers about configuration reload")

	for _, consumer := range cc.reloadConsumers {
		consumer <- true
	}
}

func readConfigFile(filepath string) (*ConfigFile, error) {
	raw, err := ioutil.ReadFile(filepath)
	if err != nil {
		return nil, err
	}

	// defaults for anything missing from the file
	file := &ConfigFile{
		WebUI: WebUIConfig{Enabled: true, Port: defaultWebPort},
	}

	if err := yaml.Unmarshal(raw, file); err != nil {
		return nil, err
	}

	return file, nil
}

// the internal config (logs/preferences.yaml) can add slider targets on top of the user's. it doesn't have to exist
func readInternalSliderMapping() map[string][]string {
	result := map[string][]string{}

	raw, err := ioutil.ReadFile(internalConfigPath)
	if err != nil {
		return result
	}

	internal := struct {
		SliderMapping map[string]stringList `yaml:"slider_mapping"`
	}{}

	if err := yaml.Unmarshal(raw, &internal); err != nil {
		return result
	}

	for key, targets := range internal.SliderMapping {
		result[key] = targets
	}

	return result
}

func defaultConfigFile() *ConfigFile {
	file := &ConfigFile{
		WebUI:    WebUIConfig{Enabled: true, Port: defaultWebPort},
		Profiles: map[string]*Profile{defaultProfileName: newDefaultProfile()},
	}

	file.normalize()

	return file
}

func (f *ConfigFile) normalize() {
	f.COMPort = strings.TrimSpace(f.COMPort)
	if f.COMPort == "" || strings.EqualFold(f.COMPort, autoCOMPort) {
		f.COMPort = autoCOMPort
	}

	if f.BaudRate <= 0 {
		f.BaudRate = defaultBaudRate
	}

	f.NoiseReduction = strings.ToLower(strings.TrimSpace(f.NoiseReduction))
	if f.NoiseReduction != noiseReductionLow && f.NoiseReduction != noiseReductionHigh {
		f.NoiseReduction = noiseReductionDefault
	}

	if f.WebUI.Port <= 0 || f.WebUI.Port > 65535 {
		f.WebUI.Port = defaultWebPort
	}

	// migrate a vanilla deej config into a default profile
	if len(f.Profiles) == 0 {
		f.Profiles = map[string]*Profile{}

		if len(f.SliderMapping) > 0 {
			profile := &Profile{Pots: map[int]*PotConfig{}}

			for idx, targets := range f.SliderMapping {
				profile.Pots[idx] = &PotConfig{Targets: targets, Invert: f.InvertSliders}
			}

			f.Profiles[defaultProfileName] = profile
		} else {
			f.Profiles[defaultProfileName] = newDefaultProfile()
		}
	}

	f.SliderMapping = nil
	f.InvertSliders = false

	for name, profile := range f.Profiles {
		if profile == nil {
			profile = &Profile{}
			f.Profiles[name] = profile
		}

		profile.normalize()
	}

	if _, ok := f.Profiles[f.ActiveProfile]; !ok {
		f.ActiveProfile = sortedProfileNames(f.Profiles)[0]
	}
}

// activeSliderMapping flattens the active profile's pots into vanilla deej's slider -> targets shape
func (f *ConfigFile) activeSliderMapping() map[string][]string {
	result := map[string][]string{}

	for idx, pot := range f.Profiles[f.ActiveProfile].Pots {
		result[strconv.Itoa(idx)] = pot.Targets
	}

	return result
}
