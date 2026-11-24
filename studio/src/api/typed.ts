// Typed API client — a thin, fully type-inferred layer over `request()` in
// client.ts. Path, path-params, request body and response type are all derived
// from the generated OpenAPI types in schema.ts (regenerated from the Go route
// table; see `task openapi:gen`). It deliberately wraps the EXISTING request()
// so the battle-tested behaviour — silent 401 refresh, ApiError, 204 handling —
// is preserved; this is not a parallel fetch client.
//
// Usage:
//   const { orgs } = await apiGet("/api/admin/orgs");
//   const org = await apiGet("/api/admin/orgs/{id}", { params: { id } });
//   const r = await apiGet("/api/teams/{id}/connections", {
//     params: { id },            // fills the path's {name} templates, nothing else
//     query: { connector },      // the operation's declared query parameters
//   });
//   await apiPost("/api/admin/orgs", { body: { name, slug } });
//
// `params` is typed from the operation's `parameters.path` and `query` from its
// `parameters.query`, so a query key in `params` (or a path key in `query`) is
// a compile error. Spreads and casts escape that check, so the client also
// refuses at run time a `params` key that names no {name} template of the
// path, and a template no `params` value fills.
//
// Existing hand-written api/*.ts modules keep working; migrate to these helpers
// incrementally where end-to-end type safety against the spec is wanted.
import { apiRequest } from "./client";
import type { paths } from "./schema";

type Method = "get" | "post" | "put" | "patch" | "delete";

// Paths that declare a given method.
type PathsFor<M extends Method> = {
  [P in keyof paths]: M extends keyof paths[P] ? P : never;
}[keyof paths];

type Op<P extends keyof paths, M extends Method> = M extends keyof paths[P] ? paths[P][M] : never;

type JsonOf<T> = T extends { content: { "application/json": infer B } } ? B : never;

// 200 (or 201) application/json response body. Routes without a typed schema
// resolve to `unknown`, which still type-checks — they're just not narrowed.
type ResponseOf<P extends keyof paths, M extends Method> =
  Op<P, M> extends { responses: infer R }
    ? R extends { 200: infer Ok }
      ? JsonOf<Ok>
      : R extends { 201: infer Created }
        ? JsonOf<Created>
        : unknown
    : unknown;

// A body the server accepts EMPTY is `requestBody?:` in the spec; the optional
// form still names the shape, so it is inferred through NonNullable. An
// operation without any requestBody infers `unknown` here and stays `never`.
type BodyOf<P extends keyof paths, M extends Method> =
  Op<P, M> extends { requestBody?: infer RB }
    ? NonNullable<RB> extends { content: { "application/json": infer B } }
      ? B
      : never
    : never;

type ParamsOf<P extends keyof paths, M extends Method> =
  Op<P, M> extends { parameters: { path: infer Pa } }
    ? Pa extends Record<string, unknown>
      ? Pa
      : Record<never, never>
    : Record<never, never>;

// The operation's declared query parameters. An operation that declares none
// (`query?: never`) resolves to `never`, so its `query` option takes nothing.
type QueryOf<P extends keyof paths, M extends Method> =
  Op<P, M> extends { parameters: { query?: infer Q } }
    ? NonNullable<Q> extends Record<string, unknown>
      ? NonNullable<Q>
      : never
    : never;

const TEMPLATE = /\{([^}]+)\}/g;

// fill substitutes the path's {name} templates from params (URL-encoded). A
// params key that names no template, or a template with no value, throws:
// the typed client never drops a value nor sends a literal "{id}"/"undefined".
function fill(path: string, params?: Record<string, string | number>): string {
  const templates = new Set(Array.from(path.matchAll(TEMPLATE), (m) => m[1]));
  for (const key of Object.keys(params ?? {})) {
    if (!templates.has(key)) {
      throw new Error(
        `typed client: params key "${key}" names no {${key}} template in ${path}; a query parameter goes in the query option`,
      );
    }
  }
  return path.replace(TEMPLATE, (_, key: string) => {
    const value = params?.[key];
    if (value === undefined || value === null) {
      throw new Error(`typed client: path template {${key}} of ${path} has no value in params`);
    }
    return encodeURIComponent(String(value));
  });
}

// search serialises the query option as URL search params: one pair per key,
// an undefined value omitted. The spec declares no array query parameter, so
// a value that is not a scalar throws rather than being stringified.
function search(path: string, query?: Record<string, unknown>): string {
  const qs = new URLSearchParams();
  for (const [key, value] of Object.entries(query ?? {})) {
    if (value === undefined) continue;
    if (typeof value !== "string" && typeof value !== "number" && typeof value !== "boolean") {
      throw new Error(`typed client: query key "${key}" of ${path} is not a string, number or boolean`);
    }
    qs.append(key, String(value));
  }
  const s = qs.toString();
  return s ? `?${s}` : "";
}

type GetOpts<P extends keyof paths, M extends Method> = {
  params?: ParamsOf<P, M>;
  query?: QueryOf<P, M>;
};
type BodyOpts<P extends keyof paths, M extends Method> = GetOpts<P, M> & {
  body?: BodyOf<P, M>;
};

// send is async so a refused params/query surfaces as a rejected promise, the
// way every other failure of the call does.
async function send<T>(
  path: string,
  method: string,
  opts?: { params?: Record<string, string | number>; query?: Record<string, unknown>; body?: unknown },
): Promise<T> {
  const init: RequestInit = { method };
  if (opts?.body !== undefined) init.body = JSON.stringify(opts.body);
  // apiRequest takes a fully-qualified path (no BASE_URL prefix) — our spec
  // paths already include the "/api" root — and carries the shared 401-refresh
  // + ApiError + 204 behaviour.
  return apiRequest<T>(fill(path, opts?.params) + search(path, opts?.query), init);
}

export function apiGet<P extends PathsFor<"get">>(
  path: P,
  opts?: GetOpts<P, "get">,
): Promise<ResponseOf<P, "get">> {
  return send(path as string, "GET", opts as never);
}

export function apiPost<P extends PathsFor<"post">>(
  path: P,
  opts?: BodyOpts<P, "post">,
): Promise<ResponseOf<P, "post">> {
  return send(path as string, "POST", opts as never);
}

export function apiPut<P extends PathsFor<"put">>(
  path: P,
  opts?: BodyOpts<P, "put">,
): Promise<ResponseOf<P, "put">> {
  return send(path as string, "PUT", opts as never);
}

export function apiPatch<P extends PathsFor<"patch">>(
  path: P,
  opts?: BodyOpts<P, "patch">,
): Promise<ResponseOf<P, "patch">> {
  return send(path as string, "PATCH", opts as never);
}

export function apiDelete<P extends PathsFor<"delete">>(
  path: P,
  opts?: GetOpts<P, "delete">,
): Promise<ResponseOf<P, "delete">> {
  return send(path as string, "DELETE", opts as never);
}
