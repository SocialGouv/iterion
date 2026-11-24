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
wrapper consumers keep. While it holds a registry lane, a bound subscription
drops every stamped legacy frame (the lane delivers its own); as the lane
closes, only frames stamped with the lane's own observation. Any other stamped
frame — in the live loop, at a lane's close and in the end flush alike — ships
only when its origin's header matches the binding. The alert manager's own
lookups — a run's name, its human-wait proof — resolve under the registration
the episode holds when it resolves them; the name is cached, so a run first observed before its
registration keeps that first resolution until a replacement. A store without the observed-producer capability keeps
today's by-ID twin.

What this does not establish:

- Detection state stays keyed by run ID: between a replacement and the
  rotation-driven re-registration a successor's events can feed the
  predecessor's episode.
- An unregistered run's alerts are unstamped and reach any subscriber of the
  ID; a user sink (a webhook) receives the run ID and a link by ID, no birth.
- An open stream's alerts compare the header only, not the recorded tenant.
- A live alert frame is best effort: a terminal alert a detached sink publishes
  may miss a live stream that ends first, and an alert of a re-adoption of the
  same observation published while the lane closes reaches the stream only as
  its twin (`TestLiveEventsReadoptedAlertQueuedAtTheLaneCloseIsDropped`). The
  persisted `run_health` twin is the durable carrier; cloud has only the twin.
