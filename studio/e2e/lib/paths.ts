import { STUDIO_BASE } from "../../src/lib/scope";

/**
 * studio turns a studio route into the address it actually answers on.
 *
 * The specs navigate with absolute paths, which Playwright resolves against
 * the origin and NOT against a baseURL's path — so the prefix has to be
 * written out. It is written out here, once: thirty literals spread over a
 * dozen spec files is thirty places for the next prefix change to land in
 * twenty-nine.
 *
 * Imported from the app's own source rather than re-declared, so a spec can
 * never test an address the app does not serve.
 */
export function studio(route = ""): string {
  return STUDIO_BASE + route;
}
