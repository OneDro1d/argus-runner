package main

import (
	"net/http"
)

func main() {
	mux := http.NewServeMux()
	http.HandleFunc("/health", healthHandler)
	mux.HandleFunc("GET /items/{id}", itemHandler)
	mux.Handle("/orders", ordersHandler).Methods("POST")

	r := newChiRouter()
	r.Get("/widgets", widgetsHandler)

	g := newGinRouter()
	g.GET("/status", statusHandler)
}
