package cli

import (
	"context"
	"fmt"
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

// RemoteSendData is RemoteSendPrint fed by a --data argument (literal
// JSON, @file, or @- for stdin), which it requires to be non-empty.
// `what` names the expected payload in the error message.
func RemoteSendData(ctx context.Context, c *RemoteClient, p *Printer, method, path, dataArg, what string) error {
	body, err := ReadDataArg(dataArg)
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return fmt.Errorf("--data is required (%s; literal JSON, @file, or @- for stdin)", what)
	}
	return RemoteSendPrint(ctx, c, p, method, path, body)
}

// RemoteAPIPrint is the `remote api` escape hatch: it prints the
// response body regardless of status (an error response is exactly
// what the caller asked to see) and maps non-2xx to an *APIError.
func RemoteAPIPrint(ctx context.Context, c *RemoteClient, p *Printer, method, path string, body []byte) error {
	code, resp, err := c.API(ctx, method, path, body)
	if err != nil {
		return err
	}
	if len(resp) > 0 {
		PrintRemoteJSON(p, resp)
	}
	if code/100 != 2 {
		return &APIError{Status: code, Method: method, Path: path, Body: string(resp)}
	}
	return nil
}

// RemoteRoutesList renders the instance's live route table.
func RemoteRoutesList(ctx context.Context, c *RemoteClient, p *Printer) error {
	var rr struct {
		Routes []struct {
			Method  string `json:"method"`
			Pattern string `json:"pattern"`
		} `json:"routes"`
	}
	raw, err := c.Call(ctx, "GET", "/api/routes", nil, &rr)
	if err != nil {
		return err
	}
	if p.Format == OutputJSON {
		PrintRemoteJSON(p, raw)
		return nil
	}
	for _, r := range rr.Routes {
		m := r.Method
		if m == "" {
			m = "ANY"
		}
		p.Line("%-6s %s", m, r.Pattern)
	}
	return nil
}
