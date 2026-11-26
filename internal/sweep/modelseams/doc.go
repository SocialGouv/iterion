// Package modelseams holds the source sweeps that pin pkg/backend/model's
// seams across the whole module: no test discards the restore a global
// lookup install returns (SetCredentialsLookup, SetOAuthDirLookup), launch
// text reaches an executor only through SetLaunchVars, and the backends whose
// registry cell says codemode is shipped are exactly the generation loops
// that call installCodemode with the task's own fields.
//
// Each guard reads every Go file of the module rather than anything
// pkg/backend/model exports, so it lives outside that package: a test
// selector then selects pkg/backend/model's behaviour tests only on what
// they import, and selects these sweeps on any Go change. The package
// imports only internal/sweepwalk, pkg/dsl/ir and pkg/backend/backendcaps;
// the setters and seams are matched by name in the parsed source.
package modelseams
