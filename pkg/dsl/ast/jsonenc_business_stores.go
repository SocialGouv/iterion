package ast

import "fmt"

type jsonBusinessStoreDecl struct {
	ID        string `json:"id"`
	Kind      string `json:"kind,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Required  *bool  `json:"required,omitempty"`
}

func businessStoresToJSON(declarations []*BusinessStoreDecl) *[]*jsonBusinessStoreDecl {
	if declarations == nil {
		return nil
	}
	out := make([]*jsonBusinessStoreDecl, 0, len(declarations))
	for _, declaration := range declarations {
		if declaration == nil {
			out = append(out, nil)
			continue
		}
		out = append(out, &jsonBusinessStoreDecl{
			ID: declaration.ID, Kind: declaration.Kind, Namespace: declaration.Namespace, Required: declaration.Required,
		})
	}
	return &out
}

func businessStoresFromJSON(declarations *[]*jsonBusinessStoreDecl) ([]*BusinessStoreDecl, error) {
	if declarations == nil {
		return nil, nil
	}
	out := make([]*BusinessStoreDecl, 0, len(*declarations))
	for _, declaration := range *declarations {
		if declaration == nil {
			return nil, fmt.Errorf("astjson: a business store declaration cannot be null")
		}
		out = append(out, &BusinessStoreDecl{
			ID: declaration.ID, Kind: declaration.Kind, Namespace: declaration.Namespace, Required: declaration.Required,
		})
	}
	return out, nil
}
