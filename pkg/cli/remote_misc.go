package cli

import (
	"context"
)

// RemoteGetPrint fetches a GET endpoint and prints the JSON response —
// the shared implementation for the long-tail read commands whose
// payloads have no dedicated table rendering.
func RemoteGetPrint(ctx context.Context, c *RemoteClient, p *Printer, path string) error {
	raw, err := c.Call(ctx, "GET", path, nil, nil)
	if err != nil {
		return err
	}
	PrintRemoteJSON(p, raw)
	return nil
}

// RemoteSendPrint sends a request with an optional raw JSON body and
// prints the response — the shared implementation for long-tail
// mutation commands driven by --data.
func RemoteSendPrint(ctx context.Context, c *RemoteClient, p *Printer, method, path string, body []byte) error {
	code, resp, err := c.API(ctx, method, path, body)
	if err != nil {
		return err
	}
	if code/100 != 2 {
		return &APIError{Status: code, Method: method, Path: path, Body: string(resp)}
	}
	if len(resp) > 0 {
		PrintRemoteJSON(p, resp)
	} else {
		p.Line("OK")
	}
	return nil
}
