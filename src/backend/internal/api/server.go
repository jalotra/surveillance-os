package api

import (
	"encoding/json"
	"log"
	"net/http"
	"runtime"
	"time"
)

// SystemStatus represents the health and metrics of the Raspberry Pi.
type SystemStatus struct {
	Status    string    `json:"status"`
	Uptime    string    `json:"uptime"`
	GoVersion string    `json:"go_version"`
	NumCPU    int       `json:"num_cpu"`
	Goroutines int      `json:"goroutines"`
	MemoryMB  uint64    `json:"memory_mb"`
	Timestamp time.Time `json:"timestamp"`
}

type Server struct {
	startTime time.Time
}

func NewServer() *Server {
	return &Server{
		startTime: time.Now(),
	}
}

func (s *Server) Start(addr string) error {
	mux := http.NewServeMux()
	
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/status", s.handleStatus)
	
	// CORS for local development
	handler := corsMiddleware(mux)

	log.Printf("Starting HTTP server on %s", addr)
	return http.ListenAndServe(addr, handler)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	status := SystemStatus{
		Status:    "online",
		Uptime:    time.Since(s.startTime).String(),
		GoVersion: runtime.Version(),
		NumCPU:    runtime.NumCPU(),
		Goroutines: runtime.NumGoroutine(),
		MemoryMB:  m.Alloc / 1024 / 1024,
		Timestamp: time.Now(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		
		next.ServeHTTP(w, r)
	})
}
