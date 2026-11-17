# Run alert origin — registered alerts

A run whose observation was captured delivers its alerts under that registration
rather than under whatever run holds the ID at alert time.

The observation is captured once, where the run starts being observed: the
ID-keyed tailer's start, a detached launch, resume or reattach at their own
seams, and the tail's rotation — a replacement under the same ID resets the
offset and re-registers before the new file's first line. It is released with
the episode: the producer's termination, the last subscriber's release, or the
service's shutdown. Each holder of the episode takes its own bail, and the
registration ends with the last one.

A registered alert's route is frozen at birth. Its `run_health` twin appends
under that registration, which a successor's file refuses. Its frame joins a
dedicated alert registry — keyed by registration, one epoch per adoption, closed
at the release — and the run ID's legacy broker subscription, which unbound and
wrapper consumers keep; a bound subscription holding a registry lane dedups the
legacy carrier, and without a lane it admits a stamped frame only when its origin
matches its binding. The alert manager's own lookups — a run's name, its
human-wait proof — resolve under the registration the episode holds when it
resolves them; the name is cached, so a run first observed before its
registration keeps that first resolution until a replacement. A store without the observed-producer capability keeps
today's by-ID twin.

What this does not establish: detection state stays keyed by run ID, so between a
replacement and the rotation-driven re-registration a successor's events can feed
the predecessor's episode; cloud keeps the persisted `run_health` twin only.
