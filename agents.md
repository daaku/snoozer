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
snoozer                # detach the daemon (no args)
snoozer --foreground   # run the daemon in the terminal, for debugging
```

The config directory holds `alarms.json`, `alarms.lock`, `daemon.lock`,
`daemon.pid`, `daemon.log`, and the installed `alarm.ogg`.

## Layout

- `main.go`: dispatch and the CLI. The `//go:embed alarm.ogg` lives here.
- `alarm.go`: `Alarm`/`State`, defaults, `--at`/`--repeat` parsing, and the
  locked atomic store.
- `daemon.go`: detach, singleton lock, the next-wake loop, mpv and zenity.
- `alarm_test.go`: parsing, scheduling, and store tests.

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
- Defaults apply to empty optional fields: label `Alarm`, snooze `9m`,
  timeout `3m`, sound `alarm.ogg`. `timeout: "0"` disables the zenity
  timeout. `repeat` is stored canonical mon..sun, and `all` expands to seven.
- mpv, zenity, missing sound files and missing audio devices are all non-fatal:
  log and keep the dialog going.

## Gotchas

- SIGUSR1 is the only reschedule signal; `notifyDaemon` finds the pid in
  `daemon.pid`. The daemon re-reads state and rebuilds its timer on receipt.
- On startup the daemon rings any alarm whose `At` is already in the past
  (catch-up), then advances or drops it.
- The detached child is marked with `SNOOZER_DAEMON=1` so it does not
  re-detach.
