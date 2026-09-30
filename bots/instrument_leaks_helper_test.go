package bots

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// leaksHelperDriver runs the capture helper the lang-python skill SHIPS —
// exec'd from the skill's own code block, never retyped — over seeded
// captures, and reports the false leaks and the promised detections.
const leaksHelperDriver = `import base64, json, random, sys, uuid

fence = chr(96) * 3
src = open(sys.argv[1], encoding="utf-8").read()
start = src.index(fence + "python\nimport base64, gzip")
block = src[start + len(fence + "python\n"):]
block = block[:block.index("\n" + fence)]
ns = {}
exec(block, ns)
leaks = ns["leaks"]


def u4(rng):
    return str(uuid.UUID(int=rng.getrandbits(128), version=4))


# Captures that carry NO planted value: 40 planted uuid4s against 200
# unrelated ones, seeded so a count is a fact, not a draw.
false_leaks = 0
for seed in range(80):
    rng = random.Random(seed)
    planted = [u4(rng) for _ in range(40)]
    capture = [json.dumps({"request_id": x, "url": "/api/v1/resources/" + x}).encode() for x in (u4(rng) for _ in range(200))]
    false_leaks += bool(leaks(capture, planted))

p = u4(random.Random(999))
tok24 = "a3f9c2e17b4d8e06f1e2d3c4"
name = "Hélèneqzx7"


def one(v, ascii_only=False):
    return [json.dumps({"x": v}, ensure_ascii=ascii_only).encode()]


def b64u(o):
    return base64.urlsafe_b64encode(json.dumps(o).encode()).decode().rstrip("=")


jwt = b64u({"alg": "none"}) + "." + b64u({"sub": p, "iat": 1}) + "." + "c2lnbmF0dXJlLWJ5dGVzLTAwMDAwMDAw"


detected = {
    "whole value": bool(leaks(one(p), [p])),
    "dash-stripped uuid (uuid.hex)": bool(leaks(one(p.replace("-", "")), [p])),
    "truncated sub[:8]": bool(leaks(one(p[:8]), [p])),
    "upper-cased": bool(leaks(one(p.upper()), [p])),
    "JSON unicode escape": bool(leaks(one(name, ascii_only=True), [name])),
    "percent-encoded": bool(leaks(one("a3f9c2e1%37b4d8e06"), ["a3f9c2e17b4d8e06"])),
    "short value, whole": bool(leaks(one("k9Zq2"), ["k9Zq2"])),
    "value without a run, whole": bool(leaks(one("Jean-Pierre"), ["Jean-Pierre"])),
    "the last run of a value (a masked token's display)": bool(leaks(one("tok_" + p[-8:]), [p])),
    "a value percent-encoded character by character": bool(leaks(one("".join("%%%02X" % ord(c) for c in "a3f9c2e17b4d8e06")), ["a3f9c2e17b4d8e06"])),
    "repr() of the UTF-8 bytes (x-escapes)": bool(leaks(one(repr(name.encode())), [name])),
    "a claim inside a JWT payload": bool(leaks(one("Bearer " + jwt), [p])),
    "JSON inside JSON (u-escapes twice)": bool(leaks(one(json.dumps({"name": name})), [name])),
    "a 7-character fragment is no leak": not leaks(one(p[:7]), [p]),
    "an unrelated address on the same domain is no leak": not leaks(one("support@example.test"), ["a3f9c2e1@example.test"]),
    "a truncated copy starting with a dotted capital I": bool(leaks(one(("İ" + "a3f9c2e17b4d8e06")[:8]), ["İ" + "a3f9c2e17b4d8e06"])),
    "a run in the middle, at an odd offset": bool(leaks(one("x" + tok24[9:17] + "y"), [tok24])),
    "a run at offset one": bool(leaks(one(tok24[1:9]), [tok24])),
    "a value percent-encoded twice": bool(leaks(one("".join("%%25%02X" % ord(c) for c in tok24)), [tok24])),
    "a plus for a space in a query string": bool(leaks(one("q=Jean+Pierre"), ["Jean Pierre"])),
    "ascii() of a name (Latin-1 x-escapes)": bool(leaks(one(ascii(name)), [name])),
    "a case fold beyond lower() (sharp s)": bool(leaks(one("WEISSBIER9X"), ["Weißbier9x"])),
    "a plain-text access line (a non-JSON item)": bool(leaks([b'INFO:     127.0.0.1:5000 - "GET /callback?code=' + tok24.encode() + b'&state=x HTTP/1.1" 200 OK'], [tok24])),
    "a numeric id (a number, not a string)": bool(leaks([json.dumps({"extra": {"user_id": 918273645501}}).encode()], ["918273645501"])),
    "an attachment line (not JSON)": bool(leaks([b"session dump: sub=" + tok24.encode()], [tok24])),
}
print(json.dumps({"false_leaks": false_leaks, "detected": detected}))
`

// TestInstrumentLeaksHelperMatchesDistinctiveRunsOnly: the helper is code the
// authoring agent copies into its capture test, so its matching is pinned
// here. A run of a planted value that crosses a separator is too weak to
// count — across a uuid4's fixed -4xxx-8xxx- it carries ~18 bits and matches
// unrelated uuid4s, failing a capture that leaked nothing — while every copy
// the skill promises to see is still seen, and what it must not see is not.
func TestInstrumentLeaksHelperMatchesDistinctiveRunsOnly(t *testing.T) {
	t.Parallel()
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not on PATH")
	}
	driver := filepath.Join(t.TempDir(), "driver.py")
	if err := os.WriteFile(driver, []byte(leaksHelperDriver), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(py, driver, filepath.Join("instrument", "skills", "lang-python.md")).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("driver: %v\n%s", err, ee.Stderr)
		}
		t.Fatalf("driver: %v", err)
	}
	var res struct {
		FalseLeaks int             `json:"false_leaks"`
		Detected   map[string]bool `json:"detected"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("driver output: %v (%s)", err, out)
	}
	if res.FalseLeaks != 0 {
		t.Errorf("%d of 80 captures holding NO planted value were reported leaking: a run across a uuid4's separators matched an unrelated uuid4", res.FalseLeaks)
	}
	if len(res.Detected) != 25 {
		t.Fatalf("want 25 detection cases, got %d: %v", len(res.Detected), res.Detected)
	}
	for name, ok := range res.Detected {
		if !ok {
			t.Errorf("the helper got this case wrong: %s", name)
		}
	}
}
