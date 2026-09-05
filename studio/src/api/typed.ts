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
//   await apiPost("/api/admin/orgs", { body: { name, slug } });
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

// fill substitutes {name} path templates from params (URL-encoded).
function fill(path: string, params?: Record<string, string | number>): string {
  if (!params) return path;
  return path.replace(/\{([^}]+)\}/g, (_, k: string) => encodeURIComponent(String(params[k])));
}

type GetOpts<P extends keyof paths, M extends Method> = {
  params?: ParamsOf<P, M>;
};
type BodyOpts<P extends keyof paths, M extends Method> = GetOpts<P, M> & {
  body?: BodyOf<P, M>;
};

function send<T>(path: string, method: string, opts?: { params?: Record<string, string | number>; body?: unknown }): Promise<T> {
  const init: RequestInit = { method };
  if (opts?.body !== undefined) init.body = JSON.stringify(opts.body);
  // apiRequest takes a fully-qualified path (no BASE_URL prefix) — our spec
  // paths already include the "/api" root — and carries the shared 401-refresh
  // + ApiError + 204 behaviour.
  return apiRequest<T>(fill(path, opts?.params), init);
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
