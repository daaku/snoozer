# snoozer

Snoozer is a Alarm application for Linux.

## Interaction

snoozer --in=5m
snoozer --in=15m --label='go for a walk'
snoozer --at=11am --label='go for a walk'
snoozer --at=3:30pm --repeat=tue --label='School Pick Up'
snoozer --at=2pm --repeat=all --label='Vitamins'
snoozer --at=9:40pm --repeat=all --silent --label='Steps'

Adding an alarm prints one line, for example
`Alarm set for Tue 05:24pm: go for a walk`.
