# deej remix

A remix of [deej](https://github.com/omriharel/deej) with a live control panel, profiles and zero-config connecting.
Same hardware, same serial protocol: any vanilla deej board works as-is.

## What's new

- **Control panel**: tray icon → *Open control panel* (or http://localhost:7979). A live mixer view of your pots: they
  move on screen as you move them, and the strip glows so you can tell which physical pot is which.
- **Sliders or knobs**: pick per pot. Knobs render as a dial, sliders as a fader.
- **Tune per pot**: invert, *audio* curve (finer control at low volumes), and **calibration** for pots that never quite
  reach 0 or 100% (click *Calibrate*, sweep the pot end to end, then save).
- **Auto-detect COM port**: `com_port: auto` listens on every serial port and picks the one speaking deej. It also
  **reconnects by itself** when you unplug/replug the board, instead of needing a restart.
- **Audio session discovery**: every app currently playing audio shows up live. Drag it onto a pot (or click it, then
  click a pot). Drag targets between pots to move them.
- **Profiles**: switch from the panel, the tray menu, or keys `1`–`9`. **Auto-activate** a profile while a given app is
  running (e.g. a game), and drop back to the previous one when it exits.
- **Instant settings**: everything saves to `config.yaml` as you go. Hand edits to the file show up in the panel live.
- **Firmware**: [`arduino/deej-remix`](arduino/deej-remix/deej-remix.ino) adds oversampling and smoothing for steadier
  readings on cheap pots. Optional, since the vanilla sketch still works.

Old configs (`slider_mapping`, `invert_sliders`) are migrated into a `default` profile automatically. No config at all?
One is created and the panel opens on first launch.

## Building

```sh
# linux (needs gtk3 + ayatana-appindicator3 dev packages for the tray)
go build -o deej ./pkg/deej/cmd

# windows (from any OS)
GOOS=windows go build -ldflags "-H=windowsgui" -o deej.exe ./pkg/deej/cmd
```

Set `DEEJ_NO_TRAY_ICON=1` to run without a tray icon.

The control panel only listens on `127.0.0.1` and rejects requests from other sites and hosts.
