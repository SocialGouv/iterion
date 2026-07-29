/**
 * A minimal `text/event-stream` reader.
 *
 * Shared by both HTTP transports: streamable HTTP may answer a POST with an
 * SSE stream, and the legacy transport is nothing but one. Node has no
 * server-side EventSource, and pulling one in would break the single-bundle
 * rule for ~40 lines of parsing.
 *
 * Only what MCP needs is implemented: `event`, `data` (multi-line, joined with
 * newlines per the spec) and `id`. Retry hints and comments are skipped.
 */

export interface SseFrame {
	/** The `event:` field, or "message" when the server omitted it. */
	event: string;
	data: string;
	id?: string;
}

/**
 * Yields frames from a fetch response body until the stream ends.
 *
 * A frame is terminated by a blank line, so a partial frame at end-of-stream
 * is dropped: emitting it would hand the caller a truncated JSON payload.
 */
export async function* readSseFrames(body: ReadableStream<Uint8Array>): AsyncGenerator<SseFrame> {
	const reader = body.getReader();
	const decoder = new TextDecoder();
	let buffer = "";

	try {
		for (;;) {
			const { done, value } = await reader.read();
			if (done) break;
			// `stream: true` keeps a multi-byte character split across two
			// chunks from decoding to a replacement character.
			buffer += decoder.decode(value, { stream: true });

			for (;;) {
				const boundary = findFrameBoundary(buffer);
				if (boundary < 0) break;
				const raw = buffer.slice(0, boundary);
				buffer = buffer.slice(boundary + frameSeparatorLength(buffer, boundary));
				const frame = parseFrame(raw);
				if (frame) yield frame;
			}
		}
	} finally {
		// cancel() rather than releaseLock(): the consumer may leave the loop
		// at the frame it cared about, and an uncancelled body would hold the
		// connection open for the rest of the process's life.
		try {
			await reader.cancel();
		} catch {
			/* already closed */
		}
	}
}

/** Index of the blank line ending the first frame, or -1. */
function findFrameBoundary(buffer: string): number {
	const lf = buffer.indexOf("\n\n");
	const crlf = buffer.indexOf("\r\n\r\n");
	if (lf < 0) return crlf;
	if (crlf < 0) return lf;
	return Math.min(lf, crlf);
}

function frameSeparatorLength(buffer: string, at: number): number {
	return buffer.startsWith("\r\n\r\n", at) ? 4 : 2;
}

function parseFrame(raw: string): SseFrame | undefined {
	let event = "";
	let id: string | undefined;
	const data: string[] = [];

	for (const line of raw.split("\n")) {
		const clean = line.endsWith("\r") ? line.slice(0, -1) : line;
		// A line opening with a colon is a comment — servers send these as
		// keep-alives, and treating one as a field would corrupt the frame.
		if (clean === "" || clean.startsWith(":")) continue;
		const colon = clean.indexOf(":");
		const field = colon < 0 ? clean : clean.slice(0, colon);
		let value = colon < 0 ? "" : clean.slice(colon + 1);
		if (value.startsWith(" ")) value = value.slice(1);

		if (field === "event") event = value;
		else if (field === "data") data.push(value);
		else if (field === "id") id = value;
	}

	if (data.length === 0) return undefined;
	return { event: event || "message", data: data.join("\n"), id };
}
