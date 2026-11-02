package deej

import (
	"fmt"
	"time"

	"github.com/getlantern/systray"

	"github.com/omriharel/deej/pkg/deej/icon"
	"github.com/omriharel/deej/pkg/deej/util"
)

// tray menus can't remove items, so profiles get a fixed set of slots that are shown and hidden as needed
const (
	maxTrayProfiles    = 16
	trayUpdateInterval = 500 * time.Millisecond
)

func (d *Deej) initializeTray(onDone func()) {
	logger := d.logger.Named("tray")

	onReady := func() {
		logger.Debug("Tray instance ready")

		systray.SetTemplateIcon(icon.DeejLogo, icon.DeejLogo)
		systray.SetTitle("deej")
		systray.SetTooltip("deej")

		connectionStatus := systray.AddMenuItem("Starting...", "")
		connectionStatus.Disable()

		systray.AddSeparator()

		openPanel := systray.AddMenuItem("Open control panel", "Configure pots, profiles and connection in your browser")

		profilesMenu := systray.AddMenuItem("Profile", "Switch between profiles")
		profileSlots := make([]*systray.MenuItem, maxTrayProfiles)
		profileSlotNames := make([]string, maxTrayProfiles)
		profileClicks := make(chan int)

		for idx := range profileSlots {
			profileSlots[idx] = profilesMenu.AddSubMenuItemCheckbox("", "", false)
			profileSlots[idx].Hide()

			go func(idx int) {
				for range profileSlots[idx].ClickedCh {
					profileClicks <- idx
				}
			}(idx)
		}

		systray.AddSeparator()

		editConfig := systray.AddMenuItem("Edit configuration file", "Open config file with a text editor")
		editConfig.SetIcon(icon.EditConfig)

		refreshSessions := systray.AddMenuItem("Re-scan audio sessions", "Manually refresh audio sessions if something's stuck")
		refreshSessions.SetIcon(icon.RefreshSessions)

		if d.version != "" {
			systray.AddSeparator()
			versionInfo := systray.AddMenuItem(d.version, "")
			versionInfo.Disable()
		}

		systray.AddSeparator()
		quit := systray.AddMenuItem("Quit", "Stop deej and quit")

		var lastConfigVersion uint64
		lastStatusText := ""

		updateMenu := func() {
			if text := trayStatusText(d.serial.Status()); text != lastStatusText {
				lastStatusText = text
				connectionStatus.SetTitle(text)
				systray.SetTooltip("deej - " + text)
			}

			version := d.config.Version()
			if version == lastConfigVersion {
				return
			}

			lastConfigVersion = version
			config := d.config.Snapshot()
			names := sortedProfileNames(config.Profiles)

			profilesMenu.SetTitle(fmt.Sprintf("Profile: %s", config.ActiveProfile))

			for idx, slot := range profileSlots {
				if idx >= len(names) {
					profileSlotNames[idx] = ""
					slot.Hide()
					continue
				}

				profileSlotNames[idx] = names[idx]
				slot.SetTitle(names[idx])
				slot.Show()

				if names[idx] == config.ActiveProfile {
					slot.Check()
				} else {
					slot.Uncheck()
				}
			}
		}

		updateMenu()

		// wait on things to happen
		go func() {
			ticker := time.NewTicker(trayUpdateInterval)
			defer ticker.Stop()

			for {
				select {

				case <-ticker.C:
					updateMenu()

				// quit
				case <-quit.ClickedCh:
					logger.Info("Quit menu item clicked, stopping")

					d.signalStop()

				// open the control panel
				case <-openPanel.ClickedCh:
					logger.Info("Control panel menu item clicked, opening browser")

					d.openControlPanel()

				// switch profile
				case idx := <-profileClicks:
					name := profileSlotNames[idx]
					logger.Infow("Profile menu item clicked, switching", "profile", name)

					if err := d.config.SetActiveProfile(name); err != nil {
						logger.Warnw("Failed to switch profile", "error", err)
					}

					// the checkmark toggles itself on click, so always resync
					lastConfigVersion = 0
					updateMenu()

				// edit config
				case <-editConfig.ClickedCh:
					logger.Info("Edit config menu item clicked, opening config for editing")

					editor := "notepad.exe"
					if util.Linux() {
						editor = "xdg-open"
					}

					if err := util.OpenExternal(logger, editor, userConfigFilepath); err != nil {
						logger.Warnw("Failed to open config file for editing", "error", err)
					}

				// refresh sessions
				case <-refreshSessions.ClickedCh:
					logger.Info("Refresh sessions menu item clicked, triggering session map refresh")

					// performance: the reason that forcing a refresh here is okay is that users can't spam the
					// right-click -> select-this-option sequence at a rate that's meaningful to performance
					d.sessions.refreshSessions(true)
				}
			}
		}()

		// actually start the main runtime
		onDone()
	}

	onExit := func() {
		logger.Debug("Tray exited")
	}

	// start the tray icon
	logger.Debug("Running in tray")
	systray.Run(onReady, onExit)
}

func (d *Deej) stopTray() {
	d.logger.Debug("Quitting tray")
	systray.Quit()
}

func trayStatusText(status SerialStatus) string {
	switch status.State {
	case serialStateConnected:
		return fmt.Sprintf("Connected on %s (%d pots)", status.Port, status.NumSliders)
	case serialStateSearching:
		return "Searching for deej..."
	case serialStateConnecting:
		return fmt.Sprintf("Connecting to %s...", status.Port)
	}

	return "Disconnected - retrying"
}
