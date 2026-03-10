package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

//go:embed public
var publicFS embed.FS

var (
	sharedDir string
	version   = "1.0.0"
	hub       = newHub()
	upgrader  = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
)

// Hub manages WebSocket client connections.
type Hub struct {
	mu      sync.Mutex
	clients map[string]*websocket.Conn
}

func newHub() *Hub {
	return &Hub{clients: make(map[string]*websocket.Conn)}
}

func (h *Hub) register(id string, conn *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[id] = conn
}

func (h *Hub) unregister(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients, id)
}

func (h *Hub) send(id string, msg any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	conn, ok := h.clients[id]
	if !ok {
		return
	}
	if err := conn.WriteJSON(msg); err != nil {
		delete(h.clients, id)
	}
}

// ProgressMsg is the WebSocket message format.
type ProgressMsg struct {
	Type     string   `json:"type"`
	ClientID string   `json:"clientId,omitempty"`
	Filename string   `json:"filename,omitempty"`
	Files    []string `json:"files,omitempty"`
	Percent  float64  `json:"percent,omitempty"`
	Error    string   `json:"error,omitempty"`
}

// progressReader wraps io.ReadCloser and sends upload_progress messages
// based on total request body bytes consumed.
type progressReader struct {
	io.ReadCloser
	total    int64
	read     int64
	clientID string
	filename string
	lastPct  float64
}

func (pr *progressReader) Read(p []byte) (n int, err error) {
	n, err = pr.ReadCloser.Read(p)
	pr.read += int64(n)
	if pr.total > 0 {
		pct := float64(pr.read) / float64(pr.total) * 100
		if pct > 100 {
			pct = 100
		}
		if pct-pr.lastPct >= 1 || (err == io.EOF && pr.lastPct < 100) {
			pr.lastPct = pct
			hub.send(pr.clientID, ProgressMsg{
				Type:     "upload_progress",
				Filename: pr.filename,
				Percent:  pct,
			})
		}
	}
	return
}

// countingWriter wraps http.ResponseWriter and sends download_progress
// messages as bytes are written to the client.
type countingWriter struct {
	http.ResponseWriter
	total    int64
	written  int64
	clientID string
	filename string
	lastPct  float64
}

func (cw *countingWriter) Write(p []byte) (n int, err error) {
	n, err = cw.ResponseWriter.Write(p)
	cw.written += int64(n)
	if cw.total > 0 {
		pct := float64(cw.written) / float64(cw.total) * 100
		if pct > 100 {
			pct = 100
		}
		if pct-cw.lastPct >= 1 || pct >= 100 {
			cw.lastPct = pct
			hub.send(cw.clientID, ProgressMsg{
				Type:     "download_progress",
				Filename: cw.filename,
				Percent:  pct,
			})
		}
	}
	return
}

// recovery is a middleware that catches panics and returns 500.
func recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				log.Printf("panic recovered: %v\n%s", err, debug.Stack())
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// FileInfo is returned by the /api/files endpoint.
type FileInfo struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

func generateID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func getLocalIPs() []string {
	var ips []string
	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip != nil && !ip.IsLoopback() && ip.To4() != nil {
				ips = append(ips, ip.String())
			}
		}
	}
	return ips
}

// safeFilePath returns the cleaned absolute path and verifies it stays within sharedDir.
func safeFilePath(filename string) (string, bool) {
	filename = filepath.Base(filename)
	if filename == "" || filename == "." || filename == string(filepath.Separator) {
		return "", false
	}
	full := filepath.Join(sharedDir, filename)
	base := filepath.Clean(sharedDir) + string(filepath.Separator)
	if !strings.HasPrefix(filepath.Clean(full)+string(filepath.Separator), base) {
		return "", false
	}
	return full, true
}

// handleWebSocket upgrades the connection and registers the client with the hub.
func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ws upgrade: %v", err)
		return
	}
	id := generateID()
	hub.register(id, conn)
	defer func() {
		hub.unregister(id)
		conn.Close()
	}()
	conn.WriteJSON(ProgressMsg{Type: "connected", ClientID: id})
	// Read loop — keeps the connection alive and detects client disconnect.
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}
}

// handleFiles handles GET /api/files (list) and DELETE /api/files/{name} (delete).
func handleFiles(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		entries, err := os.ReadDir(sharedDir)
		if err != nil {
			http.Error(w, "Cannot read directory", http.StatusInternalServerError)
			return
		}
		files := make([]FileInfo, 0, len(entries))
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			files = append(files, FileInfo{
				Name:     e.Name(),
				Size:     info.Size(),
				Modified: info.ModTime(),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(files)

	case http.MethodDelete:
		name := strings.TrimPrefix(r.URL.Path, "/api/files/")
		path, ok := safeFilePath(name)
		if !ok {
			http.Error(w, "Invalid filename", http.StatusBadRequest)
			return
		}
		if err := os.Remove(path); err != nil {
			http.Error(w, "Delete failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// handleUpload streams a multipart upload directly to sharedDir.
func handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	clientID := r.URL.Query().Get("clientId")

	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "Expected multipart/form-data: "+err.Error(), http.StatusBadRequest)
		return
	}

	var uploaded []string
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, "Multipart read error: "+err.Error(), http.StatusBadRequest)
			return
		}
		fname := part.FileName()
		if part.FormName() != "file" || fname == "" {
			part.Close()
			continue
		}
		path, ok := safeFilePath(fname)
		if !ok {
			part.Close()
			continue
		}
		tmpPath := path + ".tmp"
		dst, err := os.Create(tmpPath)
		if err != nil {
			part.Close()
			http.Error(w, "Cannot create file", http.StatusInternalServerError)
			return
		}

		pr := &progressReader{
			ReadCloser: part,
			total:      r.ContentLength,
			clientID:   clientID,
			filename:   filepath.Base(fname),
		}
		if _, err := io.Copy(dst, pr); err != nil {
			dst.Close()
			part.Close()
			os.Remove(tmpPath)
			hub.send(clientID, ProgressMsg{Type: "upload_error", Filename: filepath.Base(fname), Error: err.Error()})
			http.Error(w, "Write failed", http.StatusInternalServerError)
			return
		}
		dst.Close()
		part.Close()
		if err := os.Rename(tmpPath, path); err != nil {
			os.Remove(tmpPath)
			http.Error(w, "Finalize failed", http.StatusInternalServerError)
			return
		}
		base := filepath.Base(fname)
		uploaded = append(uploaded, base)
		hub.send(clientID, ProgressMsg{Type: "upload_done", Filename: base, Percent: 100})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"uploaded": uploaded})
}

// handleDownload serves a file from sharedDir with progress tracking.
func handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	clientID := r.URL.Query().Get("clientId")
	name := strings.TrimPrefix(r.URL.Path, "/api/download/")
	path, ok := safeFilePath(name)
	if !ok {
		http.Error(w, "Invalid filename", http.StatusBadRequest)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "Stat error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filepath.Base(name)))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size()))
	cw := &countingWriter{
		ResponseWriter: w,
		total:          info.Size(),
		clientID:       clientID,
		filename:       filepath.Base(name),
	}
	io.Copy(cw, f)
	hub.send(clientID, ProgressMsg{Type: "download_done", Filename: filepath.Base(name), Percent: 100})
}

func main() {
	dirFlag := flag.String("d", "", "shared directory (default: $HOME/Downloads/shared)")
	portFlag := flag.Int("p", 8080, "port to listen on")
	versionFlag := flag.Bool("v", false, "print version and exit")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("goshare server v%s\n", version)
		return
	}

	if *dirFlag != "" {
		sharedDir = *dirFlag
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			log.Fatal("Cannot determine home directory:", err)
		}
		sharedDir = filepath.Join(home, "Downloads", "shared")
	}

	if err := os.MkdirAll(sharedDir, 0755); err != nil {
		log.Fatalf("Cannot create shared directory %s: %v", sharedDir, err)
	}

	subFS, err := fs.Sub(publicFS, "public")
	if err != nil {
		log.Fatal("embed FS error:", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(subFS)))
	mux.HandleFunc("/ws", handleWebSocket)
	mux.HandleFunc("/api/files", handleFiles)
	mux.HandleFunc("/api/files/", handleFiles)
	mux.HandleFunc("/api/upload", handleUpload)
	mux.HandleFunc("/api/download/", handleDownload)

	server := &http.Server{
		Addr:    fmt.Sprintf("0.0.0.0:%d", *portFlag),
		Handler: recovery(mux),
	}

	fmt.Println("GoShare starting...")
	fmt.Printf("Shared directory: %s\n", sharedDir)
	fmt.Printf("Listening on http://0.0.0.0:%d\n", *portFlag)
	for _, ip := range getLocalIPs() {
		fmt.Printf("  → http://%s:%d\n", ip, *portFlag)
	}

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal("ListenAndServe:", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh

	fmt.Println("\nShutting down GoShare...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server.Shutdown(ctx)
	fmt.Println("Goodbye!")
}
