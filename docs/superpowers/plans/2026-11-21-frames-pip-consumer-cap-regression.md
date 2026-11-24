# Frames PiP consumer-cap regression plan

1. Replace the fixed three-duplicate reservation with a topology-aware,
   participant-wide budget plus a per-producer copy ceiling.
2. Keep reservation and close accounting synchronous around the existing native
   `consume()` await.
3. Add the four-producer Frames PiP regression and quota/close-path coverage,
   then run the focused media-plane suite and static checks.
