package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var kindToXRD = map[string]string{
	"XSpa":           "xspas.platform.local.lab",
	"XApi":           "xapis.platform.local.lab",
	"XSql":           "xsqls.platform.local.lab",
	"XNoSql":         "xnosqls.platform.local.lab",
	"XObjectStorage": "xobjectstorages.platform.local.lab",
	"XTopic":         "xtopics.platform.local.lab",
	"XSubscription":  "xsubscriptions.platform.local.lab",
	"XWordpress":     "xwordpresses.platform.local.lab",
}

var xrdGVR = schema.GroupVersionResource{
	Group:    "apiextensions.crossplane.io",
	Version:  "v2",
	Resource: "compositeresourcedefinitions",
}

func (a *app) handleGetSchema(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")

	xrdName, ok := kindToXRD[kind]
	if !ok {
		http.Error(w, fmt.Sprintf("unknown kind: %q", kind), http.StatusBadRequest)
		return
	}

	if a.dynClient == nil {
		http.Error(w, "k8s unavailable", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	xrd, err := a.dynClient.Resource(xrdGVR).Get(ctx, xrdName, metav1.GetOptions{})
	if err != nil {
		http.Error(w, fmt.Sprintf("XRD not found: %v", err), http.StatusNotFound)
		return
	}

	params := extractParametersSchema(xrd)
	writeJSON(w, params)
}

// extractParametersSchema pulls spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.parameters
// from an XRD and returns it as a plain map for the Angular client.
func extractParametersSchema(xrd *unstructured.Unstructured) map[string]any {
	versions, _, _ := unstructured.NestedSlice(xrd.Object, "spec", "versions")
	if len(versions) == 0 {
		return nil
	}
	v, ok := versions[0].(map[string]any)
	if !ok {
		return nil
	}

	params, _, _ := nestedMap(v,
		"schema", "openAPIV3Schema",
		"properties", "spec",
		"properties", "parameters",
	)
	return params
}

// nestedMap walks a chain of keys through nested map[string]any values.
func nestedMap(obj map[string]any, keys ...string) (map[string]any, bool, error) {
	cur := obj
	for _, k := range keys {
		v, ok := cur[k]
		if !ok {
			return nil, false, nil
		}
		next, ok := v.(map[string]any)
		if !ok {
			return nil, false, fmt.Errorf("key %q is not an object", k)
		}
		cur = next
	}
	return cur, true, nil
}

// handleGetResourceValues fetches live spec.parameters from the cluster for a named XR.
func (a *app) handleGetResourceValues(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("name")
	resource := r.PathValue("resource")
	if !validWorkspaceName.MatchString(tenant) || !validWorkspaceName.MatchString(resource) {
		http.Error(w, "invalid workspace or resource name", http.StatusBadRequest)
		return
	}

	if a.dynClient == nil {
		http.Error(w, "k8s unavailable", http.StatusServiceUnavailable)
		return
	}

	// Determine kind from the broadcaster cache — it already has every known XR.
	a.bcast.mu.RLock()
	cacheKey := ""
	var kind string
	for k, v := range a.bcast.cache {
		// cache key is "tenant/kind/name"
		if v.Workspace == tenant && v.Name == resource {
			cacheKey = k
			kind = v.Kind
			break
		}
	}
	a.bcast.mu.RUnlock()

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if cacheKey == "" {
		// Fallback: scan all known plurals to find the resource.
		params, found := findResourceParams(ctx, a.dynClient, tenant, resource)
		if !found {
			http.Error(w, "resource not found in cluster", http.StatusNotFound)
			return
		}
		delete(params, "namespace")
		writeJSON(w, params)
		return
	}

	plural := kindToPlural[kind]
	gvr := schema.GroupVersionResource{Group: xrGroup, Version: xrVersion, Resource: plural}
	obj, err := xrClient(a.dynClient, gvr, kind, tenant).Get(ctx, resource, metav1.GetOptions{})
	if err != nil {
		http.Error(w, fmt.Sprintf("not found: %v", err), http.StatusNotFound)
		return
	}

	params, _, _ := unstructured.NestedMap(obj.Object, "spec", "parameters")
	delete(params, "namespace")
	writeJSON(w, params)
}

// kindToPlural maps ResourceKind → plural used in GVR.
var kindToPlural = map[string]string{
	"XSpa":           "xspas",
	"XApi":           "xapis",
	"XSql":           "xsqls",
	"XNoSql":         "xnosqls",
	"XObjectStorage": "xobjectstorages",
	"XTopic":         "xtopics",
	"XSubscription":  "xsubscriptions",
	"XWordpress":     "xwordpresses",
}

// namespacedKinds lists XR kinds whose CRD scope is Namespaced.
// All others are Cluster-scoped and do not require a namespace in Get calls.
var namespacedKinds = map[string]bool{
	"XWordpress": true,
}

func xrClient(client dynamic.Interface, gvr schema.GroupVersionResource, kind, namespace string) dynamic.ResourceInterface {
	if namespacedKinds[kind] {
		return client.Resource(gvr).Namespace(namespace)
	}
	return client.Resource(gvr)
}

// findResourceParams scans all known XR plurals to locate a resource by name+namespace.
func findResourceParams(ctx context.Context, client dynamic.Interface, tenant, name string) (map[string]any, bool) {
	for _, res := range watchedResources {
		gvr := schema.GroupVersionResource{Group: xrGroup, Version: xrVersion, Resource: res.plural}
		obj, err := xrClient(client, gvr, res.kind, tenant).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			continue
		}
		t, _, _ := unstructured.NestedString(obj.Object, "spec", "parameters", "namespace")
		// t == "" means this kind has no namespace param (e.g. XTopic, XSubscription) — accept the match.
		if t != "" && t != tenant {
			continue
		}
		params, _, _ := unstructured.NestedMap(obj.Object, "spec", "parameters")
		return params, true
	}
	return nil, false
}

// serveSchemaJSON is a helper used in tests.
func serveSchemaJSON(w http.ResponseWriter, schema map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(schema) //nolint:errcheck
}
