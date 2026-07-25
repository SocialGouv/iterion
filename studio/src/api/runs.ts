// Barrel kept so the ~130 existing `from "@/api/runs"` import sites
// keep working unchanged; the actual code lives in ./runs/*.

export * from "./runs/client";
export * from "./runs/types";
export * from "./runs/listing";
export * from "./runs/snapshot";
export * from "./runs/artifacts";
export * from "./runs/notes";
export * from "./runs/lifecycle";
export * from "./runs/files";
export * from "./runs/commits";
export * from "./runs/conflicts";
export * from "./runs/uploads";
export * from "./runs/children";

// Repo-target launch fields (Launch form's "Target repository" section):
// aim the run at a git repository so the cloud runner clones it before
// sandboxing. Cloud-only server-side — local mode 400s. Declared here as a
// module augmentation so the createRun body type carries them without
// touching the types module (unrelated fields moving concurrently).
declare module "./runs/types" {
  interface CreateRunRequest {
    /** HTTPS clone URL of the target repo. Must live on the connection's
     *  forge host (server-checked); the server pins the workflow's
     *  forge_token secret to the connection's managed token so the clone
     *  is authenticated. Pair with connection_id. */
    repo_url?: string;
    /** Optional git ref (branch / tag / sha) to clone; empty = the forge's
     *  default branch. */
    repo_ref?: string;
    /** Team forge connection whose managed token authenticates the clone
     *  (and any push-back). Required when repo_url is set. */
    connection_id?: string;
  }
}
