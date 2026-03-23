package discovery

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/TouwaStar/BlackLight/pkg/model"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
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
	return NewDiscovererWithContext("", namespace)
}

// NewDiscovererWithContext creates a discoverer using a specific kubeconfig context.
// Empty kubeContext uses the default context.
func NewDiscovererWithContext(kubeContext, namespace string) (*Discoverer, error) {
	var config *rest.Config
	var err error
	config, err = rest.InClusterConfig()
	if err != nil {
		loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
		overrides := &clientcmd.ConfigOverrides{}
		if kubeContext != "" {
			overrides.CurrentContext = kubeContext
		}
		config, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
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

// ListKubeContexts returns the available kubeconfig contexts and the current default.
func ListKubeContexts() (contexts []string, current string, err error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	rawConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, nil).RawConfig()
	if err != nil {
		return nil, "", fmt.Errorf("load kubeconfig: %w", err)
	}
	current = rawConfig.CurrentContext
	for name := range rawConfig.Contexts {
		contexts = append(contexts, name)
	}
	return contexts, current, nil
}

// ListNamespacesFor lists all namespace names reachable by the given discoverer.
func ListNamespacesFor(ctx context.Context, clientset *kubernetes.Clientset) ([]string, error) {
	list, err := clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		names = append(names, list.Items[i].Name)
	}
	return names, nil
}

// Discover builds the graph from the cluster.
func (d *Discoverer) Discover(ctx context.Context) (*model.Graph, error) {
	g := &model.Graph{}
	nsList, err := d.listNamespaces(ctx)
	if err != nil {
		return nil, err
	}

	for _, ns := range nsList {
		// Deployments and StatefulSets (workloads).
		// Tolerate partial failures: log and continue so a single timeout
		// doesn't throw away the entire graph.
		deployments, err := d.clientset.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			log.Printf("discover: list deployments in %s: %v", ns, err)
			deployments = nil
		}
		statefulSets, err := d.clientset.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			log.Printf("discover: list statefulsets in %s: %v", ns, err)
			statefulSets = nil
		}
		daemonSets, err := d.clientset.AppsV1().DaemonSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			log.Printf("discover: list daemonsets in %s: %v", ns, err)
			daemonSets = nil
		}
		cronJobs, err := d.clientset.BatchV1().CronJobs(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			log.Printf("discover: list cronjobs in %s: %v", ns, err)
			cronJobs = nil
		}
		jobs, err := d.clientset.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			log.Printf("discover: list jobs in %s: %v", ns, err)
			jobs = nil
		}
		// Services
		services, err := d.clientset.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			log.Printf("discover: list services in %s: %v", ns, err)
			services = nil
		}
		// Ingress
		ingresses, err := d.clientset.NetworkingV1().Ingresses(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			log.Printf("discover: list ingresses in %s: %v", ns, err)
			ingresses = nil
		}

		// Add workload nodes and service -> workload edges (via selector match)
		if deployments == nil {
			deployments = &appsv1.DeploymentList{}
		}
		if statefulSets == nil {
			statefulSets = &appsv1.StatefulSetList{}
		}
		if daemonSets == nil {
			daemonSets = &appsv1.DaemonSetList{}
		}
		if cronJobs == nil {
			cronJobs = &batchv1.CronJobList{}
		}
		if jobs == nil {
			jobs = &batchv1.JobList{}
		}
		if services == nil {
			services = &corev1.ServiceList{}
		}
		if ingresses == nil {
			ingresses = &networkingv1.IngressList{}
		}
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
				Meta: map[string]any{
					"replicas":      dep.Status.Replicas,
					"readyReplicas": dep.Status.ReadyReplicas,
					"strategy":      string(dep.Spec.Strategy.Type),
					"images":        containerImages(dep.Spec.Template.Spec.Containers),
				},
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
				Meta: map[string]any{
					"replicas":      sts.Status.Replicas,
					"readyReplicas": sts.Status.ReadyReplicas,
					"images":        containerImages(sts.Spec.Template.Spec.Containers),
				},
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
				Meta: map[string]any{
					"desired": ds.Status.DesiredNumberScheduled,
					"ready":   ds.Status.NumberReady,
					"images":  containerImages(ds.Spec.Template.Spec.Containers),
				},
			})
		}
		for i := range cronJobs.Items {
			cj := &cronJobs.Items[i]
			id := model.NodeID("CronJob", ns, cj.Name)
			meta := map[string]any{
				"schedule": cj.Spec.Schedule,
				"images":   containerImages(cj.Spec.JobTemplate.Spec.Template.Spec.Containers),
			}
			if cj.Status.LastScheduleTime != nil {
				meta["lastSchedule"] = cj.Status.LastScheduleTime.Time.Unix()
			}
			g.Nodes = append(g.Nodes, model.Node{
				ID:          id,
				Kind:        "CronJob",
				Namespace:   ns,
				Name:        cj.Name,
				DisplayName: ns + "/" + cj.Name,
				Labels:      cj.Labels,
				Annotations: cj.Annotations,
				Purpose:     purposeFromAnnotations(cj.Annotations),
				Meta:        meta,
			})
		}
		for i := range jobs.Items {
			job := &jobs.Items[i]
			if isOwnedByCronJob(job.OwnerReferences) {
				continue
			}
			id := model.NodeID("Job", ns, job.Name)
			meta := map[string]any{
				"images": containerImages(job.Spec.Template.Spec.Containers),
			}
			if job.Status.Succeeded > 0 {
				meta["succeeded"] = job.Status.Succeeded
			}
			if job.Status.Failed > 0 {
				meta["failed"] = job.Status.Failed
			}
			g.Nodes = append(g.Nodes, model.Node{
				ID:          id,
				Kind:        "Job",
				Namespace:   ns,
				Name:        job.Name,
				DisplayName: ns + "/" + job.Name,
				Labels:      job.Labels,
				Annotations: job.Annotations,
				Purpose:     purposeFromAnnotations(job.Annotations),
				Meta:        meta,
			})
		}

		// Add service nodes and edges: Service -> Workload (by selector)
		for i := range services.Items {
			svc := &services.Items[i]
			sid := model.NodeID("Service", ns, svc.Name)
			ports := make([]string, 0, len(svc.Spec.Ports))
			for _, p := range svc.Spec.Ports {
				entry := fmt.Sprintf("%d/%s", p.Port, p.Protocol)
				if p.Name != "" {
					entry = p.Name + ":" + entry
				}
				ports = append(ports, entry)
			}
			g.Nodes = append(g.Nodes, model.Node{
				ID:          sid,
				Kind:        "Service",
				Namespace:   ns,
				Name:        svc.Name,
				DisplayName: ns + "/" + svc.Name,
				Labels:      svc.Labels,
				Annotations: svc.Annotations,
				Purpose:     purposeFromAnnotations(svc.Annotations),
				Meta: map[string]any{
					"type":      string(svc.Spec.Type),
					"clusterIP": svc.Spec.ClusterIP,
					"ports":     ports,
				},
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
			var hosts []string
			for _, rule := range ing.Spec.Rules {
				if rule.Host != "" {
					hosts = append(hosts, rule.Host)
				}
			}
			ingressClass := ""
			if ing.Spec.IngressClassName != nil {
				ingressClass = *ing.Spec.IngressClassName
			}
			g.Nodes = append(g.Nodes, model.Node{
				ID:          ingID,
				Kind:        "Ingress",
				Namespace:   ns,
				Name:        ing.Name,
				DisplayName: ns + "/" + ing.Name,
				Labels:      ing.Labels,
				Annotations: ing.Annotations,
				Purpose:     purposeFromAnnotations(ing.Annotations),
				Meta: map[string]any{
					"hosts": hosts,
					"class": ingressClass,
				},
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

func containerImages(containers []corev1.Container) []string {
	images := make([]string, 0, len(containers))
	for _, c := range containers {
		images = append(images, c.Image)
	}
	return images
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
	// Cache ReplicaSet owner references to avoid per-pod Get calls.
	rsOwnerCache := make(map[string]ownerInfo) // "ns/rsName" → owner

	for _, ns := range namespaces {
		// Pre-fetch ReplicaSets for this namespace to avoid N individual Get calls.
		rsList, err := d.clientset.AppsV1().ReplicaSets(ns).List(ctx, metav1.ListOptions{})
		if err == nil {
			for i := range rsList.Items {
				rs := &rsList.Items[i]
				key := ns + "/" + rs.Name
				for _, o := range rs.OwnerReferences {
					if o.Kind == "Deployment" {
						rsOwnerCache[key] = ownerInfo{kind: "Deployment", name: o.Name}
						break
					}
				}
				if _, ok := rsOwnerCache[key]; !ok {
					rsOwnerCache[key] = ownerInfo{kind: "ReplicaSet", name: rs.Name}
				}
			}
		}

		pods, err := d.clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for i := range pods.Items {
			pod := &pods.Items[i]
			ownerKind, ownerName := getWorkloadOwnerCached(ns, pod, rsOwnerCache)
			if ownerKind == "" || ownerName == "" {
				continue
			}
			fromID := model.NodeID(ownerKind, ns, ownerName)
			if !existingIDs[fromID] {
				continue
			}
			refs := d.extractServiceRefsFromEnv(ctx, ns, pod.Spec.Containers, cmCache)
			for _, ref := range refs {
				// Try to match an in-cluster Service first.
				targetID := model.NodeID("Service", ns, ref.serviceName)
				if existingIDs[targetID] {
					edgeKey := fromID + "\t" + targetID + "\t" + ref.envKey
					if seenEdges[edgeKey] {
						continue
					}
					seenEdges[edgeKey] = true
					g.Edges = append(g.Edges, model.Edge{
						From:   fromID,
						To:     targetID,
						Kind:   "env_ref",
						Detail: ref.envKey,
					})
					continue
				}
				// No in-cluster match — if it's an external hostname, create an External node.
				if ref.hostname == "" {
					continue
				}
				targetID = model.NodeID("Cloud", "", ref.hostname)
				if !existingIDs[targetID] {
					g.Nodes = append(g.Nodes, model.Node{
						ID:          targetID,
						Kind:        "Cloud",
						Name:        ref.hostname,
						DisplayName: ref.hostname,
					})
					existingIDs[targetID] = true
				}
				edgeKey := fromID + "\t" + targetID + "\t" + ref.envKey
				if seenEdges[edgeKey] {
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

type ownerInfo struct {
	kind string
	name string
}

// getWorkloadOwnerCached resolves pod → workload using a pre-fetched ReplicaSet cache.
// Avoids per-pod API calls for ReplicaSet ownership resolution.
func getWorkloadOwnerCached(namespace string, pod *corev1.Pod, rsCache map[string]ownerInfo) (string, string) {
	for _, ref := range pod.OwnerReferences {
		switch ref.Kind {
		case "ReplicaSet":
			key := namespace + "/" + ref.Name
			if info, ok := rsCache[key]; ok {
				return info.kind, info.name
			}
			// Fallback: strip hash suffix to guess Deployment name.
			name := ref.Name
			if idx := strings.LastIndex(name, "-"); idx > 0 {
				name = name[:idx]
			}
			return "Deployment", name
		case "StatefulSet":
			return "StatefulSet", ref.Name
		case "DaemonSet":
			return "DaemonSet", ref.Name
		case "Job":
			return "Job", ref.Name
		}
	}
	return "", ""
}

type serviceRef struct {
	serviceName string // short name for in-cluster matching
	hostname    string // full hostname (empty for in-cluster refs)
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
			svcName, host := extractHostAndService(val)
			if svcName == "" && host == "" {
				continue
			}
			dedup := svcName + host + key
			if seen[dedup] {
				continue
			}
			seen[dedup] = true
			refs = append(refs, serviceRef{serviceName: svcName, hostname: host, envKey: key})
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

// extractHostAndService parses an env var value and returns:
//   - svcName: the short service name (first DNS segment) for in-cluster matching
//   - host: the full hostname (empty if it looks like a plain in-cluster ref)
func extractHostAndService(val string) (svcName, host string) {
	// Kubernetes DNS: <svc>.<ns>.svc.cluster.local or <svc>.<ns> or just <svc>
	// URLs: http://svc:port, https://svc.ns.svc.cluster.local
	val = strings.TrimPrefix(val, "http://")
	val = strings.TrimPrefix(val, "https://")
	// Strip userinfo (e.g. postgres://user:pass@host)
	if at := strings.Index(val, "@"); at >= 0 {
		val = val[at+1:]
	}
	// Strip port and path
	for i, r := range val {
		if r == ':' || r == '/' {
			val = val[:i]
			break
		}
	}
	if val == "" || isNumeric(val) {
		return "", ""
	}

	// Check if this is an external hostname (not k8s internal DNS).
	// k8s internal: svc, svc.ns, svc.ns.svc, svc.ns.svc.cluster.local
	// External: anything with dots that doesn't end in .svc.cluster.local
	// or has many segments suggesting a real FQDN.
	if isExternalHostname(val) {
		host = val
	}

	// Take first segment as service name for in-cluster matching.
	svcName = val
	for i, r := range val {
		if r == '.' {
			svcName = val[:i]
			break
		}
	}
	if svcName == "" || isNumeric(svcName) {
		return "", host
	}
	return svcName, host
}

// isExternalHostname returns true if hostname looks like an external FQDN
// rather than a Kubernetes internal DNS name.
func isExternalHostname(h string) bool {
	// No dots = plain service name (in-cluster)
	if !strings.Contains(h, ".") {
		return false
	}
	// k8s internal patterns: svc.ns.svc.cluster.local, svc.ns.svc, svc.ns
	lower := strings.ToLower(h)
	if strings.HasSuffix(lower, ".svc.cluster.local") || strings.HasSuffix(lower, ".svc") {
		return false
	}
	// Count segments — k8s internal is typically 2 segments (svc.ns).
	// External FQDNs usually have 3+ segments or contain known external indicators.
	parts := strings.Split(h, ".")
	if len(parts) >= 3 {
		return true
	}
	// 2-segment names like "redis.default" are k8s internal.
	return false
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
