# snoozer

Snoozer is a Alarm application for Linux.

## Interaction

snoozer --in=5m
snoozer --in=15m --label='go for a walk'
snoozer --at=11am --label='go for a walk'
snoozer --at=3:30pm --repeat=tue --label='School Pick Up'
snoozer --at=2pm --repeat=all --label='Vitamins'
snoozer --at=9:40pm --repeat=all --silent --label='Steps'

## Implementation

- Written in Go
- Using Zenity dialogs for alarms and interactions
- A daemon runs in the background
- Daemon and CLI are the same binary, no args starts daemon mode
- Daemon is efficient and wakes up only for the next alarm
- State is stored in a JSON file in XDG_CONFIG .snoozer/alarms.json
- State is atomically replaced with a new file as it changes, instead of overwritten in place
- Guard against concurrent State file modifications
- CLI interacts with JSON file directly, and notifies the daemon by sending it a SIGUSR1
- On SIGUSR1 the daemon will re-read state, and sleep according to the updated state
- Each alarm is stored as:
  ```
  {
    "at": time.Time,
    "label": "School Pick Up",
    "repeat": ["mon", "tue", ...]
    "silent": true,
    "snooze": "5m",
    "timeout": "5m",
    "sound": "custom.ogg"
  }
  ```
- Everything except "at" is optional
- Alarm label defaults to just the word "Alarm"
- Snooze defaults to 9m
- Timeout defaults to 3m
- Sound defaults to alarm.ogg
- In CLI repeat=all should translate to every day listed in the repeat array
- For repeating alarms, once the alarm rings, the "at" value is updated to reflect the next time the alarm should ring.
- For one off alarms, the alarm is deleted
- Snoozing an alarm adds a one-off alarm with the same details to go off after snooze duration.
- While the alarm dialog is open, a repeating alarm sound should be playing unless silent=true
- Audio file should be played in loop mode using mpv until alarm is snoozed or dismissed
- Missing audio device should not be considered an error
- Use github.com/daaku/serr to wrap errors
- Initialize the go module as github.com/daaku/snoozer

# Zenity Use

The GUI is just zenity dialogs. Use exit codes to determine snooze or dismiss.

```
zenity --question --modal --ok-label=Snooze --cancel-label=Dismiss --title=Alarm --icon=alarm --text='Time for a walk'
```
