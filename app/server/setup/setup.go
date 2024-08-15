package setup

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"plandex-server/db"
	"plandex-server/host"
	"plandex-server/model/plan"
	"plandex-server/routes"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"github.com/rs/cors"
)

func MustLoadIp() {
	err := host.LoadIp()
	if err != nil {
		log.Fatal("Error loading IP: ", err)
	}
}

func MustInitDb() {
	err := db.Connect()
	if err != nil {
		log.Fatal("Error initializing database: ", err)
	}

	err = db.MigrationsUp()
	if err != nil {
		log.Fatal("Error running migrations: ", err)
	}

}

func StartServer(r *mux.Router) {
	if os.Getenv("GOENV") == "development" {
		log.Println("In development mode.")
	}

	// Get externalPort from the environment variable or default to 8080
	externalPort := os.Getenv("PORT")
	if externalPort == "" {
		externalPort = "8080"
	}

	routes.AddRoutes(r)

	// Enable CORS based on environment
	var corsHandler http.Handler
	if os.Getenv("GOENV") == "development" {
		corsHandler = cors.New(cors.Options{
			AllowedOrigins:   []string{"*"},
			AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE"},
			AllowedHeaders:   []string{"Content-Type", "Authorization"},
			AllowCredentials: true,
		}).Handler(r)
	} else {
		corsHandler = cors.New(cors.Options{
			AllowedOrigins:   []string{"http://app.plandex.ai", "http://localhost:55000"},
			AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE"},
			AllowedHeaders:   []string{"Content-Type", "Authorization"},
			AllowCredentials: true,
		}).Handler(r)
	}

	go startServer(externalPort, corsHandler)
	log.Println("Started server on port " + externalPort)

	sigTermChan := make(chan os.Signal, 1)
	signal.Notify(sigTermChan, syscall.SIGTERM)

	go func() {
		<-sigTermChan

		for {
			l := plan.NumActivePlans()
			if l == 0 {
				break
			}
			log.Printf("Waiting for %d active plans to finish...\n", l)
			time.Sleep(1 * time.Second)
		}

		os.Exit(0)
	}()

	select {}
}

func startServer(port string, handler http.Handler) {
	err := http.ListenAndServe(fmt.Sprintf(":%s", port), handler)
	if err != nil {
		log.Fatalf("Failed to start server on port %s: %v", port, err)
	}
}
