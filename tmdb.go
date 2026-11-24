package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// tmdbKey is optional. Scryer has no person data, so actor pages need TMDB.
// Either a v3 API key or a v4 read access token works.
var tmdbKey = os.Getenv("TMDB_API_KEY")

func tmdbGet(ctx context.Context, path string, out any) error {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.themoviedb.org/3"+path+sep+"language=en-US", nil)
	if err != nil {
		return err
	}
	if strings.HasPrefix(tmdbKey, "eyJ") { // v4 tokens are JWTs
		req.Header.Set("Authorization", "Bearer "+tmdbKey)
	} else {
		req.URL.RawQuery += "&api_key=" + url.QueryEscape(tmdbKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL may hold the key, keep it out of logs and responses
		}
		return fmt.Errorf("tmdb: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return json.NewDecoder(resp.Body).Decode(out)
	case http.StatusNotFound:
		return &httpError{http.StatusNotFound, "not found on TMDB"}
	}
	return fmt.Errorf("tmdb: HTTP %d", resp.StatusCode)
}

type tmdbCredit struct {
	ID           int    `json:"id"`
	MediaType    string `json:"media_type"`
	Title        string `json:"title"`
	Name         string `json:"name"`
	Character    string `json:"character"`
	Job          string `json:"job"`
	Overview     string `json:"overview"`
	PosterPath   string `json:"poster_path"`
	BackdropPath string `json:"backdrop_path"`
	ReleaseDate  string `json:"release_date"`
	FirstAirDate string `json:"first_air_date"`
	Adult        bool   `json:"adult"`
}

func creditResults(cs []tmdbCredit) []result {
	out := []result{}
	for _, c := range cs {
		if c.Adult || (c.MediaType != "movie" && c.MediaType != "tv") {
			continue
		}
		out = append(out, result{ID: c.ID, MediaType: c.MediaType, Title: c.Title, Name: c.Name,
			Character: c.Character, Job: c.Job, Overview: c.Overview,
			PosterPath: c.PosterPath, BackdropPath: c.BackdropPath,
			ReleaseDate: c.ReleaseDate, FirstAirDate: c.FirstAirDate})
	}
	return out
}

func personCredits(ctx context.Context, r *http.Request, token string) (any, error) {
	id, err := pathInt(r, "id")
	if err != nil {
		return nil, err
	}
	empty := map[string]any{"id": id, "cast": []result{}, "crew": []result{}}
	if tmdbKey == "" {
		return empty, nil
	}
	var d struct{ Cast, Crew []tmdbCredit }
	if err := tmdbGet(ctx, "/person/"+strconv.Itoa(id)+"/combined_credits", &d); err != nil {
		return nil, err
	}
	cast, crew := creditResults(d.Cast), creditResults(d.Crew)
	all, err := fillMediaInfo(ctx, token, append(cast, crew...)) // one Scryer lookup for both
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "cast": all[:len(cast)], "crew": all[len(cast):]}, nil
}

var (
	tmdbSizeRe = regexp.MustCompile(`^(original|[wh][0-9]+|w[0-9]+_and_h[0-9]+_multi_faces)$`)
	tmdbFileRe = regexp.MustCompile(`^[A-Za-z0-9]+\.(jpg|png|svg)$`)
)

// tmdbImage redirects TMDB image paths (from person credits) to TMDB's image CDN.
func tmdbImage(w http.ResponseWriter, r *http.Request) {
	size, file := r.PathValue("size"), r.PathValue("file")
	if !tmdbSizeRe.MatchString(size) || !tmdbFileRe.MatchString(file) {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "https://image.tmdb.org/t/p/"+size+"/"+file, http.StatusFound)
}

// tmdbBasics is a title's TMDB overview and backdrop file. Scryer's discovery items lack the
// overview and often the backdrop; one request gets both, remembered for a week.
type tmdbBasics struct{ Overview, Backdrop string }

func tmdbTitle(ctx context.Context, kind string, id int) tmdbBasics {
	if tmdbKey == "" {
		return tmdbBasics{}
	}
	return cached("tmdb:"+kind+":"+strconv.Itoa(id), func() (tmdbBasics, bool) {
		tmdbSem <- struct{}{}
		defer func() { <-tmdbSem }()
		var d struct {
			Overview     string
			BackdropPath string `json:"backdrop_path"`
		}
		err := tmdbGet(ctx, "/"+kind+"/"+strconv.Itoa(id), &d)
		var he *httpError
		if errors.As(err, &he) && he.code == http.StatusNotFound {
			return tmdbBasics{}, true
		}
		if err != nil {
			return tmdbBasics{}, false
		}
		if !tmdbFileRe.MatchString(strings.TrimPrefix(d.BackdropPath, "/")) {
			d.BackdropPath = ""
		}
		return tmdbBasics{d.Overview, d.BackdropPath}, true
	})
}

// tmdbSem caps parallel TMDB lookups; a Discover page asks for a few rows of 20 at once.
// ponytail: fixed cap of 8, well under TMDB's ~40 requests/second.
var tmdbSem = make(chan struct{}, 8)

// tmdbBackdrop looks up a title's backdrop file on TMDB, or "" without a key or backdrop.
func tmdbBackdrop(ctx context.Context) func(kind string, id int) string {
	return func(kind string, id int) string { return tmdbTitle(ctx, kind, id).Backdrop }
}

// tmdbOverviews fills empty overviews, which Wholphin shows in the Discover header.
func tmdbOverviews(ctx context.Context, rs []result) {
	var wg sync.WaitGroup
	for i := range rs {
		if rs[i].Overview == "" && tmdbKey != "" {
			wg.Add(1)
			go func(r *result) {
				defer wg.Done()
				r.Overview = tmdbTitle(ctx, r.MediaType, r.ID).Overview
			}(&rs[i])
		}
	}
	wg.Wait()
}

type tmdbPerson struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Character   string `json:"character,omitempty"`
	Job         string `json:"job,omitempty"`
	Department  string `json:"department,omitempty"`
	ProfilePath string `json:"profile_path,omitempty"`
}

// seerrPerson is Seerr's CreditCast/CreditCrew; same fields as tmdbPerson, so it converts directly.
type seerrPerson struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Character   string `json:"character,omitempty"`
	Job         string `json:"job,omitempty"`
	Department  string `json:"department,omitempty"`
	ProfilePath string `json:"profilePath,omitempty"`
}

func seerrPeople(ps []tmdbPerson) []seerrPerson {
	out := make([]seerrPerson, len(ps))
	for i, p := range ps {
		out[i] = seerrPerson(p)
	}
	return out
}

// tmdbRelease is a country's entry in TMDB release_dates, which Seerr passes through as "releases".
type tmdbRelease struct {
	Country string `json:"iso_3166_1"`
	Dates   []struct {
		Certification string `json:"certification"`
	} `json:"release_dates"`
}

// tmdbExtras fetches the details-page fields Scryer lacks: cast and crew, trailers, age ratings
// (movies), tagline and episode runtime (TV). It runs alongside the Scryer query, and its
// result is merged into the response. nil without a key or on error, so the page still loads.
func tmdbExtras(ctx context.Context, kind string, id int) <-chan map[string]any {
	ch := make(chan map[string]any, 1)
	if tmdbKey == "" {
		ch <- nil
		return ch
	}
	go func() {
		var d struct {
			Tagline        string
			EpisodeRunTime []int `json:"episode_run_time"`
			LastEpisode    *struct {
				Runtime int
			} `json:"last_episode_to_air"`
			Credits struct{ Cast, Crew []tmdbPerson }
			Videos  struct {
				Results []struct{ Key, Name, Site, Type string }
			}
			ReleaseDates struct{ Results []tmdbRelease } `json:"release_dates"`
		}
		appendTo := "credits,videos"
		if kind == "movie" {
			appendTo += ",release_dates"
		}
		if err := tmdbGet(ctx, "/"+kind+"/"+strconv.Itoa(id)+"?append_to_response="+appendTo, &d); err != nil {
			log.Printf("tmdb %s %d: %v", kind, id, err)
			ch <- nil
			return
		}
		videos := []map[string]string{}
		for _, v := range d.Videos.Results {
			if v.Site == "YouTube" && v.Type == "Trailer" { // Wholphin only shows trailers
				videos = append(videos, map[string]string{"key": v.Key, "name": v.Name, "site": v.Site, "type": v.Type,
					"url": "https://www.youtube.com/watch?v=" + url.QueryEscape(v.Key)})
			}
		}
		out := map[string]any{
			"tagline":       d.Tagline,
			"credits":       map[string]any{"cast": seerrPeople(d.Credits.Cast), "crew": seerrPeople(d.Credits.Crew)},
			"relatedVideos": videos,
		}
		if kind == "movie" {
			// Wholphin shows the first certification of a country, so drop the dates without one
			// (premieres, digital releases) or the rating is usually missing.
			rels := []tmdbRelease{}
			for _, r := range d.ReleaseDates.Results {
				dates := r.Dates[:0]
				for _, x := range r.Dates {
					if x.Certification != "" {
						dates = append(dates, x)
					}
				}
				if len(dates) > 0 {
					r.Dates = dates
					rels = append(rels, r)
				}
			}
			out["releases"] = map[string]any{"results": rels}
		} else {
			rt := d.EpisodeRunTime
			if len(rt) == 0 && d.LastEpisode != nil && d.LastEpisode.Runtime > 0 { // TMDB leaves it empty for most newer shows
				rt = []int{d.LastEpisode.Runtime}
			}
			out["episodeRunTime"] = rt
		}
		ch <- out
	}()
	return ch
}

// seerrToTMDB maps Seerr's discover filters to TMDB's. Any of them set makes the grid come
// from TMDB, since Scryer's discovery can't filter by them.
var seerrToTMDB = map[string]string{
	"genre": "with_genres", "studio": "with_companies", "network": "with_networks",
	"keywords": "with_keywords", "excludeKeywords": "without_keywords",
	"primaryReleaseDateGte": "primary_release_date.gte", "primaryReleaseDateLte": "primary_release_date.lte",
	"firstAirDateGte": "first_air_date.gte", "firstAirDateLte": "first_air_date.lte",
	"withRuntimeGte": "with_runtime.gte", "withRuntimeLte": "with_runtime.lte",
	"voteAverageGte": "vote_average.gte", "voteAverageLte": "vote_average.lte",
	"voteCountGte": "vote_count.gte", "voteCountLte": "vote_count.lte",
	"watchProviders": "with_watch_providers", "status": "with_status",
	"certification": "certification", "certificationGte": "certification.gte", "certificationLte": "certification.lte",
}

// tmdbDiscover serves a filtered or re-sorted Discover grid from TMDB, with library status
// from Scryer. Unfiltered grids (and everything without a key) stay on Scryer's lists.
func tmdbDiscover(kind string, scryer apiFunc) apiFunc {
	return func(ctx context.Context, r *http.Request, token string) (any, error) {
		q, v := r.URL.Query(), url.Values{}
		for s, t := range seerrToTMDB {
			if x := q.Get(s); x != "" {
				v.Set(t, x)
			}
		}
		sort := q.Get("sortBy")
		if tmdbKey == "" || (len(v) == 0 && (sort == "" || sort == "popularity.desc")) {
			return scryer(ctx, r, token)
		}
		if v.Get("vote_average.gte") != "" && v.Get("vote_count.gte") == "" {
			v.Set("vote_count.gte", "50") // or titles with a single 10/10 vote pass every rating filter
		}
		if kind == "tv" {
			sort = strings.Replace(sort, "original_title", "original_name", 1) // Wholphin sends the movie key for TV
		}
		if sort != "" {
			v.Set("sort_by", sort)
			if strings.HasPrefix(sort, "vote_average") && v.Get("vote_count.gte") == "" {
				v.Set("vote_count.gte", "200") // ponytail: fixed floor, else the top is obscure 10/10s
			}
		}
		for s, t := range map[string]string{"watchRegion": "watch_region", "certificationCountry": "certification_country"} {
			if x := q.Get(s); x != "" {
				v.Set(t, x)
			}
		}
		pg := queryInt(r, "page", 1)
		v.Set("page", strconv.Itoa(pg))
		var d struct {
			TotalPages   int `json:"total_pages"`
			TotalResults int `json:"total_results"`
			Results      []tmdbCredit
		}
		if err := tmdbGet(ctx, "/discover/"+kind+"?"+v.Encode(), &d); err != nil {
			return nil, err
		}
		for i := range d.Results {
			d.Results[i].MediaType = kind
		}
		rs, err := fillMediaInfo(ctx, token, creditResults(d.Results))
		if err != nil {
			return nil, err
		}
		pages := min(d.TotalPages, 500) // TMDB serves no page past 500
		return page{Page: pg, TotalPages: pages, TotalResults: min(d.TotalResults, pages*pageSize), Results: rs}, nil
	}
}

type genreSlide struct {
	ID        int      `json:"id"`
	Name      string   `json:"name"`
	Backdrops []string `json:"backdrops"`
}

// genreSlider lists TMDB's genres, each with backdrops of its most popular titles, for the
// Discover genre rows and the genre filter. Remembered for a week.
func genreSlider(kind string) apiFunc {
	return func(ctx context.Context, r *http.Request, token string) (any, error) {
		if tmdbKey == "" {
			return []genreSlide{}, nil
		}
		gs := cached("tmdb:genres:"+kind, func() ([]genreSlide, bool) {
			var d struct {
				Genres []struct {
					ID   int
					Name string
				}
			}
			if err := tmdbGet(ctx, "/genre/"+kind+"/list", &d); err != nil {
				log.Printf("tmdb genres %s: %v", kind, err)
				return nil, false
			}
			out := make([]genreSlide, len(d.Genres))
			var wg sync.WaitGroup
			var failed atomic.Bool
			for i, g := range d.Genres {
				out[i] = genreSlide{g.ID, g.Name, []string{}}
				wg.Add(1)
				go func(s *genreSlide) {
					defer wg.Done()
					tmdbSem <- struct{}{}
					defer func() { <-tmdbSem }()
					var p struct {
						Results []struct {
							BackdropPath string `json:"backdrop_path"`
						}
					}
					if err := tmdbGet(ctx, "/discover/"+kind+"?sort_by=popularity.desc&with_genres="+strconv.Itoa(s.ID), &p); err != nil {
						failed.Store(true) // keep what loaded, but try again next time
						return
					}
					for _, x := range p.Results {
						if len(s.Backdrops) < 4 && tmdbFileRe.MatchString(strings.TrimPrefix(x.BackdropPath, "/")) {
							s.Backdrops = append(s.Backdrops, x.BackdropPath)
						}
					}
				}(&out[i])
			}
			wg.Wait()
			return out, !failed.Load()
		})
		if gs == nil {
			return nil, &httpError{http.StatusBadGateway, "TMDB genres unavailable"}
		}
		return gs, nil
	}
}

type watchProvider struct {
	ID              int    `json:"id"`
	Name            string `json:"name"`
	LogoPath        string `json:"logoPath,omitempty"`
	DisplayPriority int    `json:"displayPriority"`
}

var regionRe = regexp.MustCompile(`^[A-Z]{2}$`)

// watchProviders lists the streaming services of a region, for the Studios filter. Remembered for a week.
func watchProviders(ctx context.Context, r *http.Request, token string) (any, error) {
	kind, region := "movie", r.URL.Query().Get("watchRegion")
	if r.PathValue("kind") == "tv" {
		kind = "tv"
	}
	if !regionRe.MatchString(region) {
		region = "US"
	}
	if tmdbKey == "" {
		return []watchProvider{}, nil
	}
	ps := cached("tmdb:providers:"+kind+":"+region, func() ([]watchProvider, bool) {
		var d struct {
			Results []struct {
				ID         int            `json:"provider_id"`
				Name       string         `json:"provider_name"`
				LogoPath   string         `json:"logo_path"`
				Priorities map[string]int `json:"display_priorities"`
			}
		}
		if err := tmdbGet(ctx, "/watch/providers/"+kind+"?watch_region="+region, &d); err != nil {
			log.Printf("tmdb providers %s %s: %v", kind, region, err)
			return nil, false
		}
		out := make([]watchProvider, len(d.Results))
		for i, x := range d.Results {
			out[i] = watchProvider{x.ID, x.Name, x.LogoPath, x.Priorities[region]}
		}
		// Wholphin lists them in order, so the big services come first.
		slices.SortStableFunc(out, func(a, b watchProvider) int { return a.DisplayPriority - b.DisplayPriority })
		return out, true
	})
	if ps == nil {
		return nil, &httpError{http.StatusBadGateway, "TMDB watch providers unavailable"}
	}
	return ps, nil
}
