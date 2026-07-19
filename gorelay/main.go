package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	links       = make(map[string]*Link)
	linksMu     sync.RWMutex
	connections = make(map[string]*Conn)
	connMu      sync.RWMutex
	totalBytes  uint64
	totalReqs   uint64
	totalErrs   uint64
	startTime   = time.Now()
	pythonPort  string
)

type Link struct {
	ID              string `json:"id"`
	Label           string `json:"label"`
	LimitBytes      int64  `json:"limit_bytes"`
	UsedBytes       int64  `json:"used_bytes"`
	Active          bool   `json:"active"`
	ExpiresAt       string `json:"expires_at,omitempty"`
	Protocol        string `json:"protocol"`
	Fingerprint     string `json:"fingerprint"`
	ALPN            string `json:"alpn"`
	Port            int    `json:"port"`
	IPLimit         int    `json:"ip_limit"`
	SpeedLimitBytes int64  `json:"speed_limit_bytes"`
}

type Conn struct {
	UUID        string
	IP          string
	Transport   string
	ConnectedAt time.Time
	Bytes       int64
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8000"
	}
	pythonPort = os.Getenv("PYTHON_PORT")
	if pythonPort == "" {
		pythonPort = "8001"
	}

	loadState()

	go reloadStateLoop()

	pythonURL, _ := url.Parse("http://127.0.0.1:" + pythonPort)
	proxy := httputil.NewSingleHostReverseProxy(pythonURL)

	director := proxy.Director
	proxy.Director = func(req *http.Request) {
		director(req)
		req.Host = req.URL.Host
	}
	proxy.Transport = &http.Transport{
		MaxIdleConns:        500,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws/", handleWebSocket)
	mux.HandleFunc("/xhttp-siz10/", handleXHTTP)
	mux.HandleFunc("/proxy/", handleHTTPProxy)
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		proxy.ServeHTTP(w, r)
	})

	addr := "0.0.0.0:" + port
	log.Printf("Cyber-Rage Go Relay starting on %s (proxying to Python on %s)", addr, pythonPort)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":      "ok",
		"relay":       "go",
		"connections": len(connections),
		"uptime":      time.Since(startTime).String(),
	})
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/ws/")
	parts := strings.SplitN(path, "/", 2)
	uuid := parts[0]
	if uuid == "" {
		http.Error(w, "missing uuid", http.StatusBadRequest)
		return
	}

	linksMu.RLock()
	link := links[uuid]
	linksMu.RUnlock()

	if !isLinkAllowed(link) {
		http.Error(w, "not authorized", http.StatusForbidden)
		return
	}

	ip := clientIP(r)
	if !isIPAllowed(link, uuid, ip) {
		http.Error(w, "ip limit", http.StatusForbidden)
		return
	}

	target := r.URL.Query().Get("target")
	if target == "" {
		target = "127.0.0.1:" + pythonPort
	}

	upgrader := &net.Dialer{Timeout: 10 * time.Second}
	targetConn, err := upgrader.Dial("tcp", target)
	if err != nil {
		http.Error(w, "target connect failed", http.StatusBadGateway)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		targetConn.Close()
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusSwitchingProtocols)
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		targetConn.Close()
		return
	}

	connID := fmt.Sprintf("%d", time.Now().UnixNano())
	connMu.Lock()
	connections[connID] = &Conn{
		UUID:        uuid,
		IP:          ip,
		Transport:   "vless-ws",
		ConnectedAt: time.Now(),
		Bytes:       0,
	}
	connMu.Unlock()

	done := make(chan struct{})
	go func() {
		n, _ := io.CopyBuffer(targetConn, clientConn, make([]byte, 4*1024*1024))
		atomic.AddUint64(&totalBytes, uint64(n))
		connMu.Lock()
		if c, ok := connections[connID]; ok {
			c.Bytes = n
		}
		connMu.Unlock()
		close(done)
	}()
	go func() {
		n, _ := io.CopyBuffer(clientConn, targetConn, make([]byte, 4*1024*1024))
		atomic.AddUint64(&totalBytes, uint64(n))
		<-done
		clientConn.Close()
		targetConn.Close()
		connMu.Lock()
		delete(connections, connID)
		connMu.Unlock()
	}()
}

func handleXHTTP(w http.ResponseWriter, r *http.Request) {
	proxyToPython(w, r)
}

func handleHTTPProxy(w http.ResponseWriter, r *http.Request) {
	proxyToPython(w, r)
}

func proxyToPython(w http.ResponseWriter, r *http.Request) {
	target := "http://127.0.0.1:" + pythonPort + r.URL.RequestURI()
	req, err := http.NewRequest(r.Method, target, r.Body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	for k, v := range r.Header {
		req.Header[k] = v
	}
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 20,
			IdleConnTimeout:     60 * time.Second,
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "proxy error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	io.CopyBuffer(w, resp.Body, make([]byte, 64*1024))
}

func isLinkAllowed(l *Link) bool {
	if l == nil || !l.Active {
		return false
	}
	if l.ExpiresAt != "" {
		exp, err := time.Parse(time.RFC3339, l.ExpiresAt)
		if err == nil && time.Now().After(exp) {
			return false
		}
	}
	if l.LimitBytes > 0 && l.UsedBytes >= l.LimitBytes {
		return false
	}
	return true
}

func uniqueIPsForUUID(uuid string) map[string]bool {
	ipSet := make(map[string]bool)
	connMu.RLock()
	for _, c := range connections {
		if c.UUID == uuid && c.IP != "" {
			ipSet[c.IP] = true
		}
	}
	connMu.RUnlock()
	return ipSet
}

func isIPAllowed(l *Link, uuid, ip string) bool {
	if l == nil {
		return false
	}
	if l.IPLimit <= 0 {
		return true
	}
	ips := uniqueIPsForUUID(uuid)
	if ips[ip] {
		return true
	}
	return len(ips) < l.IPLimit
}

func clientIP(r *http.Request) string {
	fwd := r.Header.Get("X-Forwarded-For")
	if fwd != "" {
		parts := strings.Split(fwd, ",")
		return strings.TrimSpace(parts[0])
	}
	realIP := r.Header.Get("X-Real-IP")
	if realIP != "" {
		return strings.TrimSpace(realIP)
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func loadState() {
	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "/data"
	}
	stateFile := dataDir + "/cyberrage_state.json"
	data, err := os.ReadFile(stateFile)
	if err != nil {
		log.Printf("No existing state found")
		return
	}
	var state struct {
		Links map[string]*Link `json:"links"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		log.Printf("Error loading state: %v", err)
		return
	}
	linksMu.Lock()
	for k, v := range state.Links {
		links[k] = v
	}
	linksMu.Unlock()
	log.Printf("Loaded %d links", len(links))
}

func reloadStateLoop() {
	ticker := time.NewTicker(5 * time.Second)
	for range ticker.C {
		dataDir := os.Getenv("DATA_DIR")
		if dataDir == "" {
			dataDir = "/data"
		}
		stateFile := dataDir + "/cyberrage_state.json"
		data, err := os.ReadFile(stateFile)
		if err != nil {
			continue
		}
		var state struct {
			Links map[string]*Link `json:"links"`
		}
		if err := json.Unmarshal(data, &state); err != nil {
			continue
		}
		linksMu.Lock()
		for k, v := range state.Links {
			links[k] = v
		}
		linksMu.Unlock()
	}
}

func fmtBytes(b int64) string {
	if b < 1024 {
		return strconv.FormatInt(b, 10) + " B"
	}
	if b < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(b)/1024)
	}
	if b < 1024*1024*1024 {
		return fmt.Sprintf("%.2f MB", float64(b)/(1024*1024))
	}
	return fmt.Sprintf("%.2f GB", float64(b)/(1024*1024*1024))
}
