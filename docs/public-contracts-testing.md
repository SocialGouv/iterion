# Public contract compatibility tests

The store compatibility suite executes both current stores and binaries built
from main commit `3872f9dd1d1cbf9c18ed338c66c9f55aaaea3fcc`. The old probe
is compiled inside that source tree and calls its public APIs; it is not a
mock of the old implementation. S3 calls use the real clients from both builds
against a disposable HTTP object fixture. Mongo uses a real Mongo 8 replica
set; follow [the development recipe](development.md#running-the-mongo-conformance-harness-locally).

From the task checkout, with Go 1.26 and Node 24 provided by devbox:

```bash
devbox run -- bash scripts/build-port-legacy-fixture.sh /tmp/iterion-port-legacy
ITERION_TEST_REQUIRED=1 \
ITERION_TEST_MONGO_URI='mongodb://localhost:27018/?replicaSet=rs0' \
ITERION_TEST_LEGACY_BINARY=/tmp/iterion-port-legacy/iterion-legacy \
ITERION_TEST_LEGACY_PROBE=/tmp/iterion-port-legacy/probe-legacy \
devbox run -- go test -race -json -count=1 ./pkg/store ./pkg/store/blob ./pkg/store/mongo -run TestNative > /tmp/iterion-port-storage.jsonl
devbox run -- node scripts/verify-port-tests.mjs pkg/store/storetest/native_namespaces.json /tmp/iterion-port-storage.jsonl
```

For a devbox container that mounts the checkout without its parent `.git`,
create an archive with `git archive` of the pinned commit on the host and pass
its container path as `PORTS_LEGACY_SOURCE_ARCHIVE` to the builder. The builder
checks the archive's commit identity and records executable SHA-256 sums.
It removes only its own temporary source directory. Tests use isolated stores
and configuration directories and require no provider credentials.

Required mode fails when a binary or integration prerequisite is missing.
The manifest verifier also fails if any expected case is absent or skipped.
These checks establish store namespace isolation, not scheduler correctness,
queue compatibility, fleet activation or safe workspace maintenance. Those
remain separate entries in the [acceptance matrix](public-contracts-acceptance.md).
