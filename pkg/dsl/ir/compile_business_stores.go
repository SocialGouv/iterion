package ir

import "github.com/SocialGouv/iterion/pkg/dsl/ast"

func compileBusinessStores(declarations []*ast.BusinessStoreDecl) []BusinessStore {
	if declarations == nil {
		return nil
	}
	out := make([]BusinessStore, 0, len(declarations))
	for _, declaration := range declarations {
		out = append(out, BusinessStore{
			ID: declaration.ID, Kind: declaration.Kind, Namespace: declaration.Namespace,
			Required: declaration.Required != nil && *declaration.Required,
		})
	}
	return out
}
