package discovery

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/TouwaStar/BlackLight/pkg/model"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Discoverer reads the cluster and builds a service graph.
type Discoverer struct {
	clientset  *kubernetes.Clientset
	restConfig *rest.Config
	namespace  string // empty = all namespaces
}

func (d *Discoverer) Clientset() *kubernetes.Clientset { return d.clientset }
func (d *Discoverer) RestConfig() *rest.Config         { return d.restConfig }
func (d *Discoverer) Namespace() string                { return d.namespace }

// NewDiscoverer creates a discoverer from in-cluster config (when running inside K8s) or KUBECONFIG.
func NewDiscoverer(namespace string) (*Discoverer, error) {
	var config *rest.Config
	var err error
	config, err = rest.InClusterConfig()
	if err != nil {
		loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
		config, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, nil).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("kube config: %w", err)
		}
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	return &Discoverer{clientset: clientset, restConfig: config, namespace: namespace}, nil
}

// Discover builds the graph from the cluster.
func (d *Discoverer) Discover(ctx context.Context) (*model.Graph, error) {
	g := &model.Graph{}
	nsList, err := d.listNamespaces(ctx)
	if err != nil {
		return nil, err
	}

	for _, ns := range nsList {
		// Deployments and StatefulSets (workloads)
		deployments, err := d.clientset.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list deployments in %s: %w", ns, err)
		}
		statefulSets, err := d.clientset.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list statefulsets in %s: %w", ns, err)
		}
		daemonSets, err := d.clientset.AppsV1().DaemonSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list daemonsets in %s: %w", ns, err)
		}
		cronJobs, err := d.clientset.BatchV1().CronJobs(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list cronjobs in %s: %w", ns, err)
		}
		jobs, err := d.clientset.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list jobs in %s: %w", ns, err)
		}
		// Services
		services, err := d.clientset.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list services in %s: %w", ns, err)
		}
		// Ingress
		ingresses, err := d.clientset.NetworkingV1().Ingresses(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list ingresses in %s: %w", ns, err)
		}

		// Add workload nodes and service -> workload edges (via selector match)
		for i := range deployments.Items {
			dep := &deployments.Items[i]
			id := model.NodeID("Deployment", ns, dep.Name)
			g.Nodes = append(g.Nodes, model.Node{
				ID:          id,
				Kind:        "Deployment",
				Namespace:   ns,
				Name:        dep.Name,
				DisplayName: ns + "/" + dep.Name,
				Labels:      dep.Labels,
				Annotations: dep.Annotations,
				Purpose:     purposeFromAnnotations(dep.Annotations),
			})
		}
		for i := range statefulSets.Items {
			sts := &statefulSets.Items[i]
			id := model.NodeID("StatefulSet", ns, sts.Name)
			g.Nodes = append(g.Nodes, model.Node{
				ID:          id,
				Kind:        "StatefulSet",
				Namespace:   ns,
				Name:        sts.Name,
				DisplayName: ns + "/" + sts.Name,
				Labels:      sts.Labels,
				Annotations: sts.Annotations,
				Purpose:     purposeFromAnnotations(sts.Annotations),
			})
		}
		for i := range daemonSets.Items {
			ds := &daemonSets.Items[i]
			id := model.NodeID("DaemonSet", ns, ds.Name)
			g.Nodes = append(g.Nodes, model.Node{
				ID:          id,
				Kind:        "DaemonSet",
				Namespace:   ns,
				Name:        ds.Name,
				DisplayName: ns + "/" + ds.Name,
				Labels:      ds.Labels,
				Annotations: ds.Annotations,
				Purpose:     purposeFromAnnotations(ds.Annotations),
			})
		}
		for i := range cronJobs.Items {
			cj := &cronJobs.Items[i]
			id := model.NodeID("CronJob", ns, cj.Name)
			g.Nodes = append(g.Nodes, model.Node{
				ID:          id,
				Kind:        "CronJob",
				Namespace:   ns,
				Name:        cj.Name,
				DisplayName: ns + "/" + cj.Name,
				Labels:      cj.Labels,
				Annotations: cj.Annotations,
				Purpose:     purposeFromAnnotations(cj.Annotations),
			})
		}
		for i := range jobs.Items {
			job := &jobs.Items[i]
			if isOwnedByCronJob(job.OwnerReferences) {
				continue
			}
			id := model.NodeID("Job", ns, job.Name)
			g.Nodes = append(g.Nodes, model.Node{
				ID:          id,
				Kind:        "Job",
				Namespace:   ns,
				Name:        job.Name,
				DisplayName: ns + "/" + job.Name,
				Labels:      job.Labels,
				Annotations: job.Annotations,
				Purpose:     purposeFromAnnotations(job.Annotations),
			})
		}

		// Add service nodes and edges: Service -> Workload (by selector)
		for i := range services.Items {
			svc := &services.Items[i]
			sid := model.NodeID("Service", ns, svc.Name)
			g.Nodes = append(g.Nodes, model.Node{
				ID:          sid,
				Kind:        "Service",
				Namespace:   ns,
				Name:        svc.Name,
				DisplayName: ns + "/" + svc.Name,
				Labels:      svc.Labels,
				Annotations: svc.Annotations,
				Purpose:     purposeFromAnnotations(svc.Annotations),
			})
			selector := svc.Spec.Selector
			if len(selector) == 0 {
				continue
			}
			// Match deployments
			for j := range deployments.Items {
				dep := &deployments.Items[j]
				if labelsMatch(selector, dep.Spec.Template.Labels) {
					wid := model.NodeID("Deployment", ns, dep.Name)
					g.Edges = append(g.Edges, model.Edge{From: sid, To: wid, Kind: "selector", Detail: "pods"})
				}
			}
			for j := range statefulSets.Items {
				sts := &statefulSets.Items[j]
				if labelsMatch(selector, sts.Spec.Template.Labels) {
					wid := model.NodeID("StatefulSet", ns, sts.Name)
					g.Edges = append(g.Edges, model.Edge{From: sid, To: wid, Kind: "selector", Detail: "pods"})
				}
			}
			for j := range daemonSets.Items {
				ds := &daemonSets.Items[j]
				if labelsMatch(selector, ds.Spec.Template.Labels) {
					wid := model.NodeID("DaemonSet", ns, ds.Name)
					g.Edges = append(g.Edges, model.Edge{From: sid, To: wid, Kind: "selector", Detail: "pods"})
				}
			}
			for j := range cronJobs.Items {
				cj := &cronJobs.Items[j]
				if labelsMatch(selector, cj.Spec.JobTemplate.Spec.Template.Labels) {
					wid := model.NodeID("CronJob", ns, cj.Name)
					g.Edges = append(g.Edges, model.Edge{From: sid, To: wid, Kind: "selector", Detail: "pods"})
				}
			}
			for j := range jobs.Items {
				job := &jobs.Items[j]
				if isOwnedByCronJob(job.OwnerReferences) {
					continue
				}
				if labelsMatch(selector, job.Spec.Template.Labels) {
					wid := model.NodeID("Job", ns, job.Name)
					g.Edges = append(g.Edges, model.Edge{From: sid, To: wid, Kind: "selector", Detail: "pods"})
				}
			}
		}

		// Ingress -> Service
		for i := range ingresses.Items {
			ing := &ingresses.Items[i]
			ingID := model.NodeID("Ingress", ns, ing.Name)
			g.Nodes = append(g.Nodes, model.Node{
				ID:          ingID,
				Kind:        "Ingress",
				Namespace:   ns,
				Name:        ing.Name,
				DisplayName: ns + "/" + ing.Name,
				Labels:      ing.Labels,
				Annotations: ing.Annotations,
				Purpose:     purposeFromAnnotations(ing.Annotations),
			})
			for _, rule := range ing.Spec.Rules {
				if rule.HTTP == nil {
					continue
				}
				for _, path := range rule.HTTP.Paths {
					backend := path.Backend.Service
					if backend == nil {
						continue
					}
					svcName := backend.Name
					sid := model.NodeID("Service", ns, svcName)
					detail := path.Path
					if detail == "" {
						detail = "/"
					}
					g.Edges = append(g.Edges, model.Edge{From: ingID, To: sid, Kind: "ingress_backend", Detail: detail})
				}
			}
			// Ingress may have default backend
			if ing.Spec.DefaultBackend != nil && ing.Spec.DefaultBackend.Service != nil {
				sid := model.NodeID("Service", ns, ing.Spec.DefaultBackend.Service.Name)
				g.Edges = append(g.Edges, model.Edge{From: ingID, To: sid, Kind: "ingress_backend", Detail: "default"})
			}
		}
	}

	// Traefik IngressRoute CRDs → Service edges
	d.addEdgesFromIngressRoutes(ctx, g, nsList)

	// Optional: infer from Pod env vars (e.g. SERVICE_X_HOST, DATABASE_URL)
	d.addEdgesFromPodEnv(ctx, g, nsList)
	return g, nil
}

func (d *Discoverer) listNamespaces(ctx context.Context) ([]string, error) {
	if d.namespace != "" {
		return []string{d.namespace}, nil
	}
	list, err := d.clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		names = append(names, list.Items[i].Name)
	}
	return names, nil
}

func isOwnedByCronJob(refs []metav1.OwnerReference) bool {
	for _, ref := range refs {
		if ref.Kind == "CronJob" {
			return true
		}
	}
	return false
}

func labelsMatch(selector, labels map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func purposeFromAnnotations(annotations map[string]string) string {
	if annotations == nil {
		return ""
	}
	// Common patterns teams use to describe purpose
	for _, key := range []string{"blacklight/purpose", "servicemap/purpose", "purpose", "description", "app.kubernetes.io/part-of"} {
		if v, ok := annotations[key]; ok && v != "" {
			return v
		}
	}
	return ""
}

// addEdgesFromIngressRoutes discovers Traefik IngressRoute CRDs and creates edges from the
// Traefik deployment to the backend services referenced in routes.
func (d *Discoverer) addEdgesFromIngressRoutes(ctx context.Context, g *model.Graph, namespaces []string) {
	// Find Traefik nodes by label or name convention.
	var traefikIDs []string
	for _, n := range g.Nodes {
		switch n.Kind {
		case "Deployment", "DaemonSet", "StatefulSet":
		default:
			continue
		}
		if n.Labels["app.kubernetes.io/name"] == "traefik" ||
			n.Labels["app"] == "traefik" ||
			strings.Contains(strings.ToLower(n.Name), "traefik") {
			traefikIDs = append(traefikIDs, n.ID)
		}
	}
	if len(traefikIDs) == 0 {
		return
	}

	dynClient, err := dynamic.NewForConfig(d.restConfig)
	if err != nil {
		log.Printf("dynamic client: %v", err)
		return
	}

	// Try both old and new Traefik CRD group names.
	gvrs := []schema.GroupVersionResource{
		{Group: "traefik.containo.us", Version: "v1alpha1", Resource: "ingressroutes"},
		{Group: "traefik.io", Version: "v1alpha1", Resource: "ingressroutes"},
	}

	existingIDs := make(map[string]bool)
	for _, n := range g.Nodes {
		existingIDs[n.ID] = true
	}
	seen := make(map[string]bool)

	for _, gvr := range gvrs {
		for _, ns := range namespaces {
			list, err := dynClient.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{})
			if err != nil {
				continue
			}
			for _, item := range list.Items {
				routeName := item.GetName()
				spec, ok := item.Object["spec"].(map[string]any)
				if !ok {
					continue
				}
				routes, ok := spec["routes"].([]any)
				if !ok {
					continue
				}
				for _, r := range routes {
					route, ok := r.(map[string]any)
					if !ok {
						continue
					}
					services, ok := route["services"].([]any)
					if !ok {
						continue
					}
					for _, s := range services {
						svc, ok := s.(map[string]any)
						if !ok {
							continue
						}
						name, ok := svc["name"].(string)
						if !ok || strings.Contains(name, "@") {
							continue // skip Traefik internal services (e.g. noop@file, api@internal)
						}
						targetID := model.NodeID("Service", ns, name)
						if !existingIDs[targetID] {
							continue
						}
						for _, tid := range traefikIDs {
							edgeKey := tid + "\t" + targetID
							if seen[edgeKey] {
								continue
							}
							seen[edgeKey] = true
							g.Edges = append(g.Edges, model.Edge{
								From:   tid,
								To:     targetID,
								Kind:   "ingress_backend",
								Detail: routeName,
							})
						}
					}
				}
			}
		}
	}
}

// addEdgesFromPodEnv scans pods for env vars that reference other services.
// It resolves both inline values and ConfigMapKeyRef references.
func (d *Discoverer) addEdgesFromPodEnv(ctx context.Context, g *model.Graph, namespaces []string) {
	existingIDs := make(map[string]bool)
	for _, n := range g.Nodes {
		existingIDs[n.ID] = true
	}

	// Dedup across pods: multiple replicas of the same workload produce identical edges.
	seenEdges := make(map[string]bool)
	// Cache ConfigMap data per namespace to avoid repeated API calls.
	cmCache := make(map[string]map[string]string) // "ns/name" → data

	for _, ns := range namespaces {
		pods, err := d.clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for i := range pods.Items {
			pod := &pods.Items[i]
			ownerKind, ownerName := d.getWorkloadOwner(ctx, ns, pod)
			if ownerKind == "" || ownerName == "" {
				continue
			}
			fromID := model.NodeID(ownerKind, ns, ownerName)
			if !existingIDs[fromID] {
				continue
			}
			refs := d.extractServiceRefsFromEnv(ctx, ns, pod.Spec.Containers, cmCache)
			for _, ref := range refs {
				targetID := model.NodeID("Service", ns, ref.serviceName)
				edgeKey := fromID + "\t" + targetID + "\t" + ref.envKey
				if !existingIDs[targetID] || seenEdges[edgeKey] {
					continue
				}
				seenEdges[edgeKey] = true
				g.Edges = append(g.Edges, model.Edge{
					From:   fromID,
					To:     targetID,
					Kind:   "env_ref",
					Detail: ref.envKey,
				})
			}
		}
	}
}

func (d *Discoverer) getWorkloadOwner(ctx context.Context, namespace string, pod *corev1.Pod) (kind, name string) {
	for _, ref := range pod.OwnerReferences {
		switch ref.Kind {
		case "ReplicaSet":
			rs, err := d.clientset.AppsV1().ReplicaSets(namespace).Get(ctx, ref.Name, metav1.GetOptions{})
			if err != nil {
				return "Deployment", ref.Name // fallback to RS name
			}
			for _, o := range rs.OwnerReferences {
				if o.Kind == "Deployment" {
					return "Deployment", o.Name
				}
			}
			return "ReplicaSet", ref.Name
		case "StatefulSet":
			return "StatefulSet", ref.Name
		case "DaemonSet":
			return "DaemonSet", ref.Name
		case "Job":
			job, err := d.clientset.BatchV1().Jobs(namespace).Get(ctx, ref.Name, metav1.GetOptions{})
			if err != nil {
				return "Job", ref.Name
			}
			for _, o := range job.OwnerReferences {
				if o.Kind == "CronJob" {
					return "CronJob", o.Name
				}
			}
			return "Job", ref.Name
		}
	}
	return "", ""
}

type serviceRef struct {
	serviceName string
	envKey      string
}

func (d *Discoverer) extractServiceRefsFromEnv(ctx context.Context, ns string, containers []corev1.Container, cmCache map[string]map[string]string) []serviceRef {
	var refs []serviceRef
	seen := make(map[string]bool)
	for _, c := range containers {
		for _, env := range c.Env {
			key := env.Name
			if !hasServiceRefSuffix(key) {
				continue
			}
			var val string
			if env.Value != "" {
				val = env.Value
			} else if env.ValueFrom != nil && env.ValueFrom.ConfigMapKeyRef != nil {
				val = d.resolveConfigMapKey(ctx, ns, env.ValueFrom.ConfigMapKeyRef.Name, env.ValueFrom.ConfigMapKeyRef.Key, cmCache)
			}
			if val == "" {
				continue
			}
			svcName := extractServiceNameFromValue(val)
			if svcName == "" || seen[svcName+key] {
				continue
			}
			seen[svcName+key] = true
			refs = append(refs, serviceRef{serviceName: svcName, envKey: key})
		}
	}
	return refs
}

func (d *Discoverer) resolveConfigMapKey(ctx context.Context, ns, cmName, cmKey string, cache map[string]map[string]string) string {
	cacheKey := ns + "/" + cmName
	data, ok := cache[cacheKey]
	if !ok {
		cm, err := d.clientset.CoreV1().ConfigMaps(ns).Get(ctx, cmName, metav1.GetOptions{})
		if err != nil {
			cache[cacheKey] = nil
			return ""
		}
		data = cm.Data
		cache[cacheKey] = data
	}
	if data == nil {
		return ""
	}
	return data[cmKey]
}

var envRefSuffixes = []string{
	"_HOST", "_URL", "_SERVICE", "_ADDR",
	"_ENDPOINT", "_BROKER", "_QUEUE", "_DB", "_REDIS", "_DSN",
}

func hasServiceRefSuffix(key string) bool {
	for _, suffix := range envRefSuffixes {
		if strings.HasSuffix(key, suffix) {
			return true
		}
	}
	return false
}

func extractServiceNameFromValue(val string) string {
	// Kubernetes DNS: <svc>.<ns>.svc.cluster.local or <svc>.<ns> or just <svc>
	// URLs: http://svc:port, https://svc.ns.svc.cluster.local
	val = strings.TrimPrefix(val, "http://")
	val = strings.TrimPrefix(val, "https://")
	// Strip port
	for i, r := range val {
		if r == ':' || r == '/' {
			val = val[:i]
			break
		}
	}
	// Take first segment (service name in same-namespace form)
	for i, r := range val {
		if r == '.' {
			val = val[:i]
			break
		}
	}
	// Reject empty, purely numeric (port numbers), or IP-address-like values
	if val == "" || isNumeric(val) {
		return ""
	}
	return val
}

func isNumeric(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// MergeSourceEdges matches source-derived edges against the K8s graph and adds them.
func MergeSourceEdges(g *model.Graph, sourceEdges []SourceEdge) {
	// Index nodes by name for matching.
	// Workloads may share a name across namespaces, so map to slices.
	workloadsByName := make(map[string][]nodeRef)
	var workloadList []nodeRef
	// Services indexed by name (same name across namespaces → slice).
	servicesByName := make(map[string][]nodeRef)

	for _, n := range g.Nodes {
		ref := nodeRef{id: n.ID, name: n.Name, namespace: n.Namespace}
		switch n.Kind {
		case "Deployment", "StatefulSet", "DaemonSet", "CronJob", "Job":
			workloadsByName[n.Name] = append(workloadsByName[n.Name], ref)
			workloadList = append(workloadList, ref)
		case "Service":
			servicesByName[n.Name] = append(servicesByName[n.Name], ref)
		}
	}

	seen := make(map[string]bool)
	for _, se := range sourceEdges {
		// Find all callee service nodes (exact match on name).
		targets := servicesByName[se.Callee]
		if len(targets) == 0 {
			continue
		}

		// Find all caller workload nodes.
		callers := workloadsByName[se.Caller]
		if len(callers) == 0 {
			// Fallback: substring match (e.g. source dir "assistant" → deployment "av-assistant-portal").
			callers = matchAllBySubstring(se.Caller, workloadList)
			if len(callers) == 0 {
				continue
			}
		}

		for _, fromRef := range callers {
			for _, targetRef := range targets {
				// Only connect within the same namespace (production→production, not production→local).
				if fromRef.namespace != targetRef.namespace {
					continue
				}
				edgeKey := fromRef.id + "\t" + targetRef.id
				if seen[edgeKey] {
					continue
				}
				seen[edgeKey] = true
				g.Edges = append(g.Edges, model.Edge{
					From:   fromRef.id,
					To:     targetRef.id,
					Kind:   "code_ref",
					Detail: se.File,
				})
			}
		}
	}
}

type nodeRef struct {
	id        string
	name      string
	namespace string
}

func matchAllBySubstring(name string, nodes []nodeRef) []nodeRef {
	var matches []nodeRef
	for _, n := range nodes {
		if strings.Contains(n.name, name) || strings.Contains(name, n.name) {
			matches = append(matches, n)
		}
	}
	return matches
}
