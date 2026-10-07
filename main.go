package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

func main() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL nao definida")
	}

	// Roda as migrações
	sqlDB, err := sql.Open("pgx", dbURL)
	if err != nil {
		log.Fatal(err)
	}
	if err := goose.SetDialect("postgres"); err != nil {
		log.Fatal(err)
	}
	if err := goose.Up(sqlDB, "migrations"); err != nil {
		log.Fatal(err)
	}
	sqlDB.Close()

	// Pool de conexões
	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	if len(os.Args) == 3 && os.Args[1] == "createuser" {
		if err := createUserCmd(context.Background(), pool, os.Args[2]); err != nil {
			log.Fatal(err)
		}
		fmt.Println("usuario criado")
		return
	}

	auth := newAuth(pool)
	auth.register()

	// Twilio real: só liga se as chaves existirem no .env
	if os.Getenv("TWILIO_AUTH_TOKEN") != "" {
		app := newApp(pool)
		app.register()
		log.Println("twilio ligada")
	}

	// Simulador de demo: só liga com SIMULATOR=1
	if os.Getenv("SIMULATOR") == "1" {
		sim, err := newSim(context.Background(), pool)
		if err != nil {
			log.Fatal(err)
		}
		sim.register(auth.require)
		log.Println("simulador em http://localhost:8080/sim")
	}

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if err := pool.Ping(r.Context()); err != nil {
			http.Error(w, "db indisponivel", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})

	log.Println("servidor rodando em http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
