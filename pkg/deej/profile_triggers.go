package deej

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/mitchellh/go-ps"
	"go.uber.org/zap"
)

// profileTriggers switches to a profile while any of its auto_activate processes are running,
// and back to whatever was active before once they all exit
type profileTriggers struct {
	deej   *Deej
	logger *zap.SugaredLogger

	// the profile we switched to automatically, and the one that was active before it
	autoProfile     string
	previousProfile string

	lastDesired string
}

const (
	profileTriggerInterval = 3 * time.Second
	linuxProcessNameLength = 15
)

func newProfileTriggers(deej *Deej, logger *zap.SugaredLogger) *profileTriggers {
	return &profileTriggers{
		deej:   deej,
		logger: logger.Named("profiles"),
	}
}

func (pt *profileTriggers) watch() {
	ticker := time.NewTicker(profileTriggerInterval)
	defer ticker.Stop()

	for range ticker.C {
		pt.check()
	}
}

func (pt *profileTriggers) check() {
	config := pt.deej.config.Snapshot()

	// skip the process scan entirely when nobody uses triggers
	hasTriggers := false
	for _, profile := range config.Profiles {
		hasTriggers = hasTriggers || len(profile.AutoActivate) > 0
	}

	if !hasTriggers && pt.autoProfile == "" {
		return
	}

	running := pt.runningNames()

	desired := ""
	for _, name := range sortedProfileNames(config.Profiles) {
		for _, trigger := range config.Profiles[name].AutoActivate {
			if pt.matches(running, trigger) {
				desired = name
				break
			}
		}

		if desired != "" {
			break
		}
	}

	// only act on changes, so manually switching away from an auto-activated profile sticks
	if desired == pt.lastDesired {
		return
	}

	pt.lastDesired = desired

	if desired != "" {
		if config.ActiveProfile != desired {
			if pt.autoProfile == "" {
				pt.previousProfile = config.ActiveProfile
			}

			pt.activate(desired, fmt.Sprintf("Switched to \"%s\"", desired))
		}

		pt.autoProfile = desired
		return
	}

	// all triggers gone: go back, unless the user has moved on to something else in the meantime
	if config.ActiveProfile == pt.autoProfile {
		if _, ok := config.Profiles[pt.previousProfile]; ok {
			pt.activate(pt.previousProfile, fmt.Sprintf("Back to \"%s\"", pt.previousProfile))
		}
	}

	pt.autoProfile = ""
	pt.previousProfile = ""
}

func (pt *profileTriggers) activate(name string, message string) {
	pt.logger.Infow("Auto-switching profile", "profile", name)

	if err := pt.deej.config.SetActiveProfile(name); err != nil {
		pt.logger.Warnw("Failed to auto-switch profile", "profile", name, "error", err)
		return
	}

	pt.deej.notifier.Notify("Profile switched", message)
}

func (pt *profileTriggers) matches(running map[string]bool, trigger string) bool {
	trigger = strings.ToLower(trigger)

	// linux reports process names cut down to 15 characters
	if len(trigger) > linuxProcessNameLength && running[trigger[:linuxProcessNameLength]] {
		return true
	}

	return running[trigger]
}

// running process names (lowercase), plus every app currently holding an audio session
func (pt *profileTriggers) runningNames() map[string]bool {
	names := map[string]bool{}

	processes, err := ps.Processes()
	if err != nil {
		pt.logger.Debugw("Failed to list processes", "error", err)
	}

	for _, process := range processes {
		names[strings.ToLower(filepath.Base(process.Executable()))] = true
	}

	for _, session := range pt.deej.sessions.snapshot(false) {
		names[session.Key] = true
	}

	return names
}
