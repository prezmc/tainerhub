// Package debugsrv serves Go's profiler when TAINERHUB_PPROF_ADDR is set
// (e.g. 127.0.0.1:6060). Off by default; for diagnosing CPU or memory use.
package debugsrv

import (
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"time"
)

func Start(log *slog.Logger) {
	addr := os.Getenv("TAINERHUB_PPROF_ADDR")
	if addr == "" {
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Warn("profiler listening; don't expose this address", "addr", addr)
	go func() { _ = srv.ListenAndServe() }()
}
