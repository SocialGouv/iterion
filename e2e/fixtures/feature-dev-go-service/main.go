// Package main is a small in-memory user service used as a realistic
// fixture for iterion's feature_dev bot live test. It is NOT
// production code — the data store is in-memory and resets on every
// process start.
package main

import (
	"log"
	"net/http"
	"os"

	"example.com/userservice/handlers"
	"example.com/userservice/internal/store"
	"example.com/userservice/middleware"
)

func main() {
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}

	db := store.New()
	// Seed two users so the GET endpoints have something to return.
	_ = db.CreateUser(store.User{ID: "u1", Name: "Alice", Email: "alice@example.com"})
	_ = db.CreateUser(store.User{ID: "u2", Name: "Bob", Email: "bob@example.com"})

	mux := http.NewServeMux()
	mux.Handle("/users", handlers.NewUsersList(db))
	mux.Handle("/users/", handlers.NewUserByID(db))

	handler := middleware.RequestLogger(log.New(os.Stdout, "", log.LstdFlags))(mux)

	log.Printf("listening on %s", addr)
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatalf("server: %v", err)
	}
}
