**Guard the cutoff against a zero fired time.**

A round that never fired has no cutoff; comparing against the zero time makes every comment look in-window and can converge a round on stale evidence. Return early when FiredAt is nil.

Also applies to: lines 84-91
