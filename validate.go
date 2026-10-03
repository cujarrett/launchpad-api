package main

import (
	"fmt"
)

// writeRequest is the body POSTed by the Angular client.
type writeRequest struct {
	Workspace string         `json:"workspace"`
	Kind      string         `json:"kind"`
	Name      string         `json:"name"`
	Params    map[string]any `json:"params"`
}

// validate checks required fields per resource kind.
func validate(req writeRequest) error {
	if req.Kind == "" {
		return fmt.Errorf("kind is required")
	}
	if req.Name == "" {
		return fmt.Errorf("name is required")
	}
	if !validWorkspaceName.MatchString(req.Name) {
		return fmt.Errorf("invalid resource name: must be lowercase alphanumeric with hyphens, max 63 chars")
	}

	p := req.Params
	if p == nil {
		p = map[string]any{}
	}

	switch req.Kind {
	case "Spa":
		if err := requireFields(p, "image", "host"); err != nil {
			return err
		}
	case "Api":
		if err := requireFields(p, "image"); err != nil {
			return err
		}
	case "Sql", "NoSql", "ObjectStorage":
		// no required fields beyond name
	case "Topic":
		if err := requireFields(p, "streamName", "subjects"); err != nil {
			return err
		}
	case "Subscription":
		if err := requireFields(p, "topicRef"); err != nil {
			return err
		}
	case "Wordpress":
		if err := requireFields(p, "host"); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown kind: %q", req.Kind)
	}

	return nil
}

func requireFields(p map[string]any, keys ...string) error {
	for _, k := range keys {
		v, ok := p[k]
		if !ok || v == nil || v == "" {
			return fmt.Errorf("params.%s is required for this kind", k)
		}
	}
	return nil
}
