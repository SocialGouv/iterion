package spec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
)

// EvalDelivery evaluates one of a package's delivery expressions (an output,
// a filter, the loop guard, the installation, the handshake) under ctx — the
// evaluator the validation, the preview and the lane share.
func EvalDelivery(src string, ctx *expr.Context) (any, error) { return evalExpr(src, ctx) }

// Locate reads one value of a delivery through one of in's locators — a
// header that carries no credential, or an expression over the delivery's
// context.
func Locate(in *Ingress, l IngressLocator, payload map[string]any, headers http.Header, listed []string) (string, error) {
	return locate(in, l, payload, headers, listed)
}

// IngressDigest is the identity of an ingress contract as an operator
// consents to it when binding: the sha256 of the block's canonical JSON. The
// lane refuses a delivery whose package resolves to another digest.
func IngressDigest(in *Ingress) (string, error) {
	if in == nil {
		return "", fmt.Errorf("spec: no ingress block to digest")
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return "", fmt.Errorf("spec: encode the ingress block: %w", err)
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
