# snoozer

An alarm clock for Linux. One Go binary is both the CLI that schedules alarms
and the background daemon that rings them. The dialog is zenity; the sound is
mpv. State is a JSON file under `$XDG_CONFIG_HOME/snoozer`.

## Commands

```sh
go test ./...          # unit tests, no zenity or mpv needed
go build
snoozer --in=5m --label='go for a walk'
snoozer --at=3:30pm --repeat=tue --label='School Pick Up'
snoozer --at=2pm --repeat=all --label='Vitamins'
snoozer                # run the daemon in the foreground (no args)
```

The config directory holds `alarms.json`, `alarms.lock`, `daemon.lock`,
`daemon.pid`, and the installed `alarm.ogg`.

## Layout

- `main.go`: dispatch and the CLI. The `//go:embed alarm.ogg` lives here.
- `alarm.go`: `Alarm`/`State`, defaults, `--at`/`--repeat` parsing, and the
  locked atomic store.
- `daemon.go`: the singleton lock, the next-wake loop, mpv and zenity.
- `alarm_test.go`: parsing, scheduling, and store tests.
- `snoozer.service`, `PKGBUILD`, `license`: the systemd user unit and the
  Arch package that installs it.

## State

`alarms.json` is `{"alarms": [...]}`; each alarm is:

```json
{
  "at": "2026-09-29T15:30:00+04:00",
  "label": "School Pick Up",
  "repeat": ["mon", "tue"],
  "silent": true,
  "snooze": "5m",
  "timeout": "5m",
  "sound": "custom.ogg"
}
```

Only `at` (RFC 3339) is required. The rest fall back to their defaults when
absent.

## Zenity

The GUI is one zenity question; the exit code decides the action: `0` is
Snooze, anything else is Dismiss. The alarm's timeout is passed as
`--timeout`, or omitted when the timeout is `0`.

```sh
zenity --question --modal --ok-label=Snooze --cancel-label=Dismiss \
  --title=Alarm --icon=alarm --text='Time for a walk'
```

## systemd

`snoozer.service` is the user unit; the PKGBUILD installs it to
`/usr/lib/systemd/user/`. It runs `/usr/bin/snoozer` in the foreground, waits
for `graphical-session.target` so zenity has a display, and restarts on
failure. Install it by hand with:

```sh
install -Dm644 snoozer.service ~/.config/systemd/user/snoozer.service
systemctl --user daemon-reload
systemctl --user enable --now snoozer
```

`systemctl --user status snoozer` and `journalctl --user -u snoozer -f` are
the places to look when the daemon misbehaves.

## Package

`PKGBUILD` builds with `go build -trimpath` and installs the binary to
`/usr/bin`, the unit to `/usr/lib/systemd/user`, and the MIT license. Like
whispy's, it assumes makepkg runs in the repo root: `build()` and `package()`
`cd ..` because makepkg's working directory is the `src/` subdirectory, and
`/pkg` and `/src` are gitignored build artifacts.

```sh
makepkg -si
```

## Invariants

- Every state change goes through `store.update`, which takes the `flock` so
  the CLI and daemon cannot clobber each other. Never write `alarms.json`
  directly. `save` writes a temp file and renames it, so readers never see a
  partial file.
- `Alarm.At` is always the next absolute ring time, including for repeating
  alarms. `takeDue` advances it after a ring; there is no separate first-ring
  field.
- `takeDue` claims a ring before the dialog opens: one-offs are removed and
  repeats are advanced. A crash mid-dialog therefore loses that ring.
- Snoozing appends a one-off copy of the alarm (same label, silent, snooze,
  timeout and sound, no repeat) at now plus the snooze duration.
- The daemon sleeps only until the next ring; it never polls.
- Defaults apply to empty optional fields: label `Alarm`, snooze `9m`,
  timeout `3m`, sound `alarm.ogg`. `timeout: "0"` disables the zenity
  timeout. `repeat` is stored canonical mon..sun, and `all` expands to seven.
- While the dialog is open mpv loops the sound (`--loop=inf`) unless the
  alarm is silent. mpv, zenity, missing sound files and missing audio devices
  are all non-fatal: log and keep the dialog going.
- Errors are wrapped with `github.com/daaku/serr`, the module's only
  dependency.

## Gotchas

- The CLI writes the state file itself and only pokes the daemon with
  SIGUSR1; `notifyDaemon` finds the pid in `daemon.pid`. On receipt the
  daemon re-reads state and rebuilds its timer.
- On startup the daemon rings any alarm whose `At` is already in the past
  (catch-up), then advances or drops it.
- The daemon does not fork. It runs in the foreground and logs to stderr with
  no timestamp of its own, so a supervisor such as systemd owns backgrounding,
  restarts and the journal, and journald's stamp is the only one.
