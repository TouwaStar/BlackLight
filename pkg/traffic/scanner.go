package traffic

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TouwaStar/BlackLight/pkg/model"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

const (
	// ExecTimeout is the per-pod timeout for reading /proc/net/tcp.
	ExecTimeout = 5 * time.Second
	// ExecCooldown is how long a pod is skipped after a failed exec.
	ExecCooldown = 2 * time.Minute
	// MaxExecWorkers is the max concurrent pod exec calls.
	MaxExecWorkers = 20
	// MaxDNSWorkers is the max concurrent reverse DNS lookups.
	MaxDNSWorkers = 20
	// DNSTimeout is the per-IP timeout for reverse DNS lookups.
	DNSTimeout = time.Second
	// PartialResultDelay is how long the exec phase must run before emitting partial results.
	PartialResultDelay = 3 * time.Second
)

// Scanner reads /proc/net/tcp from pods to discover live TCP connections.
type Scanner struct {
	clientset  *kubernetes.Clientset
	restConfig *rest.Config
	namespace  string

	dnsCache   sync.Map // IP string → bool (isCloud), persists across scans
	failedPods sync.Map // pod name → time.Time (skip exec until this time)
}

func NewScanner(clientset *kubernetes.Clientset, restConfig *rest.Config, namespace string) *Scanner {
	return &Scanner{clientset: clientset, restConfig: restConfig, namespace: namespace}
}

type podRef struct {
	namespace string
	name      string
	container string
}

// Scan runs one full traffic scan across all workloads.
func (s *Scanner) Scan(ctx context.Context) (*model.TrafficSnapshot, error) {
	return s.ScanWithProgress(ctx, nil)
}

// ScanWithProgress runs a traffic scan, calling onProgress with partial results
// as pods respond (at most once per second). Returns the final complete snapshot.
func (s *Scanner) ScanWithProgress(ctx context.Context, onProgress func(*model.TrafficSnapshot)) (*model.TrafficSnapshot, error) {
	scanStart := time.Now()
	nsList, err := s.listNamespaces(ctx)
	if err != nil {
		return nil, err
	}
	scanSet := make(map[string]bool, len(nsList))
	for _, ns := range nsList {
		scanSet[ns] = true
	}
	// Build IP lookup tables.
	serviceClusterIPs := make(map[string]string) // ClusterIP → node ID
	podIPs := make(map[string]string)            // PodIP → workload node ID
	knownIPs := make(map[string]bool)             // all cluster-internal IPs (nodes, system pods)
	workloadPods := make(map[string]podRef)       // workload node ID → one pod

	// Collect node IPs (kubelet health checks, kube-proxy come from these).
	nodes, err := s.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err == nil {
		for i := range nodes.Items {
			for _, addr := range nodes.Items[i].Status.Addresses {
				knownIPs[addr.Address] = true
			}
		}
	}

	// Single cluster-wide listing for services and pods (instead of per-namespace loops).
	svcs, err := s.clientset.CoreV1().Services("").List(ctx, metav1.ListOptions{})
	if err == nil {
		for i := range svcs.Items {
			svc := &svcs.Items[i]
			if svc.Spec.ClusterIP != "" && svc.Spec.ClusterIP != "None" {
				serviceClusterIPs[svc.Spec.ClusterIP] = model.NodeID("Service", svc.Namespace, svc.Name)
				knownIPs[svc.Spec.ClusterIP] = true
			}
		}
	}

	allPods, err := s.clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "status.phase=Running",
	})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	for i := range allPods.Items {
		pod := &allPods.Items[i]
		if pod.Status.PodIP == "" {
			continue
		}
		knownIPs[pod.Status.PodIP] = true
		ownerKind, ownerName := WorkloadOwnerFromPod(pod)
		if ownerKind == "" {
			continue
		}
		ns := pod.Namespace
		wid := model.NodeID(ownerKind, ns, ownerName)
		podIPs[pod.Status.PodIP] = wid
		// Only track pods in scanned namespaces for exec.
		if scanSet[ns] {
			if _, exists := workloadPods[wid]; !exists {
				cn := PickRunningContainer(pod)
				workloadPods[wid] = podRef{namespace: ns, name: pod.Name, container: cn}
			}
		}
	}
	// Exec into one pod per workload and parse connections (parallel).
	type connKey struct{ source, target string }
	type extKey struct{ wid, ip string }

	type podResult struct {
		wid     string
		entries []tcpEntry
	}

	// Fan out exec calls with bounded concurrency.
	const maxWorkers = MaxExecWorkers
	resultCh := make(chan podResult, len(workloadPods))
	sem := make(chan struct{}, maxWorkers)
	var wg sync.WaitGroup

	execNow := time.Now()
	for wid, pr := range workloadPods {
		// Skip pods that recently failed exec (2-minute cooldown).
		if expiry, ok := s.failedPods.Load(pr.name); ok {
			if t, valid := expiry.(time.Time); valid && execNow.Before(t) {
				continue
			}
			s.failedPods.Delete(pr.name)
		}
		wg.Add(1)
		go func(wid string, pr podRef) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			execCtx, cancel := context.WithTimeout(ctx, ExecTimeout)
			raw, err := s.execReadTCP(execCtx, pr.namespace, pr.name, pr.container)
			cancel()
			if err != nil {
				if !strings.Contains(err.Error(), "executable file not found") &&
					!strings.Contains(err.Error(), "container not found") {
					log.Printf("traffic: skip %s: %v", pr.name, err)
				}
				s.failedPods.Store(pr.name, time.Now().Add(ExecCooldown))
				return
			}
			resultCh <- podResult{wid: wid, entries: parseProcNetTCP(raw)}
		}(wid, pr)
	}

	// Close channel when all workers finish; process results as they arrive.
	go func() {
		wg.Wait()
		close(resultCh)
	}()

	// Aggregate results progressively.
	connCounts := make(map[connKey]int)
	errCounts := make(map[connKey]int)
	extPerIP := make(map[extKey]int)
	lastNotify := time.Time{}

	for res := range resultCh {
		for _, e := range res.entries {
			remoteIP := e.RemoteIP.String()
			var targetID string
			if tid, ok := serviceClusterIPs[remoteIP]; ok {
				targetID = tid
			} else if tid, ok := podIPs[remoteIP]; ok && tid != res.wid {
				targetID = tid
			}
			if targetID != "" {
				key := connKey{res.wid, targetID}
				if e.IsError {
					errCounts[key]++
				} else {
					connCounts[key]++
				}
			} else if !e.IsError && !knownIPs[remoteIP] && !isPrivateIP(e.RemoteIP) {
				extPerIP[extKey{res.wid, remoteIP}]++
			}
		}

		// Emit partial results only if exec is taking a while (>3s). For fast scans,
		// partials cause visual flicker as edges toggle active→idle→active.
		if onProgress != nil && time.Since(execNow) > PartialResultDelay && time.Since(lastNotify) > time.Second {
			partial := &model.TrafficSnapshot{Timestamp: time.Now().UnixMilli()}
			pKeys := make(map[connKey]bool)
			for k := range connCounts {
				pKeys[k] = true
			}
			for k := range errCounts {
				pKeys[k] = true
			}
			for key := range pKeys {
				partial.Connections = append(partial.Connections, model.TrafficConnection{
					SourceWorkload: key.source,
					TargetService:  key.target,
					ConnCount:      connCounts[key],
					ErrorCount:     errCounts[key],
				})
			}
			onProgress(partial)
			lastNotify = time.Now()
		}
	}

	snap := &model.TrafficSnapshot{Timestamp: time.Now().UnixMilli()}
	// Collect all keys from both maps.
	allKeys := make(map[connKey]bool)
	for k := range connCounts {
		allKeys[k] = true
	}
	for k := range errCounts {
		allKeys[k] = true
	}
	for key := range allKeys {
		snap.Connections = append(snap.Connections, model.TrafficConnection{
			SourceWorkload: key.source,
			TargetService:  key.target,
			ConnCount:      connCounts[key],
			ErrorCount:     errCounts[key],
		})
	}
	// Classify external IPs as cloud infra or truly external via cached reverse DNS.
	// Parallelize lookups — sequential DNS for 300+ IPs takes minutes.
	cloudIPs := make(map[string]bool)
	uniqueIPs := make(map[string]bool, len(extPerIP))
	for ek := range extPerIP {
		uniqueIPs[ek.ip] = true
	}
	var dnsMu sync.Mutex
	var dnsWg sync.WaitGroup
	dnsSem := make(chan struct{}, MaxDNSWorkers)
	for ip := range uniqueIPs {
		dnsWg.Add(1)
		go func(ip string) {
			defer dnsWg.Done()
			dnsSem <- struct{}{}
			defer func() { <-dnsSem }()
			if s.isCloudIPCached(ip) {
				dnsMu.Lock()
				cloudIPs[ip] = true
				dnsMu.Unlock()
			}
		}(ip)
	}
	dnsWg.Wait()

	// Aggregate per workload, splitting cloud vs external.
	type ipBucket struct {
		cloud    map[string]int
		external map[string]int
	}
	byWID := make(map[string]*ipBucket)
	for ek, count := range extPerIP {
		b := byWID[ek.wid]
		if b == nil {
			b = &ipBucket{cloud: make(map[string]int), external: make(map[string]int)}
			byWID[ek.wid] = b
		}
		if cloudIPs[ek.ip] {
			b.cloud[ek.ip] += count
		} else {
			b.external[ek.ip] += count
		}
	}
	for wid, b := range byWID {
		if len(b.external) > 0 {
			snap.External = append(snap.External, buildExtTraffic(wid, b.external))
		}
		if len(b.cloud) > 0 {
			snap.Cloud = append(snap.Cloud, buildExtTraffic(wid, b.cloud))
		}
	}
	log.Printf("traffic scan: %v (%d connections, %d workloads)", time.Since(scanStart), len(snap.Connections), len(workloadPods))
	return snap, nil
}

func (s *Scanner) execReadTCP(ctx context.Context, namespace, podName, container string) (string, error) {
	req := s.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   []string{"cat", "/proc/net/tcp", "/proc/net/tcp6"},
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(s.restConfig, "POST", req.URL())
	if err != nil {
		return "", fmt.Errorf("spdy: %w", err)
	}
	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}); err != nil {
		return "", fmt.Errorf("exec: %w", err)
	}
	return stdout.String(), nil
}

// TCP connection states from /proc/net/tcp (hex-encoded).
const (
	tcpEstablished = "01"
	tcpSynSent     = "02"
	tcpTimeWait    = "06"
	tcpCloseWait   = "08"
)

type tcpEntry struct {
	LocalIP    net.IP
	LocalPort  uint16
	RemoteIP   net.IP
	RemotePort uint16
	IsError    bool // SYN_SENT or CLOSE_WAIT
}

func parseProcNetTCP(raw string) []tcpEntry {
	var entries []tcpEntry
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "sl") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		state := fields[3]
		isError := state == tcpSynSent || state == tcpCloseWait
		if state != tcpEstablished && state != tcpTimeWait && !isError {
			continue
		}
		localIP, localPort, ok1 := parseHexAddr(fields[1])
		remoteIP, remotePort, ok2 := parseHexAddr(fields[2])
		if !ok1 || !ok2 {
			continue
		}
		entries = append(entries, tcpEntry{
			LocalIP: localIP, LocalPort: localPort,
			RemoteIP: remoteIP, RemotePort: remotePort,
			IsError: isError,
		})
	}
	return entries
}

func parseHexAddr(s string) (net.IP, uint16, bool) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return nil, 0, false
	}
	port64, err := strconv.ParseUint(parts[1], 16, 16)
	if err != nil {
		return nil, 0, false
	}
	ipBytes, err := hex.DecodeString(parts[0])
	if err != nil {
		return nil, 0, false
	}
	var ip net.IP
	switch len(ipBytes) {
	case 4:
		// IPv4: stored little-endian in /proc/net/tcp
		ip = net.IPv4(ipBytes[3], ipBytes[2], ipBytes[1], ipBytes[0])
	case 16:
		// IPv6: 4 groups of 4 bytes, each group little-endian
		decoded := make([]byte, 16)
		for i := 0; i < 4; i++ {
			decoded[i*4+0] = ipBytes[i*4+3]
			decoded[i*4+1] = ipBytes[i*4+2]
			decoded[i*4+2] = ipBytes[i*4+1]
			decoded[i*4+3] = ipBytes[i*4+0]
		}
		ip = net.IP(decoded)
		if v4 := ip.To4(); v4 != nil {
			ip = v4
		}
	default:
		return nil, 0, false
	}
	return ip, uint16(port64), true
}

// isPrivateIP returns true for RFC 1918, CGNAT, link-local, and loopback addresses.
func isPrivateIP(ip net.IP) bool {
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsPrivate()
}

// isCloudIPCached checks reverse DNS with caching to detect cloud provider IPs.
func (s *Scanner) isCloudIPCached(ip string) bool {
	if val, ok := s.dnsCache.Load(ip); ok {
		return val.(bool)
	}
	result := false
	resolver := &net.Resolver{}
	ctx, cancel := context.WithTimeout(context.Background(), DNSTimeout)
	defer cancel()
	names, err := resolver.LookupAddr(ctx, ip)
	if err == nil && len(names) > 0 {
		host := strings.ToLower(names[0])
		for _, suffix := range cloudDNSSuffixes {
			if strings.Contains(host, suffix) {
				result = true
				break
			}
		}
	}
	s.dnsCache.Store(ip, result)
	return result
}

var cloudDNSSuffixes = []string{
	"amazonaws.com",
	"azure.com",
	"cloudapp.azure.com",
	"googleusercontent.com",
	"cloud.google.com",
}

// buildExtTraffic aggregates an IP→count map into an ExternalTraffic entry.
func buildExtTraffic(wid string, ips map[string]int) model.ExternalTraffic {
	total := 0
	var topIPs []model.IPCount
	for ip, count := range ips {
		total += count
		topIPs = append(topIPs, model.IPCount{IP: ip, Count: count})
	}
	sortIPCounts(topIPs)
	if len(topIPs) > 5 {
		topIPs = topIPs[:5]
	}
	return model.ExternalTraffic{NodeID: wid, ConnCount: total, TopIPs: topIPs}
}

func sortIPCounts(s []model.IPCount) {
	sort.Slice(s, func(i, j int) bool { return s[i].Count > s[j].Count })
}

// PickRunningContainer returns the name of a running container in the pod.
// Prefers containers that are actually running over the first spec entry.
func PickRunningContainer(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Ready && cs.State.Running != nil {
			return cs.Name
		}
	}
	// Fallback: first container from spec.
	if len(pod.Spec.Containers) > 0 {
		return pod.Spec.Containers[0].Name
	}
	return ""
}

// WorkloadOwnerFromPod derives workload kind+name from pod owner references.
// Strips the RS hash suffix to get the Deployment name (avoids extra API calls).
func WorkloadOwnerFromPod(pod *corev1.Pod) (string, string) {
	for _, ref := range pod.OwnerReferences {
		switch ref.Kind {
		case "ReplicaSet":
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

// systemNamespaces are skipped for traffic scanning — their pods typically
// lack a shell and their connections aren't relevant to service maps.
var systemNamespaces = map[string]bool{
	"kube-system":     true,
	"kube-public":     true,
	"kube-node-lease": true,
}

func (s *Scanner) listNamespaces(ctx context.Context) ([]string, error) {
	if s.namespace != "" {
		return []string{s.namespace}, nil
	}
	list, err := s.clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		ns := list.Items[i].Name
		if systemNamespaces[ns] {
			continue
		}
		names = append(names, ns)
	}
	return names, nil
}
