package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	debug := flag.Bool("debug", false, "enable debug logging (requests, responses, parsed summary)")
	flag.Parse()

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	prometheus.MustRegister(NewExporter(cfg, *debug))

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<html><head><title>SysMonitor Exporter</title></head><body><a href="/metrics">/metrics</a></body></html>`))
	})

	log.Printf("listening on %s, monitoring %d target(s)", cfg.Listen, len(cfg.Targets))
	log.Fatal(http.ListenAndServe(cfg.Listen, mux))
}
