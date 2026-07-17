`@kristofferR`, I rechecked the apply and deadline paths, and the bounded loop does not cover this branch as implemented:

- `applyFire` handles `FireNo` without mutating the round.
- `ensureWaitDeadline` only writes a deadline for `PhaseFired` or `PhaseReviewing`.
- This branch can therefore leave a queued/awaiting-retry round with no `WaitDeadline`; a missing Codex response then remains eligible for repeated `FireNo` decisions.

The "evidence is imminent" condition is a reasonable operational expectation, but it is not a timeout guarantee. I'm retaining the finding: adopt/record the existing Codex command into a timed waiting round, or introduce a persisted wait outcome for this case.
