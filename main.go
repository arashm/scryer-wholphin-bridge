// scryer-wholphin-bridge serves the slice of the Seerr API that Wholphin uses, backed by Scryer.
package main

import (
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

var (
	scryerURL       = strings.TrimRight(os.Getenv("SCRYER_URL"), "/")
	scryerPublicURL = strings.TrimRight(os.Getenv("SCRYER_PUBLIC_URL"), "/")
	dryRun          = os.Getenv("BRIDGE_DRY_RUN") == "1"
)

func main() {
	if scryerURL == "" {
		log.Fatal("SCRYER_URL is required")
	}
	if scryerPublicURL == "" {
		scryerPublicURL = scryerURL
	}
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":5055"
	}
	if dryRun {
		log.Print("BRIDGE_DRY_RUN=1: POST /request only previews, nothing is submitted")
	}
	if tmdbKey == "" {
		log.Print("TMDB_API_KEY not set: actor pages stay empty")
	}
	log.Printf("listening on %s, scryer %s", addr, scryerURL)
	log.Fatal(http.ListenAndServe(addr, logged(routes())))
}

func routes() *http.ServeMux {
	m := http.NewServeMux()
	const v1 = "/api/v1"
	m.Handle("GET "+v1+"/auth/me", api(authMe))
	m.HandleFunc("GET "+v1+"/settings/public", settingsPublic)
	m.Handle("GET "+v1+"/search", api(search))

	m.Handle("GET "+v1+"/discover/trending", api(discover("", `sectionId:"trending_now"`)))
	m.Handle("GET "+v1+"/discover/movies", api(discover("", `sectionId:"popular_movies"`)))
	m.Handle("GET "+v1+"/discover/tv", api(discover("", `sectionId:"popular_series"`)))
	// Scryer's upcoming sections are tiny and there's none for movies, so add the titles it tags as unreleased.
	const unreleased = `statusTags:["Announced","Upcoming"], includeUnresolved:true, sort:RELEASE_DATE`
	m.Handle("GET "+v1+"/discover/tv/upcoming", api(discover("tv", `sectionId:"upcoming_series"`, `sectionId:"upcoming_next_season"`, unreleased)))
	m.Handle("GET "+v1+"/discover/movies/upcoming", api(discover("movie", unreleased)))
	m.HandleFunc("GET "+v1+"/discover/genreslider/{kind}", emptyList)
	m.HandleFunc("GET "+v1+"/watchproviders/{kind}", emptyList)
	m.Handle("GET "+v1+"/person/{id}/combined_credits", api(personCredits))

	m.Handle("GET "+v1+"/movie/{id}", api(movieDetails))
	m.Handle("GET "+v1+"/tv/{id}", api(tvDetails))
	for _, k := range []string{"movie", "tv"} {
		m.Handle("GET "+v1+"/"+k+"/{id}/ratings", api(ratings(k)))
		m.Handle("GET "+v1+"/"+k+"/{id}/similar", api(similar(k)))
		m.Handle("GET "+v1+"/"+k+"/{id}/recommendations", api(similar(k)))
	}

	m.Handle("GET "+v1+"/request", api(listRequests))
	m.Handle("POST "+v1+"/request", api(createRequest))
	m.Handle("DELETE "+v1+"/request/{id}", api(deleteRequest))

	m.HandleFunc("GET /imageproxy/tmdb/t/p/{size}/images/media/{hash}", imageProxy)
	m.HandleFunc("GET /imageproxy/tmdb/t/p/{size}/{file}", tmdbImage)
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("unhandled: %s %s", r.Method, r.URL.Path)
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "not implemented by scryer-wholphin-bridge"})
	})
	return m
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int) { w.code = c; w.ResponseWriter.WriteHeader(c) }

// logged prints one line per request. The API key is never logged.
func logged(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, sw := time.Now(), &statusWriter{w, 200}
		h.ServeHTTP(sw, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.RequestURI(), sw.code, time.Since(start).Round(time.Millisecond))
	})
}
