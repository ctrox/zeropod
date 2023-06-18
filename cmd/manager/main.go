package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/ctrox/zeropod/manager"
)

var metricsAddr = flag.String("metrics-addr", ":8080", "address of the metrics server")

func main() {
	flag.Parse()

	http.HandleFunc("/metrics", manager.Handler)
	log.Printf("starting metrics server on %s", *metricsAddr)
	if err := http.ListenAndServe(*metricsAddr, nil); err != nil {
		log.Fatal(err)
	}
}
