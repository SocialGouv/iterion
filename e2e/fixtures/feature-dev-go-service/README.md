# userservice (iterion fixture)

A tiny HTTP service used as a realistic starting point for
`iterion`'s `feature_dev` live test. Not production code:
the store is in-memory and resets on every restart.

## Layout

- `main.go` — wires the HTTP server, seeds two users, mounts handlers.
- `handlers/` — `GET /users` and `GET /users/{id}`.
- `internal/store/` — in-memory user + post repository (mutex-guarded).
- `middleware/` — `RequestLogger` access log.

## Running

```bash
go run .
# ADDR=:9999 go run .
```

## Tests

```bash
go test ./...
```
