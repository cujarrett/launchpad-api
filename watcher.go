package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// InitContainerStatus carries the state of a binding-wait init container.
type InitContainerStatus struct {
	Name       string `json:"name"`
	Binding    string `json:"binding"`
	Completed  bool   `json:"completed"`
	FinishedAt string `json:"finishedAt,omitempty"`
}

// ResourceStatus is broadcast to SSE clients.
// Kind "Pod" carries init container binding status; all other kinds are XR status.
type ResourceStatus struct {
	Workspace      string                `json:"workspace"`
	Kind           string                `json:"kind"`
	Name           string                `json:"name"`
	Synced         bool                  `json:"synced"`
	Ready          bool                  `json:"ready"`
	Message        string                `json:"message,omitempty"`
	InitContainers []InitContainerStatus `json:"initContainers,omitempty"`
}

type xrResource struct {
	plural string
	kind   string
}

var watchedResources = []xrResource{
	{"xspas", "XSpa"},
	{"xapis", "XApi"},
	{"xsqls", "XSql"},
	{"xnosqls", "XNoSql"},
	{"xobjectstorages", "XObjectStorage"},
	{"xtopics", "XTopic"},
	{"xsubscriptions", "XSubscription"},
	{"xwordpresses", "XWordpress"},
}

const xrGroup = "platform.local.lab"
const xrVersion = "v1alpha1"

// startWatcher opens a K8s watch for every platform XR kind and publishes
// status events to the broadcaster for fan-out to SSE clients.
func startWatcher(ctx context.Context, b *broadcaster, client dynamic.Interface) {
	if client == nil {
		slog.Warn("watcher disabled: no K8s client")
		return
	}
	for _, res := range watchedResources {
		go watchResource(ctx, client, b, res)
	}
}

func watchResource(ctx context.Context, client dynamic.Interface, b *broadcaster, res xrResource) {
	gvr := schema.GroupVersionResource{Group: xrGroup, Version: xrVersion, Resource: res.plural}

	for {
		if ctx.Err() != nil {
			return
		}

		// List existing resources to seed the broadcaster cache with current state.
		// This ensures Ready resources are visible immediately after a server restart.
		list, err := client.Resource(gvr).List(ctx, metav1.ListOptions{})
		if err != nil {
			slog.Error("list for watch", "resource", res.plural, "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
				continue
			}
		}
		for i := range list.Items {
			if s := extractStatus(&list.Items[i], res.kind); s != nil {
				b.publish(*s)
			}
		}

		// Watch from the resource version returned by the list so we don't miss events.
		watcher, err := client.Resource(gvr).Watch(ctx, metav1.ListOptions{
			ResourceVersion: list.GetResourceVersion(),
		})
		if err != nil {
			slog.Error("watch start", "resource", res.plural, "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
				continue
			}
		}

		slog.Info("watching", "resource", res.plural, "resourceVersion", list.GetResourceVersion())

	watchLoop:
		for {
			select {
			case <-ctx.Done():
				watcher.Stop()
				return
			case event, ok := <-watcher.ResultChan():
				if !ok {
					slog.Warn("watch channel closed, restarting", "resource", res.plural)
					watcher.Stop()
					break watchLoop
				}
				if event.Type == watch.Error {
					continue
				}
				obj, ok := event.Object.(*unstructured.Unstructured)
				if !ok {
					continue
				}
				if event.Type == watch.Deleted {
					s := extractStatus(obj, res.kind)
					if s != nil {
						b.evict(s.Workspace, s.Kind, s.Name)
					}
					continue
				}
				s := extractStatus(obj, res.kind)
				if s == nil {
					continue
				}
				b.publish(*s)
			}
		}

		// Brief pause before restarting the list-watch cycle.
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// extractStatus pulls Crossplane conditions from an XR object.
// Workspace resolution order:
//  1. spec.parameters.namespace  — set by most XRs (XSpa, XApi, etc.)
//  2. metadata.namespace         — set for namespaced XRs (XWordpress)
//  3. argocd tracking annotation — set for cluster-scoped XRs with no namespace
//     param (XTopic, XSubscription): "app:group/Kind:namespace/name"
func extractStatus(obj *unstructured.Unstructured, kind string) *ResourceStatus {
	workspace, _, _ := unstructured.NestedString(obj.Object, "spec", "parameters", "namespace")
	if workspace == "" {
		workspace = obj.GetNamespace()
	}
	if workspace == "" {
		if tracking := obj.GetAnnotations()["argocd.argoproj.io/tracking-id"]; tracking != "" {
			// format: "appName:group/Kind:namespace/resourceName"
			parts := strings.SplitN(tracking, ":", 3)
			if len(parts) == 3 {
				workspace = strings.SplitN(parts[2], "/", 2)[0]
			}
		}
	}
	slog.Debug("extractStatus", "kind", kind, "name", obj.GetName(), "workspace", workspace)
	if workspace == "" {
		return nil
	}

	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")

	var synced, ready bool
	var message string

	for _, c := range conditions {
		cm, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		t, _ := cm["type"].(string)
		s, _ := cm["status"].(string)
		msg, _ := cm["message"].(string)

		switch t {
		case "Synced":
			synced = s == "True"
			if s != "True" && msg != "" {
				message = msg
			}
		case "Ready":
			ready = s == "True"
			if s != "True" && msg != "" && message == "" {
				message = msg
			}
		}
	}

	return &ResourceStatus{
		Workspace: workspace,
		Kind:      kind,
		Name:      obj.GetName(),
		Synced:    synced,
		Ready:     ready,
		Message:   message,
	}
}

// startPodWatcher watches pods across all guest-* namespaces and broadcasts
// init container binding status so the SPA can show real wait events.
func startPodWatcher(ctx context.Context, b *broadcaster, client dynamic.Interface) {
	if client == nil {
		return
	}
	go watchPods(ctx, client, b)
}

func watchPods(ctx context.Context, client dynamic.Interface, b *broadcaster) {
	podGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}

	for {
		if ctx.Err() != nil {
			return
		}

		list, err := client.Resource(podGVR).Namespace("").List(ctx, metav1.ListOptions{})
		if err != nil {
			slog.Error("pod list for watch", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
				continue
			}
		}
		for i := range list.Items {
			if s := extractPodStatus(&list.Items[i]); s != nil {
				b.publish(*s)
			}
		}

		watcher, err := client.Resource(podGVR).Namespace("").Watch(ctx, metav1.ListOptions{
			ResourceVersion: list.GetResourceVersion(),
		})
		if err != nil {
			slog.Error("pod watch start", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
				continue
			}
		}

		slog.Info("watching pods")

	podWatchLoop:
		for {
			select {
			case <-ctx.Done():
				watcher.Stop()
				return
			case event, ok := <-watcher.ResultChan():
				if !ok {
					slog.Warn("pod watch channel closed, restarting")
					watcher.Stop()
					break podWatchLoop
				}
				if event.Type == watch.Error {
					continue
				}
				obj, ok := event.Object.(*unstructured.Unstructured)
				if !ok {
					continue
				}
				if event.Type == watch.Deleted {
					ns := obj.GetNamespace()
					if strings.HasPrefix(ns, "guest-") {
						b.evict(ns, "Pod", obj.GetName())
					}
					continue
				}
				if s := extractPodStatus(obj); s != nil {
					b.publish(*s)
				}
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func extractPodStatus(obj *unstructured.Unstructured) *ResourceStatus {
	ns := obj.GetNamespace()
	if !strings.HasPrefix(ns, "guest-") {
		return nil
	}

	initStatuses, _, _ := unstructured.NestedSlice(obj.Object, "status", "initContainerStatuses")
	var initContainers []InitContainerStatus
	for _, raw := range initStatuses {
		ism, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := ism["name"].(string)
		binding := bindingFromInitContainerName(name)
		if binding == "" {
			continue
		}
		ready, _ := ism["ready"].(bool)
		ics := InitContainerStatus{Name: name, Binding: binding, Completed: ready}
		if state, ok := ism["state"].(map[string]interface{}); ok {
			if term, ok := state["terminated"].(map[string]interface{}); ok {
				ics.FinishedAt, _ = term["finishedAt"].(string)
			}
		}
		initContainers = append(initContainers, ics)
	}

	// Only broadcast pods that have binding init containers (i.e. XApi pods).
	if len(initContainers) == 0 {
		return nil
	}

	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	var initialized, ready bool
	for _, raw := range conditions {
		cm, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		t, _ := cm["type"].(string)
		s, _ := cm["status"].(string)
		switch t {
		case "Initialized":
			initialized = s == "True"
		case "Ready":
			ready = s == "True"
		}
	}

	return &ResourceStatus{
		Workspace:      ns,
		Kind:           "Pod",
		Name:           obj.GetName(),
		Synced:         initialized,
		Ready:          ready,
		InitContainers: initContainers,
	}
}

// bindingFromInitContainerName extracts the binding type from a wait-for-*-binding
// container name. Returns "" for unrecognised names.
//
//	wait-for-sql-binding                        → "sql"
//	wait-for-cache-binding                      → "cache"
//	wait-for-nosql-binding                      → "nosql"
//	wait-for-object-storage-{name}-binding      → "object-storage"
func bindingFromInitContainerName(name string) string {
	const prefix = "wait-for-"
	const suffix = "-binding"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
		return ""
	}
	inner := name[len(prefix) : len(name)-len(suffix)]
	if strings.HasPrefix(inner, "object-storage") {
		return "object-storage"
	}
	return inner
}

func loadKubeconfig() (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, _ := os.UserHomeDir()
		kubeconfig = filepath.Join(home, ".kube", "config")
	}
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}
