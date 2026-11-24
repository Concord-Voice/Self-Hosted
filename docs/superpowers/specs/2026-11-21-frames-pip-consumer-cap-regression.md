# Frames PiP consumer-cap regression

## Evidence

`95b1b04a` limits a participant to three duplicate consumers in total. Frames
PiP legitimately creates one duplicate of every active microphone and camera,
so the fourth active producer is refused with `Secondary consumer limit reached
(max 3)`.

## Required behavior

- Permit one Frames PiP duplicate for each microphone and camera.
- Retain an atomic participant-wide duplicate budget and a maximum of three
  consumers for one producer, including in-flight reservations.
- Decrement committed copy accounting on every consumer close path.

## Out of scope

No client protocol, configuration, or transport-count change is required.
