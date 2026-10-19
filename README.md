# peats

Control SoundPEATS earbuds from Linux without the PeatsAudio phone app.
Set noise control, toggles, prompt volume, and touch gestures, and read the
battery level of each bud and the case.

Tested with a **SoundPEATS T3 Pro** on firmware `ZKLXT3PRO_20250428_v0.0.31`.
Other SoundPEATS models that the app controls over Bluetooth Classic SPP
probably work the same way.

## Requirements

- Linux with BlueZ and `bluetoothctl`
- Go 1.26 or later, to build
- The buds paired and connected

## Install

```bash
go install github.com/hamidriaz1998/peats@latest
```

The binary goes to `~/go/bin/peats`.

## Usage

```bash
peats status                        # full state as JSON
peats mode off|anc|transparency|next
peats game on|off                   # also: touch-lock, movie, dynamic-eq
peats prompt-volume 50              # 0-100, the buds round down to 6.25% steps
peats keys                          # list gesture -> action
peats key triple-left next          # set one gesture
peats keys reset                    # restore the factory gestures
peats watch                         # hold the channel, print JSON lines
```

Gestures: `tap`, `double`, `triple`, and `hold`, each with `-left` or `-right`.

Actions: `volume-up`, `volume-down`, `play-pause`, `game-mode`, `previous`,
`next`, `anc`, `assistant`.

A change is confirmed by a read-back. If the buds do not show the new value,
`peats` exits with code 1.

### Keybinding example

On Omarchy, add this to `~/.config/hypr/bindings.lua`:

```lua
o.bind("SUPER + CTRL + N", "Cycle earbud noise mode", "peats mode next")
```

## How it works

The buds accept one SPP client at a time. `peats watch` keeps that client
open and listens on the abstract Unix socket `@hamid.soundpeats`. Other
`peats` commands send their request through a running watcher. If no watcher
runs, they connect to the buds directly. The socket accepts only processes of
the same user.

The protocol uses GAIA-style frames on RFCOMM channel 1:

```
request  FF 04 00 <len> 00 0A <cmd:2> <payload>
reply    FF 04 00 <len> 00 0A <cmd|0x8000:2> <payload>
```

| Setting | Get | Set | Values |
|---|---|---|---|
| Noise control | `0310` | `0311` | 0 off, 1 ANC, 2 transparency |
| Game mode | `030E` | `030F` | 0 off, 1 on |
| Disable touch | `0312` | `0313` | 0 off, 1 on |
| Movie mode | `0327` | `0328` | 0 off, 1 on |
| Dynamic EQ | `0331` | `0332` | 0 off, 1 on |
| Prompt volume | `0319` | `0320` | percent |
| Gestures | `03AA` | `03AB` | `[gesture, action]` pairs, reset with `03AC` |
| Battery left / right / case | `0306` / `0307` / `0323` | — | percent |
| Firmware | `0309` | — | ASCII string |

Get replies carry `[status, value]`. The buds do not acknowledge a set, so
`peats` reads the value back after a short delay and resends once if needed.

## Known limits

- While the phone app is connected, the buds do not answer `peats`, and the
  reverse is also true.
- `peats keys reset` restores the factory gestures. These can be different
  from the defaults that the phone app shows.
- Not supported: Find Earbuds, prompt language, the equalizer, and firmware
  upgrade.

## Omarchy

The [hamid.soundpeats](https://github.com/hamidriaz1998/dotfiles/tree/main/omarchy/.config/omarchy/plugins/hamid.soundpeats)
bar plugin runs `peats watch` and adds a panel for all the settings above.

## License

MIT. See `LICENSE`.

Not affiliated with or endorsed by SoundPEATS. The protocol notes come from
interoperability analysis of the PeatsAudio Android app and traffic to a
T3 Pro. This repo includes no app code.
