// Command ordersapi serves the order database as GraphQL at /graphql.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"

	"github.com/davmaultsby/support-triage-agent/orders"
)

func main() {
	addr := flag.String("addr", "localhost:8080", "listen address")
	flag.Parse()

	db, err := orders.Open(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	http.Handle("/graphql", orders.Handler(db))
	log.Printf("GraphQL on http://%s/graphql", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
