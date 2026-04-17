package typesserver

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"

	"activity-reward-extension/internal/config"
	"activity-reward-extension/pkg/decoder"
)

// maxDecodeBodyBytes caps the /decode request body. The payload is a small JSON
// envelope with a hex blob; without a cap an unauthenticated caller could stream an
// arbitrarily large body straight into json.Decode and exhaust memory.
const maxDecodeBodyBytes = 1 << 20 // 1 MiB

type decodeRequest struct {
	OPType    string `json:"opType"`
	OPCommand string `json:"opCommand"`
	Kind      string `json:"kind"`
	Data      string `json:"data"`
}

type decodeResponse struct {
	Decoded any `json:"decoded"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// Server holds the HTTP handlers and decoder registry.
type Server struct {
	registry *decoder.Registry
	mux      *http.ServeMux
}

// New creates a Server wired to the given registry.
func New(registry *decoder.Registry) *Server {
	s := &Server{registry: registry, mux: http.NewServeMux()}
	s.mux.HandleFunc("POST /decode", s.handleDecode)
	s.mux.HandleFunc("GET /registry", s.handleRegistry)
	s.mux.HandleFunc("GET /health", s.handleHealth)
	return s
}

// ListenAndServe starts the HTTP server on the configured host and given port.
// The timeouts bound slow-client attacks (slowloris and idle connections) that a
// bare http.ListenAndServe — which sets none — leaves open on this public,
// unauthenticated endpoint.
func (s *Server) ListenAndServe(port int) error {
	addr := net.JoinHostPort(config.TypesServerHost, strconv.Itoa(port))
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 16, // 64 KiB
	}
	fmt.Printf("types-server %s listening on %s\n", config.Version, addr)
	return srv.ListenAndServe()
}

func (s *Server) handleDecode(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxDecodeBodyBytes)

	var req decodeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body: " + err.Error()})
		return
	}

	kind := decoder.DataKind(req.Kind)
	if kind != decoder.KindMessage && kind != decoder.KindResult {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "kind must be \"message\" or \"result\""})
		return
	}

	data, err := hexutil.Decode(req.Data)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid hex data: " + err.Error()})
		return
	}

	dec, err := s.registry.Lookup(req.OPType, req.OPCommand, kind)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: err.Error()})
		return
	}

	decoded, err := dec.Decode(data)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, errorResponse{Error: "decode failed: " + err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, decodeResponse{Decoded: decoded})
}

func (s *Server) handleRegistry(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.registry.Keys())
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": config.Version})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}
